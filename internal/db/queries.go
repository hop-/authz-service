package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"authz-service/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Query constants
const (
	// Steps 2 & 3: override lookup
	checkOverrideQuery = `SELECT effect, expires_at FROM user_permission_overrides
WHERE user_id       = $1
  AND permission_id = $2
  AND resource_id   = $3
  AND (expires_at IS NULL OR expires_at > NOW())`

	// Step 4: role expansion. The longest-lived grant decides (a grant with no
	// expiry first), with the role name as a deterministic tie-break.
	checkRoleQuery = `SELECT r.name, ur.expires_at
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN roles r             ON r.id       = ur.role_id
WHERE ur.user_id       = $1
  AND rp.permission_id = $2
  AND ur.resource_type = $3
  AND ur.resource_id   = $4
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
ORDER BY ur.expires_at DESC NULLS FIRST, r.name
LIMIT 1`

	// GET /query/v1/resources — no scope filter
	resourcesQueryNoScope = `SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'ALLOW'
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())
UNION
SELECT ur.resource_id
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
WHERE ur.user_id = $1 AND rp.permission_id = $2 AND ur.resource_type = $3
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
EXCEPT
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'DENY'
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())`

	// GET /query/v1/resources — with scope filter
	resourcesQueryWithScope = `SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'ALLOW'
  AND upo.scope @> $4
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())
UNION
SELECT ur.resource_id
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
WHERE ur.user_id = $1 AND rp.permission_id = $2 AND ur.resource_type = $3
  AND ur.scope @> $4
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
EXCEPT
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'DENY'
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())`

	// POST /query/v1/resources/filter: the resourcesQueryNoScope set,
	// restricted to the candidate ids ($4) in every branch.
	filterResourcesQuery = `SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'ALLOW'
  AND upo.resource_id = ANY($4::text[])
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())
UNION
SELECT ur.resource_id
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
WHERE ur.user_id = $1 AND rp.permission_id = $2 AND ur.resource_type = $3
  AND ur.resource_id = ANY($4::text[])
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
EXCEPT
SELECT upo.resource_id
FROM user_permission_overrides upo
WHERE upo.user_id = $1 AND upo.permission_id = $2 AND upo.effect = 'DENY'
  AND upo.resource_id = ANY($4::text[])
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())`

	// GET /query/v1/permissions: raw role-grant rows. A role grants a permission
	// only on assignments of the permission's own resource type.
	listUserPermissionsQuery = `SELECT p.action, p.resource_type, ur.resource_id, ur.scope, r.name AS source
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
JOIN roles r ON r.id = ur.role_id
WHERE ur.user_id = $1
  AND p.resource_type = ur.resource_type
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())`

	listUserPermissionsWithResourceQuery = `SELECT p.action, p.resource_type, ur.resource_id, ur.scope, r.name AS source
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
JOIN roles r ON r.id = ur.role_id
WHERE ur.user_id = $1
  AND p.resource_type = ur.resource_type
  AND ur.resource_type = $2
  AND ur.resource_id = $3
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())`

	listUserPermissionsWithTypeQuery = `SELECT p.action, p.resource_type, ur.resource_id, ur.scope, r.name AS source
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
JOIN roles r ON r.id = ur.role_id
WHERE ur.user_id = $1
  AND p.resource_type = ur.resource_type
  AND ur.resource_type = $2
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())`

	// GET /query/v1/permissions: raw override rows, both effects (merged in resolver.List).
	listUserOverridesQuery = `SELECT p.action, p.resource_type, upo.resource_id, upo.scope, upo.effect
FROM user_permission_overrides upo
JOIN permissions p ON p.id = upo.permission_id
WHERE upo.user_id = $1
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())`

	listUserOverridesWithResourceQuery = `SELECT p.action, p.resource_type, upo.resource_id, upo.scope, upo.effect
FROM user_permission_overrides upo
JOIN permissions p ON p.id = upo.permission_id
WHERE upo.user_id = $1
  AND p.resource_type = $2
  AND upo.resource_id = $3
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())`

	listUserOverridesWithTypeQuery = `SELECT p.action, p.resource_type, upo.resource_id, upo.scope, upo.effect
FROM user_permission_overrides upo
JOIN permissions p ON p.id = upo.permission_id
WHERE upo.user_id = $1
  AND p.resource_type = $2
  AND (upo.expires_at IS NULL OR upo.expires_at > NOW())`

	// Registry loader queries
	loadPermissionsQuery = `SELECT id, action, resource_type FROM permissions`
	loadRolesQuery       = `SELECT id, name FROM roles`

	// Admin read queries
	listAllPermissionsQuery  = `SELECT action, resource_type, description, created_at FROM permissions ORDER BY resource_type, action`
	listAllRolesQuery        = `SELECT name, description, created_at FROM roles ORDER BY name`
	listRolePermissionsQuery = `SELECT p.action, p.resource_type FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE rp.role_id = $1 ORDER BY p.resource_type, p.action`

	// Worker write queries
	insertEventLogQuery = `INSERT INTO event_log (event_id, event_type) VALUES ($1, $2) ON CONFLICT DO NOTHING`

	insertUserRoleQuery = `INSERT INTO user_roles (user_id, role_id, resource_type, resource_id, scope, granted_by, granted_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT DO NOTHING`

	deleteUserRoleQuery = `DELETE FROM user_roles WHERE user_id = $1 AND role_id = $2 AND resource_type = $3 AND resource_id = $4`

	upsertOverrideQuery = `INSERT INTO user_permission_overrides (user_id, permission_id, resource_id, scope, effect, reason, set_by, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (user_id, permission_id, resource_id) DO UPDATE
SET effect = EXCLUDED.effect, reason = EXCLUDED.reason, set_by = EXCLUDED.set_by, expires_at = EXCLUDED.expires_at`

	deleteOverrideQuery = `DELETE FROM user_permission_overrides WHERE user_id = $1 AND permission_id = $2 AND resource_id = $3`

	// Idempotent insert-or-select: returns the existing id for an existing natural key.
	insertPermissionQuery = `WITH ins AS (
  INSERT INTO permissions (action, resource_type, description) VALUES ($1, $2, $3)
  ON CONFLICT (action, resource_type) DO NOTHING RETURNING id)
SELECT id FROM ins
UNION ALL
SELECT id FROM permissions WHERE action = $1 AND resource_type = $2
LIMIT 1`

	deletePermissionQuery = `DELETE FROM permissions WHERE action = $1 AND resource_type = $2`

	// Idempotent insert-or-select: returns the existing id for an existing name.
	insertRoleQuery = `WITH ins AS (
  INSERT INTO roles (name, description) VALUES ($1, $2)
  ON CONFLICT (name) DO NOTHING RETURNING id)
SELECT id FROM ins
UNION ALL
SELECT id FROM roles WHERE name = $1
LIMIT 1`

	deleteRoleQuery = `DELETE FROM roles WHERE name = $1`

	insertRolePermissionQuery = `INSERT INTO role_permissions (role_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`

	deleteRolePermissionQuery = `DELETE FROM role_permissions WHERE role_id = $1 AND permission_id = $2`

	// event_log retention cleanup (worker). One batch: at most $2 rows
	// processed more than $1 ago.
	tryAdvisoryLockQuery     = `SELECT pg_try_advisory_lock($1)`
	advisoryUnlockQuery      = `SELECT pg_advisory_unlock($1)`
	deleteExpiredEventsQuery = `DELETE FROM event_log WHERE ctid IN (
  SELECT ctid FROM event_log WHERE processed_at < NOW() - $1::interval LIMIT $2)`

	// Worker id lookups (primary DB), used by duplicate-path follow-ups
	permissionIDQuery = `SELECT id FROM permissions WHERE action = $1 AND resource_type = $2`
	roleIDQuery       = `SELECT id FROM roles WHERE name = $1`
)

