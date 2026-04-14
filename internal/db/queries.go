package db

import (
	"context"
	"encoding/json"
	"time"

	"authz-service/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Query constants
const (
	// Steps 2 & 3: override lookup
	checkOverrideQuery = `SELECT effect FROM user_permission_overrides
WHERE user_id       = $1
  AND permission_id = $2
  AND resource_id   = $3
  AND (expires_at IS NULL OR expires_at > NOW())`

	// Step 4: role expansion
	checkRoleQuery = `SELECT r.name
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN roles r             ON r.id       = ur.role_id
WHERE ur.user_id       = $1
  AND rp.permission_id = $2
  AND ur.resource_type = $3
  AND ur.resource_id   = $4
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())
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

	// List all permissions for a user (for GET /query/v1/permissions)
	listUserPermissionsQuery = `SELECT p.action, p.resource_type, ur.resource_id, ur.scope, r.name AS source
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
JOIN roles r ON r.id = ur.role_id
WHERE ur.user_id = $1
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())`

	listUserPermissionsWithResourceQuery = `SELECT p.action, p.resource_type, ur.resource_id, ur.scope, r.name AS source
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
JOIN roles r ON r.id = ur.role_id
WHERE ur.user_id = $1
  AND ur.resource_type = $2
  AND ur.resource_id = $3
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())`

	listUserPermissionsWithTypeQuery = `SELECT p.action, p.resource_type, ur.resource_id, ur.scope, r.name AS source
