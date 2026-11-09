//go:build integration

package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/testinfra"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One migrated Postgres for the package. Tests isolate themselves with fresh
// user ids and fresh role/permission names; the seed (roles viewer, editor,
// admin; permissions read/write/delete on document) is shared and read-only.
var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, stop, err := testinfra.StartPostgres(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	if err := stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop postgres:", err)
	}
	os.Exit(code)
}

var actor = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")

func queries() *db.Queries { return db.NewQueries(pool, testinfra.AcquireTimeout) }

// apply runs fn in ApplyEvent with a fresh event id and requires it to apply.
func apply(t *testing.T, q *db.Queries, fn func(w db.Writer) error) {
	t.Helper()
	applied, err := q.ApplyEvent(context.Background(), uuid.New(), "test.event", fn)
	if err != nil || !applied {
		t.Fatalf("ApplyEvent = (%v, %v), want (true, nil)", applied, err)
	}
}

func roleID(t *testing.T, q *db.Queries, name string) int16 {
	t.Helper()
	id, found, err := q.RoleID(context.Background(), name)
	if err != nil || !found {
		t.Fatalf("RoleID(%s) = (%d, %v, %v)", name, id, found, err)
	}
	return id
}

func permID(t *testing.T, q *db.Queries, action, resourceType string) int16 {
	t.Helper()
	id, found, err := q.PermissionID(context.Background(), action, resourceType)
	if err != nil || !found {
		t.Fatalf("PermissionID(%s, %s) = (%d, %v, %v)", action, resourceType, id, found, err)
	}
	return id
}

func assignRole(t *testing.T, q *db.Queries, user uuid.UUID, role, resourceType, resourceID string, scope map[string]string, expiresAt *time.Time) {
	t.Helper()
	rid := roleID(t, q, role)
	apply(t, q, func(w db.Writer) error {
		return w.InsertUserRole(context.Background(), user, rid, resourceType, resourceID, scope, actor, time.Now(), expiresAt)
	})
}

func setOverride(t *testing.T, q *db.Queries, user uuid.UUID, action, resourceType, resourceID string, effect model.Effect, scope map[string]string, expiresAt *time.Time) {
	t.Helper()
	pid := permID(t, q, action, resourceType)
	apply(t, q, func(w db.Writer) error {
		return w.UpsertOverride(context.Background(), user, pid, resourceID, scope, effect, "test", actor, time.Now(), expiresAt)
	})
}

func at(d time.Duration) *time.Time {
	ts := time.Now().Add(d)
	return &ts
}

// --- /permissions SQL ---

func TestUserRoleGrants_TypeJoin(t *testing.T) {
	q := queries()
	ctx := context.Background()
	user := uuid.New()
	// The seed's permissions are all on resource type "document". An editor
	// assignment on a "project" must not grant document permissions there.
	assignRole(t, q, user, "editor", "document", "d1", map[string]string{"team": "a"}, nil)
	assignRole(t, q, user, "editor", "project", "p1", nil, nil)
	assignRole(t, q, user, "admin", "document", "d-expired", nil, at(-time.Minute))

	grants, err := q.UserRoleGrants(ctx, user, "", "")
	if err != nil {
		t.Fatalf("UserRoleGrants: %v", err)
	}
	var got []string
	for _, g := range grants {
		if g.ResourceType != "document" || g.ResourceID != "d1" || g.RoleName != "editor" {
			t.Errorf("unexpected grant %+v (type join or expiry filter broken)", g)
		}
		if g.Scope["team"] != "a" {
			t.Errorf("grant %+v lost the assignment's scope", g)
		}
		got = append(got, g.Action)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"read", "write"}) {
		t.Fatalf("granted actions = %v, want [read write]", got)
	}

	filtered, err := q.UserRoleGrants(ctx, user, "project", "p1")
	if err != nil {
		t.Fatalf("UserRoleGrants(project, p1): %v", err)
	}
	if len(filtered) != 0 {
		t.Fatalf("project assignment granted %+v; permissions only apply to their own resource type", filtered)
	}
}