// Queries wraps a pgxpool.Pool and provides typed query methods.
type Queries struct {
	pool *pgxpool.Pool
	// acquireTimeout bounds each connection acquisition (0: no bound).
	acquireTimeout time.Duration
}

// NewQueries returns Queries on pool. acquireTimeout bounds how long each
// method waits for a pool connection (DB_ACQUIRE_TIMEOUT_SECS); 0 means it
// waits as long as its ctx allows. pgxpool has no acquire-timeout option of
// its own.
func NewQueries(pool *pgxpool.Pool, acquireTimeout time.Duration) *Queries {
	return &Queries{pool: pool, acquireTimeout: acquireTimeout}
}

// acquire gets a pool connection, waiting at most acquireTimeout. The
// timeout covers the acquisition only: the caller runs its query on its own
// ctx and must Release the connection after closing any rows.
func (q *Queries) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	actx := ctx
	if q.acquireTimeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, q.acquireTimeout)
		defer cancel()
	}
	conn, err := q.pool.Acquire(actx)
	if err != nil {
		return nil, fmt.Errorf("acquire db connection: %w", err)
	}
	return conn, nil
}

// queryRow runs a single-row query on an acquired connection and scans it
// into dest.
func (q *Queries) queryRow(ctx context.Context, sql string, dest ...any) error {
	conn, err := q.acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	return conn.QueryRow(ctx, sql).Scan(dest...)
}