FROM user_roles ur
JOIN role_permissions rp ON rp.role_id = ur.role_id
JOIN permissions p ON p.id = rp.permission_id
JOIN roles r ON r.id = ur.role_id
WHERE ur.user_id = $1
  AND ur.resource_type = $2
  AND (ur.expires_at IS NULL OR ur.expires_at > NOW())`

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

	insertPermissionQuery = `INSERT INTO permissions (action, resource_type, description) VALUES ($1, $2, $3) RETURNING id`

	deletePermissionQuery = `DELETE FROM permissions WHERE action = $1 AND resource_type = $2`

	insertRoleQuery = `INSERT INTO roles (name, description) VALUES ($1, $2) RETURNING id`

	deleteRoleQuery = `DELETE FROM roles WHERE name = $1`

	insertRolePermissionQuery = `INSERT INTO role_permissions (role_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`

	deleteRolePermissionQuery = `DELETE FROM role_permissions WHERE role_id = $1 AND permission_id = $2`
)

// Queries wraps a pgxpool.Pool and provides typed query methods.
type Queries struct {
	pool *pgxpool.Pool
}

func NewQueries(pool *pgxpool.Pool) *Queries {
	return &Queries{pool: pool}
}

func (q *Queries) Pool() *pgxpool.Pool {
	return q.pool
}

// CheckOverride returns the override effect for a user+permission+resource, if any.
func (q *Queries) CheckOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) (*model.Effect, error) {
	rows, err := q.pool.Query(ctx, checkOverrideQuery, userID, permissionID, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var denyFound, allowFound bool
	for rows.Next() {
		var effect string
		if err := rows.Scan(&effect); err != nil {
			return nil, err
		}
		switch model.Effect(effect) {
		case model.EffectDeny:
			denyFound = true
		case model.EffectAllow:
			allowFound = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if denyFound {
		e := model.EffectDeny
		return &e, nil
	}
	if allowFound {
		e := model.EffectAllow
		return &e, nil
	}
	return nil, nil
}

// CheckRole returns the role name if the user has the permission via a role assignment.
func (q *Queries) CheckRole(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType, resourceID string) (string, error) {
	var roleName string
	err := q.pool.QueryRow(ctx, checkRoleQuery, userID, permissionID, resourceType, resourceID).Scan(&roleName)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return roleName, err
}

// Resources returns resource IDs where a user has the given permission.
func (q *Queries) Resources(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType string, scopeFilter map[string]string) ([]string, error) {
	var rows pgx.Rows
	var err error

	if scopeFilter == nil {
		rows, err = q.pool.Query(ctx, resourcesQueryNoScope, userID, permissionID, resourceType)
	} else {
		scopeJSON, jsonErr := json.Marshal(scopeFilter)
		if jsonErr != nil {
			return nil, jsonErr
		}
		rows, err = q.pool.Query(ctx, resourcesQueryWithScope, userID, permissionID, resourceType, scopeJSON)
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

// ListUserPermissions returns all resolved permissions for a user.
func (q *Queries) ListUserPermissions(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.PermissionEntry, error) {
	var entries []model.PermissionEntry

	// Role-based permissions
	var rows pgx.Rows
	var err error
	if resourceType != "" && resourceID != "" {
		rows, err = q.pool.Query(ctx, listUserPermissionsWithResourceQuery, userID, resourceType, resourceID)
	} else if resourceType != "" {
		rows, err = q.pool.Query(ctx, listUserPermissionsWithTypeQuery, userID, resourceType)
	} else {
		rows, err = q.pool.Query(ctx, listUserPermissionsQuery, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var e model.PermissionEntry
		var scopeBytes []byte
		var roleName string
		if err := rows.Scan(&e.Action, &e.ResourceType, &e.ResourceID, &scopeBytes, &roleName); err != nil {
			return nil, err
		}
		if scopeBytes != nil {
			_ = json.Unmarshal(scopeBytes, &e.Scope)
		}
		e.Source = "role:" + roleName
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Override-based permissions
	var overrideRows pgx.Rows
	if resourceType != "" && resourceID != "" {
		overrideRows, err = q.pool.Query(ctx, listUserOverridesWithResourceQuery, userID, resourceType, resourceID)
	} else if resourceType != "" {
		overrideRows, err = q.pool.Query(ctx, listUserOverridesWithTypeQuery, userID, resourceType)
	} else {
		overrideRows, err = q.pool.Query(ctx, listUserOverridesQuery, userID)
	}
	if err != nil {
		return nil, err
	}
	defer overrideRows.Close()

	for overrideRows.Next() {
		var e model.PermissionEntry
		var scopeBytes []byte
		var effect string
		if err := overrideRows.Scan(&e.Action, &e.ResourceType, &e.ResourceID, &scopeBytes, &effect); err != nil {
			return nil, err
		}
		if scopeBytes != nil {
			_ = json.Unmarshal(scopeBytes, &e.Scope)
		}
		if model.Effect(effect) == model.EffectAllow {
			e.Source = "override:allow"
			entries = append(entries, e)
		}
	}
	return entries, overrideRows.Err()
}

// LoadPermissions reads all permissions for the registry.
func (q *Queries) LoadPermissions(ctx context.Context) (map[[2]string]int16, error) {
	rows, err := q.pool.Query(ctx, loadPermissionsQuery)
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
	rows, err := q.pool.Query(ctx, loadRolesQuery)
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
	rows, err := q.pool.Query(ctx, listAllPermissionsQuery)
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
	rows, err := q.pool.Query(ctx, listAllRolesQuery)
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
	rows, err := q.pool.Query(ctx, listRolePermissionsQuery, roleID)
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

// --- Worker write methods ---

// InsertEventLog inserts an event_id for deduplication. Returns true if inserted (new event).
func (q *Queries) InsertEventLog(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, eventType string) (bool, error) {
	tag, err := tx.Exec(ctx, insertEventLogQuery, eventID, eventType)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// InsertUserRole inserts a user-role assignment.
func (q *Queries) InsertUserRole(ctx context.Context, tx pgx.Tx, userID uuid.UUID, roleID int16, resourceType, resourceID string, scope map[string]string, grantedBy uuid.UUID, grantedAt time.Time, expiresAt *time.Time) error {
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, insertUserRoleQuery, userID, roleID, resourceType, resourceID, scopeJSON, grantedBy, grantedAt, expiresAt)
	return err
}

// DeleteUserRole deletes a user-role assignment.
func (q *Queries) DeleteUserRole(ctx context.Context, tx pgx.Tx, userID uuid.UUID, roleID int16, resourceType, resourceID string) error {
	_, err := tx.Exec(ctx, deleteUserRoleQuery, userID, roleID, resourceType, resourceID)
	return err
}

// UpsertOverride inserts or updates a permission override.
func (q *Queries) UpsertOverride(ctx context.Context, tx pgx.Tx, userID uuid.UUID, permissionID int16, resourceID string, scope map[string]string, effect model.Effect, reason string, setBy uuid.UUID, createdAt time.Time, expiresAt *time.Time) error {
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, upsertOverrideQuery, userID, permissionID, resourceID, scopeJSON, string(effect), reason, setBy, createdAt, expiresAt)
	return err
}

// DeleteOverride deletes a permission override.
func (q *Queries) DeleteOverride(ctx context.Context, tx pgx.Tx, userID uuid.UUID, permissionID int16, resourceID string) error {
	_, err := tx.Exec(ctx, deleteOverrideQuery, userID, permissionID, resourceID)
	return err
}

// InsertPermission inserts a new permission and returns its surrogate id.
func (q *Queries) InsertPermission(ctx context.Context, tx pgx.Tx, action, resourceType, description string) (int16, error) {
	var id int16
	err := tx.QueryRow(ctx, insertPermissionQuery, action, resourceType, description).Scan(&id)
	return id, err
}

// DeletePermission deletes a permission by natural key. CASCADE removes role_permissions and overrides.
func (q *Queries) DeletePermission(ctx context.Context, tx pgx.Tx, action, resourceType string) error {
	_, err := tx.Exec(ctx, deletePermissionQuery, action, resourceType)
	return err
}

// InsertRole inserts a new role and returns its surrogate id.
func (q *Queries) InsertRole(ctx context.Context, tx pgx.Tx, name, description string) (int16, error) {
	var id int16
	err := tx.QueryRow(ctx, insertRoleQuery, name, description).Scan(&id)
	return id, err
}

// DeleteRole deletes a role by name. CASCADE removes role_permissions and user_roles.
func (q *Queries) DeleteRole(ctx context.Context, tx pgx.Tx, name string) error {
	_, err := tx.Exec(ctx, deleteRoleQuery, name)
	return err
}

// InsertRolePermission links a role to a permission.
func (q *Queries) InsertRolePermission(ctx context.Context, tx pgx.Tx, roleID, permissionID int16) error {
	_, err := tx.Exec(ctx, insertRolePermissionQuery, roleID, permissionID)
	return err
}

// DeleteRolePermission unlinks a role from a permission.
func (q *Queries) DeleteRolePermission(ctx context.Context, tx pgx.Tx, roleID, permissionID int16) error {
	_, err := tx.Exec(ctx, deleteRolePermissionQuery, roleID, permissionID)
	return err
}

// Ping checks database connectivity.
func (q *Queries) Ping(ctx context.Context) error {
	_, err := q.pool.Exec(ctx, "SELECT 1")
	return err
}

// BeginTx starts a new transaction.
func (q *Queries) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return q.pool.Begin(ctx)
}