func TestUserOverrides_BothEffects(t *testing.T) {
	q := queries()
	ctx := context.Background()
	user := uuid.New()
	setOverride(t, q, user, "write", "document", "d1", model.EffectAllow, map[string]string{"team": "b"}, nil)
	setOverride(t, q, user, "read", "document", "d1", model.EffectDeny, nil, nil)
	setOverride(t, q, user, "delete", "document", "d1", model.EffectAllow, nil, at(-time.Minute))

	overrides, err := q.UserOverrides(ctx, user, "document", "d1")
	if err != nil {
		t.Fatalf("UserOverrides: %v", err)
	}
	effects := map[string]model.Effect{}
	for _, o := range overrides {
		effects[o.Action] = o.Effect
		if o.Action == "write" && o.Scope["team"] != "b" {
			t.Errorf("ALLOW override lost its scope: %+v", o)
		}
	}
	want := map[string]model.Effect{"write": model.EffectAllow, "read": model.EffectDeny}
	if len(effects) != len(want) || effects["write"] != want["write"] || effects["read"] != want["read"] {
		t.Fatalf("overrides = %v, want %v (expired ones excluded)", effects, want)
	}
}

// --- Insert-or-select ---

func TestInsertOrSelect_ReturnsSameIDTwice(t *testing.T) {
	q := queries()
	action, roleName := "share-"+uuid.NewString()[:8], "auditor-"+uuid.NewString()[:8]
	var p1, p2, r1, r2 int16
	apply(t, q, func(w db.Writer) error {
		var err error
		p1, err = w.InsertPermission(context.Background(), action, "document", "first")
		return err
	})
	apply(t, q, func(w db.Writer) error {
		var err error
		p2, err = w.InsertPermission(context.Background(), action, "document", "second")
		return err
	})
	apply(t, q, func(w db.Writer) error {
		var err error
		r1, err = w.InsertRole(context.Background(), roleName, "first")
		return err
	})
	apply(t, q, func(w db.Writer) error {
		var err error
		r2, err = w.InsertRole(context.Background(), roleName, "second")
		return err
	})

	if p1 == 0 || p1 != p2 {
		t.Errorf("InsertPermission ids = %d, %d; want the same non-zero id", p1, p2)
	}
	if r1 == 0 || r1 != r2 {
		t.Errorf("InsertRole ids = %d, %d; want the same non-zero id", r1, r2)
	}
}

// --- ApplyEvent ---

func TestApplyEvent_DuplicateSkipsFn(t *testing.T) {
	q := queries()
	ctx := context.Background()
	eventID := uuid.New()
	calls := 0
	fn := func(db.Writer) error { calls++; return nil }

	if applied, err := q.ApplyEvent(ctx, eventID, "test.event", fn); err != nil || !applied {
		t.Fatalf("first ApplyEvent = (%v, %v), want (true, nil)", applied, err)
	}
	applied, err := q.ApplyEvent(ctx, eventID, "test.event", fn)
	if err != nil || applied {
		t.Fatalf("duplicate ApplyEvent = (%v, %v), want (false, nil)", applied, err)
	}
	if calls != 1 {
		t.Fatalf("fn calls = %d, want 1 (the duplicate must not run fn)", calls)
	}
}