func (q *Queries) Pool() *pgxpool.Pool {
	return q.pool
}

// CheckOverride returns the active override effect for a user+permission+resource,
// if any, and its expires_at (nil when it never expires). DENY wins over ALLOW.
func (q *Queries) CheckOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) (*model.Effect, *time.Time, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, checkOverrideQuery, userID, permissionID, resourceID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var denyFound, allowFound bool
	var denyExpires, allowExpires *time.Time
	for rows.Next() {
		var effect string
		var expiresAt *time.Time
		if err := rows.Scan(&effect, &expiresAt); err != nil {
			return nil, nil, err
		}
		switch model.Effect(effect) {
		case model.EffectDeny:
			denyFound, denyExpires = true, expiresAt
		case model.EffectAllow:
			allowFound, allowExpires = true, expiresAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if denyFound {
		e := model.EffectDeny
		return &e, denyExpires, nil
	}
	if allowFound {
		e := model.EffectAllow
		return &e, allowExpires, nil
	}
	return nil, nil, nil
}

// CheckRole returns the role name if the user has the permission via a role
// assignment, and that assignment's expires_at (nil when it never expires).
// When several assignments match, the longest-lived one is returned.
func (q *Queries) CheckRole(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType, resourceID string) (string, *time.Time, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return "", nil, err
	}
	defer conn.Release()

	var roleName string
	var expiresAt *time.Time
	err = conn.QueryRow(ctx, checkRoleQuery, userID, permissionID, resourceType, resourceID).Scan(&roleName, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return roleName, expiresAt, nil
}

// Resources returns resource IDs where a user has the given permission.
func (q *Queries) Resources(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType string, scopeFilter map[string]string) ([]string, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	var rows pgx.Rows

	if scopeFilter == nil {
		rows, err = conn.Query(ctx, resourcesQueryNoScope, userID, permissionID, resourceType)
	} else {
		scopeJSON, jsonErr := json.Marshal(scopeFilter)
		if jsonErr != nil {
			return nil, jsonErr
		}
		rows, err = conn.Query(ctx, resourcesQueryWithScope, userID, permissionID, resourceType, scopeJSON)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FilterResources returns the candidates on which the user has the given
// permission: the same ALLOW override ∪ role grant − DENY override set as
// Resources without a scope filter, restricted to candidates in SQL. The
// result is unordered and has no duplicates.
func (q *Queries) FilterResources(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType string, candidates []string) ([]string, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, filterResourcesQuery, userID, permissionID, resourceType, candidates)
	if err != nil {
		return nil, fmt.Errorf("query filter resources: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan filter resource: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filter resources: %w", err)
	}
	return ids, nil
}

// UserRoleGrants returns the user's active role-grant rows, optionally filtered
// by resource type, or by resource type and id. Rows are raw: precedence
// against overrides is applied by the resolver.
func (q *Queries) UserRoleGrants(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.RoleGrant, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	var rows pgx.Rows
	switch {
	case resourceType != "" && resourceID != "":
		rows, err = conn.Query(ctx, listUserPermissionsWithResourceQuery, userID, resourceType, resourceID)
	case resourceType != "":
		rows, err = conn.Query(ctx, listUserPermissionsWithTypeQuery, userID, resourceType)
	default:
		rows, err = conn.Query(ctx, listUserPermissionsQuery, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("query role grants: %w", err)
	}
	defer rows.Close()

	var grants []model.RoleGrant
	for rows.Next() {
		var g model.RoleGrant
		var scopeBytes []byte
		if err := rows.Scan(&g.Action, &g.ResourceType, &g.ResourceID, &scopeBytes, &g.RoleName); err != nil {
			return nil, fmt.Errorf("scan role grant: %w", err)
		}
		if g.Scope, err = decodeScope(scopeBytes); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate role grants: %w", err)
	}
	return grants, nil
}

// UserOverrides returns the user's active override rows (ALLOW and DENY),
// optionally filtered by resource type, or by resource type and id.
func (q *Queries) UserOverrides(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.OverrideGrant, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	var rows pgx.Rows
	switch {
	case resourceType != "" && resourceID != "":
		rows, err = conn.Query(ctx, listUserOverridesWithResourceQuery, userID, resourceType, resourceID)
	case resourceType != "":
		rows, err = conn.Query(ctx, listUserOverridesWithTypeQuery, userID, resourceType)
	default:
		rows, err = conn.Query(ctx, listUserOverridesQuery, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("query overrides: %w", err)
	}
	defer rows.Close()

	var overrides []model.OverrideGrant
	for rows.Next() {
		var o model.OverrideGrant
		var scopeBytes []byte
		var effect string
		if err := rows.Scan(&o.Action, &o.ResourceType, &o.ResourceID, &scopeBytes, &effect); err != nil {
			return nil, fmt.Errorf("scan override: %w", err)
		}
		if o.Scope, err = decodeScope(scopeBytes); err != nil {
			return nil, err
		}
		o.Effect = model.Effect(effect)
		overrides = append(overrides, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate overrides: %w", err)
	}
	return overrides, nil
}

func decodeScope(b []byte) (map[string]string, error) {
	if b == nil {
		return nil, nil
	}
	var scope map[string]string
	if err := json.Unmarshal(b, &scope); err != nil {
		return nil, fmt.Errorf("decode scope: %w", err)
	}
	return scope, nil
}

// LoadPermissions reads all permissions for the registry.
func (q *Queries) LoadPermissions(ctx context.Context) (map[[2]string]int16, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, loadPermissionsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[[2]string]int16)
	for rows.Next() {
		var id int16
		var action, resType string
		if err := rows.Scan(&id, &action, &resType); err != nil {
			return nil, err
		}
		m[[2]string{action, resType}] = id
	}
	return m, rows.Err()
}

// LoadRoles reads all roles for the registry.
func (q *Queries) LoadRoles(ctx context.Context) (map[string]int16, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, loadRolesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]int16)
	for rows.Next() {
		var id int16
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[name] = id
	}
	return m, rows.Err()
}

// ListAllPermissions returns all registered permissions (admin read).
func (q *Queries) ListAllPermissions(ctx context.Context) ([]model.Permission, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, listAllPermissionsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var perms []model.Permission
	for rows.Next() {
		var p model.Permission
		if err := rows.Scan(&p.Action, &p.ResourceType, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

// ListAllRoles returns all roles (admin read).
func (q *Queries) ListAllRoles(ctx context.Context) ([]model.Role, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, listAllRolesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []model.Role
	for rows.Next() {
		var r model.Role
		if err := rows.Scan(&r.Name, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// ListRolePermissions returns permissions assigned to a role (admin read).
func (q *Queries) ListRolePermissions(ctx context.Context, roleID int16) ([]model.Permission, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	rows, err := conn.Query(ctx, listRolePermissionsQuery, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var perms []model.Permission
	for rows.Next() {
		var p model.Permission
		if err := rows.Scan(&p.Action, &p.ResourceType); err != nil {
			return nil, err
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

// --- Worker write path ---

// Writer is the set of writes the worker may perform inside an ApplyEvent
// transaction. It is defined here, next to its implementation, because the
// ApplyEvent callback type must be nameable by both db and worker.
type Writer interface {
	InsertUserRole(ctx context.Context, userID uuid.UUID, roleID int16, resourceType, resourceID string,
		scope map[string]string, grantedBy uuid.UUID, grantedAt time.Time, expiresAt *time.Time) error
	DeleteUserRole(ctx context.Context, userID uuid.UUID, roleID int16, resourceType, resourceID string) error
	UpsertOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string, scope map[string]string,
		effect model.Effect, reason string, setBy uuid.UUID, createdAt time.Time, expiresAt *time.Time) error
	DeleteOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) error
	// InsertPermission is idempotent: it returns the existing id when (action, resourceType) already exists.
	InsertPermission(ctx context.Context, action, resourceType, description string) (int16, error)
	DeletePermission(ctx context.Context, action, resourceType string) error
	// InsertRole is idempotent: it returns the existing id when the name already exists.
	InsertRole(ctx context.Context, name, description string) (int16, error)
	DeleteRole(ctx context.Context, name string) error
	InsertRolePermission(ctx context.Context, roleID, permissionID int16) error
	DeleteRolePermission(ctx context.Context, roleID, permissionID int16) error
	// RoleID and PermissionID resolve natural keys in the same transaction as the
	// writes, so no id is cached across events. Under READ COMMITTED each
	// statement sees the latest committed rows, not one snapshot for the whole
	// transaction: a concurrent remove makes a later write fail its foreign
	// key check or affect no rows, rather than use a stale id.
	// found is false (with a nil error) when the name doesn't exist.
	RoleID(ctx context.Context, name string) (id int16, found bool, err error)
	PermissionID(ctx context.Context, action, resourceType string) (id int16, found bool, err error)
}

// ApplyEvent runs fn in one transaction together with the event_log
// deduplication insert. If eventID is already recorded, it rolls back and
// returns (false, nil) without calling fn. Otherwise it runs fn, commits, and
// returns (true, nil).
func (q *Queries) ApplyEvent(ctx context.Context, eventID uuid.UUID, eventType string, fn func(Writer) error) (bool, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	// Rollback after a successful Commit is a no-op; its error carries no information then.
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, insertEventLogQuery, eventID, eventType)
	if err != nil {
		return false, fmt.Errorf("insert event_log: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if err := fn(txWriter{tx: tx}); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit tx: %w", err)
	}
	return true, nil
}

// EventLogDeleter deletes expired event_log rows on the connection that holds
// the cleanup's advisory lock.
type EventLogDeleter interface {
	// DeleteExpiredEvents deletes up to limit rows processed more than
	// retention ago and returns how many it deleted.
	DeleteExpiredEvents(ctx context.Context, retention time.Duration, limit int) (int64, error)
}

// unlockTimeout bounds releasing an advisory lock. It is detached from the
// caller's ctx, so a cancelled caller still unlocks.
const unlockTimeout = 5 * time.Second

// WithAdvisoryLock acquires a dedicated connection and tries the session
// advisory lock key on it, without waiting. If another session holds the
// lock it returns (false, nil) without calling fn. Otherwise it runs fn with
// a deleter bound to that connection, then unlocks and releases the
// connection, and returns (true, fn's error).
//
// If the unlock fails, the connection is closed rather than returned to the
// pool, so the lock can't outlive the round on a pooled session.
func (q *Queries) WithAdvisoryLock(ctx context.Context, key int64, fn func(EventLogDeleter) error) (locked bool, err error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return false, err
	}
	if err := conn.QueryRow(ctx, tryAdvisoryLockQuery, key).Scan(&locked); err != nil {
		conn.Release()
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !locked {
		conn.Release()
		return false, nil
	}
	defer func() {
		err = errors.Join(err, unlockAndRelease(ctx, conn, key))
	}()
	return true, fn(eventLogConn{conn: conn})
}

// unlockAndRelease releases the advisory lock key held by conn's session and
// returns conn to the pool. If the unlock fails, it closes the connection,
// which ends the session and its lock, before releasing it.
func unlockAndRelease(ctx context.Context, conn *pgxpool.Conn, key int64) error {
	defer conn.Release()
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()

	var unlocked bool
	err := conn.QueryRow(uctx, advisoryUnlockQuery, key).Scan(&unlocked)
	if err == nil && unlocked {
		return nil
	}
	if err == nil {
		err = errors.New("advisory lock was not held")
	}
	// A closed connection is destroyed by Release instead of being reused.
	return errors.Join(fmt.Errorf("advisory unlock: %w", err), conn.Conn().Close(uctx))
}

// eventLogConn implements EventLogDeleter on the lock-holding connection.
type eventLogConn struct {
	conn *pgxpool.Conn
}

func (c eventLogConn) DeleteExpiredEvents(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	tag, err := c.conn.Exec(ctx, deleteExpiredEventsQuery, retention, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired event_log rows: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PermissionID looks up a permission's surrogate id by natural key.
// found is false (with a nil error) when no such permission exists.
func (q *Queries) PermissionID(ctx context.Context, action, resourceType string) (int16, bool, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Release()

	var id int16
	err = conn.QueryRow(ctx, permissionIDQuery, action, resourceType).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup permission id: %w", err)
	}
	return id, true, nil
}

// RoleID looks up a role's surrogate id by name.
// found is false (with a nil error) when no such role exists.
func (q *Queries) RoleID(ctx context.Context, name string) (int16, bool, error) {
	conn, err := q.acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Release()

	var id int16
	err = conn.QueryRow(ctx, roleIDQuery, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup role id: %w", err)
	}
	return id, true, nil
}

// txWriter implements Writer on an open transaction.
type txWriter struct {
	tx pgx.Tx
}

func (w txWriter) InsertUserRole(ctx context.Context, userID uuid.UUID, roleID int16, resourceType, resourceID string, scope map[string]string, grantedBy uuid.UUID, grantedAt time.Time, expiresAt *time.Time) error {
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return fmt.Errorf("marshal scope: %w", err)
	}
	if _, err := w.tx.Exec(ctx, insertUserRoleQuery, userID, roleID, resourceType, resourceID, scopeJSON, grantedBy, grantedAt, expiresAt); err != nil {
		return fmt.Errorf("insert user_role: %w", err)
	}
	return nil
}

func (w txWriter) DeleteUserRole(ctx context.Context, userID uuid.UUID, roleID int16, resourceType, resourceID string) error {
	if _, err := w.tx.Exec(ctx, deleteUserRoleQuery, userID, roleID, resourceType, resourceID); err != nil {
		return fmt.Errorf("delete user_role: %w", err)
	}
	return nil
}

func (w txWriter) UpsertOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string, scope map[string]string, effect model.Effect, reason string, setBy uuid.UUID, createdAt time.Time, expiresAt *time.Time) error {
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return fmt.Errorf("marshal scope: %w", err)
	}
	if _, err := w.tx.Exec(ctx, upsertOverrideQuery, userID, permissionID, resourceID, scopeJSON, string(effect), reason, setBy, createdAt, expiresAt); err != nil {
		return fmt.Errorf("upsert override: %w", err)
	}
	return nil
}

func (w txWriter) DeleteOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) error {
	if _, err := w.tx.Exec(ctx, deleteOverrideQuery, userID, permissionID, resourceID); err != nil {
		return fmt.Errorf("delete override: %w", err)
	}
	return nil
}

func (w txWriter) InsertPermission(ctx context.Context, action, resourceType, description string) (int16, error) {
	var id int16
	if err := w.tx.QueryRow(ctx, insertPermissionQuery, action, resourceType, description).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert permission: %w", err)
	}
	return id, nil
}

func (w txWriter) DeletePermission(ctx context.Context, action, resourceType string) error {
	if _, err := w.tx.Exec(ctx, deletePermissionQuery, action, resourceType); err != nil {
		return fmt.Errorf("delete permission: %w", err)
	}
	return nil
}

func (w txWriter) InsertRole(ctx context.Context, name, description string) (int16, error) {
	var id int16
	if err := w.tx.QueryRow(ctx, insertRoleQuery, name, description).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert role: %w", err)
	}
	return id, nil
}

func (w txWriter) DeleteRole(ctx context.Context, name string) error {
	if _, err := w.tx.Exec(ctx, deleteRoleQuery, name); err != nil {
		return fmt.Errorf("delete role: %w", err)
	}
	return nil
}

func (w txWriter) InsertRolePermission(ctx context.Context, roleID, permissionID int16) error {
	if _, err := w.tx.Exec(ctx, insertRolePermissionQuery, roleID, permissionID); err != nil {
		return fmt.Errorf("insert role_permission: %w", err)
	}
	return nil
}

func (w txWriter) DeleteRolePermission(ctx context.Context, roleID, permissionID int16) error {
	if _, err := w.tx.Exec(ctx, deleteRolePermissionQuery, roleID, permissionID); err != nil {
		return fmt.Errorf("delete role_permission: %w", err)
	}
	return nil
}

func (w txWriter) RoleID(ctx context.Context, name string) (int16, bool, error) {
	var id int16
	err := w.tx.QueryRow(ctx, roleIDQuery, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup role id: %w", err)
	}
	return id, true, nil
}

func (w txWriter) PermissionID(ctx context.Context, action, resourceType string) (int16, bool, error) {
	var id int16
	err := w.tx.QueryRow(ctx, permissionIDQuery, action, resourceType).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup permission id: %w", err)
	}
	return id, true, nil
}

// Ping checks database connectivity.
func (q *Queries) Ping(ctx context.Context) error {
	conn, err := q.acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, "SELECT 1")
	return err
}