func TestApplyEvent_FnErrorRollsBack(t *testing.T) {
	q := queries()
	ctx := context.Background()
	eventID := uuid.New()
	user := uuid.New()
	rid := roleID(t, q, "viewer")
	boom := errors.New("boom")

	_, err := q.ApplyEvent(ctx, eventID, "role.assigned", func(w db.Writer) error {
		if err := w.InsertUserRole(ctx, user, rid, "document", "d1", nil, actor, time.Now(), nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ApplyEvent = %v, want the fn error", err)
	}

	var logged, roles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_log WHERE event_id = $1`, eventID).Scan(&logged); err != nil {
		t.Fatalf("count event_log: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_roles WHERE user_id = $1`, user).Scan(&roles); err != nil {
		t.Fatalf("count user_roles: %v", err)
	}
	if logged != 0 || roles != 0 {
		t.Fatalf("after rollback event_log=%d user_roles=%d, want 0 and 0", logged, roles)
	}

	// The event is not recorded, so redelivery applies it.
	applied, err := q.ApplyEvent(ctx, eventID, "role.assigned", func(w db.Writer) error {
		return w.InsertUserRole(ctx, user, rid, "document", "d1", nil, actor, time.Now(), nil)
	})
	if err != nil || !applied {
		t.Fatalf("redelivered ApplyEvent = (%v, %v), want (true, nil)", applied, err)
	}
}

// --- Lookups ---

func TestPermissionIDAndRoleID(t *testing.T) {
	q := queries()
	ctx := context.Background()

	if id, found, err := q.PermissionID(ctx, "read", "document"); err != nil || !found || id == 0 {
		t.Errorf("PermissionID(read, document) = (%d, %v, %v)", id, found, err)
	}
	if id, found, err := q.PermissionID(ctx, "read", "spaceship"); err != nil || found || id != 0 {
		t.Errorf("PermissionID(read, spaceship) = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
	if id, found, err := q.RoleID(ctx, "editor"); err != nil || !found || id == 0 {
		t.Errorf("RoleID(editor) = (%d, %v, %v)", id, found, err)
	}
	if id, found, err := q.RoleID(ctx, "ghost"); err != nil || found || id != 0 {
		t.Errorf("RoleID(ghost) = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
}

// --- CheckRole / CheckOverride expiry (H1) ---

func TestCheckRole_LongestLivedGrantDecides(t *testing.T) {
	q := queries()
	ctx := context.Background()
	read := permID(t, q, "read", "document")
	sameExpiry := time.Now().Add(2 * time.Hour).Truncate(time.Microsecond)

	tests := []struct {
		name     string
		assign   func(t *testing.T, user uuid.UUID)
		wantRole string
		wantExp  func(*time.Time) bool
	}{
		{
			name: "later expiry wins",
			assign: func(t *testing.T, user uuid.UUID) {
				assignRole(t, q, user, "viewer", "document", "d1", nil, at(time.Hour))
				assignRole(t, q, user, "editor", "document", "d1", nil, at(3*time.Hour))
			},
			wantRole: "editor",
			wantExp:  func(e *time.Time) bool { return e != nil && time.Until(*e) > 2*time.Hour },
		},
		{
			name: "no expiry beats any expiry",
			assign: func(t *testing.T, user uuid.UUID) {
				assignRole(t, q, user, "editor", "document", "d1", nil, at(time.Hour))
				assignRole(t, q, user, "viewer", "document", "d1", nil, nil)
			},
			wantRole: "viewer",
			wantExp:  func(e *time.Time) bool { return e == nil },
		},
		{
			name: "equal expiry breaks the tie by role name",
			assign: func(t *testing.T, user uuid.UUID) {
				assignRole(t, q, user, "viewer", "document", "d1", nil, &sameExpiry)
				assignRole(t, q, user, "editor", "document", "d1", nil, &sameExpiry)
			},
			wantRole: "editor",
			wantExp:  func(e *time.Time) bool { return e != nil && e.Equal(sameExpiry) },
		},
		{
			name: "expired grants are ignored",
			assign: func(t *testing.T, user uuid.UUID) {
				assignRole(t, q, user, "admin", "document", "d1", nil, at(-time.Minute))
				assignRole(t, q, user, "viewer", "document", "d1", nil, at(time.Hour))
			},
			wantRole: "viewer",
			wantExp:  func(e *time.Time) bool { return e != nil && time.Until(*e) > 50*time.Minute },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := uuid.New()
			tt.assign(t, user)

			// Repeat: the answer must be deterministic.
			for range 5 {
				name, exp, err := q.CheckRole(ctx, user, read, "document", "d1")
				if err != nil {
					t.Fatalf("CheckRole: %v", err)
				}
				if name != tt.wantRole || !tt.wantExp(exp) {
					t.Fatalf("CheckRole = (%q, %v), want role %q", name, exp, tt.wantRole)
				}
			}
		})
	}

	t.Run("no grant", func(t *testing.T) {
		name, exp, err := q.CheckRole(ctx, uuid.New(), read, "document", "d1")
		if err != nil || name != "" || exp != nil {
			t.Fatalf("CheckRole = (%q, %v, %v), want empty", name, exp, err)
		}
	})
}

func TestCheckOverride_ReturnsExpiry(t *testing.T) {
	q := queries()
	ctx := context.Background()
	read := permID(t, q, "read", "document")
	user := uuid.New()
	exp := time.Now().Add(10 * time.Minute).Truncate(time.Microsecond)
	setOverride(t, q, user, "read", "document", "d1", model.EffectAllow, nil, &exp)
	setOverride(t, q, user, "read", "document", "d2", model.EffectDeny, nil, nil)

	effect, gotExp, err := q.CheckOverride(ctx, user, read, "d1")
	if err != nil || effect == nil || *effect != model.EffectAllow || gotExp == nil || !gotExp.Equal(exp) {
		t.Fatalf("CheckOverride(d1) = (%v, %v, %v), want ALLOW expiring %v", effect, gotExp, err, exp)
	}
	effect, gotExp, err = q.CheckOverride(ctx, user, read, "d2")
	if err != nil || effect == nil || *effect != model.EffectDeny || gotExp != nil {
		t.Fatalf("CheckOverride(d2) = (%v, %v, %v), want DENY with no expiry", effect, gotExp, err)
	}
	effect, gotExp, err = q.CheckOverride(ctx, user, read, "d3")
	if err != nil || effect != nil || gotExp != nil {
		t.Fatalf("CheckOverride(d3) = (%v, %v, %v), want none", effect, gotExp, err)
	}
}

// TestWriter_LookupsInTransaction covers the CL-1 lookups the worker runs inside
// ApplyEvent: they see the transaction's own writes, report missing names as
// not found, and resolve a re-created name to its new id.
func TestWriter_LookupsInTransaction(t *testing.T) {
	q := queries()
	ctx := context.Background()
	role, action := "tx-role-"+uuid.NewString()[:8], "tx-act-"+uuid.NewString()[:8]

	var oldRoleID int16
	apply(t, q, func(w db.Writer) error {
		id, found, err := w.RoleID(ctx, role)
		if err != nil || found || id != 0 {
			return fmt.Errorf("RoleID before insert = (%d, %v, %v), want not found", id, found, err)
		}
		if oldRoleID, err = w.InsertRole(ctx, role, "tx"); err != nil {
			return err
		}
		// Same transaction: the new row is visible.
		id, found, err = w.RoleID(ctx, role)
		if err != nil || !found || id != oldRoleID {
			return fmt.Errorf("RoleID after insert in the same tx = (%d, %v, %v), want (%d, true)", id, found, err, oldRoleID)
		}
		permID, err := w.InsertPermission(ctx, action, "document", "tx")
		if err != nil {
			return err
		}
		pid, found, err := w.PermissionID(ctx, action, "document")
		if err != nil || !found || pid != permID {
			return fmt.Errorf("PermissionID in the same tx = (%d, %v, %v), want (%d, true)", pid, found, err, permID)
		}
		pid, found, err = w.PermissionID(ctx, action, "spaceship")
		if err != nil || found || pid != 0 {
			return fmt.Errorf("PermissionID(%s, spaceship) = (%d, %v, %v), want not found", action, pid, found, err)
		}
		return nil
	})

	// Remove and re-create the name: a later transaction resolves the new id.
	apply(t, q, func(w db.Writer) error { return w.DeleteRole(ctx, role) })
	var newRoleID int16
	apply(t, q, func(w db.Writer) error {
		var err error
		newRoleID, err = w.InsertRole(ctx, role, "tx again")
		return err
	})
	if newRoleID == oldRoleID {
		t.Fatalf("re-created role kept id %d", newRoleID)
	}
	apply(t, q, func(w db.Writer) error {
		id, found, err := w.RoleID(ctx, role)
		if err != nil || !found || id != newRoleID {
			return fmt.Errorf("RoleID after re-create = (%d, %v, %v), want (%d, true)", id, found, err, newRoleID)
		}
		return nil
	})
}
