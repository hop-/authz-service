//go:build integration

package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/testinfra"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// singleConnPool returns a pool on the test database with one connection.
func singleConnPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = 1, 0
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// --- AC-3: acquire timeout ---

// An exhausted pool fails every method within the acquire timeout instead of
// waiting for the caller's (longer) deadline.
func TestAcquireTimeout_ExhaustedPoolFailsFast(t *testing.T) {
	p := singleConnPool(t)
	held, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()
	q := db.NewQueries(p, 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	calls := map[string]func() error{
		"Ping":         func() error { return q.Ping(ctx) },
		"CheckRole":    func() error { _, _, err := q.CheckRole(ctx, uuid.New(), 1, "document", "d1"); return err },
		"LoadRoles":    func() error { _, err := q.LoadRoles(ctx); return err },
		"Resources":    func() error { _, err := q.Resources(ctx, uuid.New(), 1, "document", nil); return err },
		"Filter":       func() error { _, err := q.FilterResources(ctx, uuid.New(), 1, "document", []string{"d1"}); return err },
		"UserOverride": func() error { _, err := q.UserOverrides(ctx, uuid.New(), "", ""); return err },
		"ApplyEvent": func() error {
			_, err := q.ApplyEvent(ctx, uuid.New(), "test", func(db.Writer) error { return nil })
			return err
		},
		"WithAdvisoryLock": func() error {
			_, err := q.WithAdvisoryLock(ctx, 42, func(db.EventLogDeleter) error { return nil })
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := call()
			elapsed := time.Since(start)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want an acquire deadline error", err)
			}
			if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
				t.Fatalf("failed after %v, want about the 200ms acquire timeout", elapsed)
			}
			if ctx.Err() != nil {
				t.Fatal("the caller's ctx must not be cancelled by the acquire timeout")
			}
		})
	}
}

// The timeout covers the acquisition only: a query slower than it succeeds.
func TestAcquireTimeout_QueryRunsOnCallerContext(t *testing.T) {
	ctx := context.Background()
	q := db.NewQueries(pool, 200*time.Millisecond)

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lockConn.Release()
	tx, err := lockConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE roles IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}
	committed := make(chan error, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		committed <- tx.Commit(ctx) // releases the lock
	}()

	start := time.Now()
	roles, err := q.LoadRoles(ctx)
	if err != nil {
		t.Fatalf("LoadRoles = %v; the acquire timeout must not bound the query", err)
	}
	if err := <-committed; err != nil {
		t.Fatalf("commit: %v", err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("LoadRoles did not wait for the lock; the test proves nothing")
	}
	if len(roles) == 0 {
		t.Fatal("no roles loaded")
	}
}

func TestAcquireTimeout_ZeroMeansCallerDeadline(t *testing.T) {
	p := singleConnPool(t)
	held, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held.Release()
	q := db.NewQueries(p, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = q.Ping(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("Ping = %v after %v, want the caller's 300ms deadline", err, time.Since(start))
	}
}

// --- AC-6: FilterResources ---

// FilterResources returns exactly Resources (no scope filter) restricted to
// the candidates: ALLOW overrides and role grants, minus DENY overrides,
// ignoring expired grants and other resource types.
func TestFilterResources_MatchesResources(t *testing.T) {
	q := queries()
	ctx := context.Background()
	user := uuid.New()
	for _, id := range []string{"d1", "d2", "d3", "d4"} {
		assignRole(t, q, user, "viewer", "document", id, nil, nil)
	}
	assignRole(t, q, user, "viewer", "document", "d-expired", nil, at(-time.Minute))
	assignRole(t, q, user, "viewer", "project", "p1", nil, nil)
	setOverride(t, q, user, "read", "document", "d2", model.EffectDeny, nil, nil)
	setOverride(t, q, user, "read", "document", "d9", model.EffectAllow, nil, nil)
	setOverride(t, q, user, "read", "document", "d3", model.EffectDeny, nil, at(-time.Minute))  // expired DENY
	setOverride(t, q, user, "read", "document", "d8", model.EffectAllow, nil, at(-time.Minute)) // expired ALLOW
	setOverride(t, q, uuid.New(), "read", "document", "d4", model.EffectDeny, nil, nil)         // another user's DENY
	read := permID(t, q, "read", "document")

	all, err := q.Resources(ctx, user, read, "document", nil)
	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	candidates := []string{"d4", "d2", "zz", "d9", "d-expired", "p1", "d8", "d3", "d1", "d1"}

	got, err := q.FilterResources(ctx, user, read, "document", candidates)
	if err != nil {
		t.Fatalf("FilterResources: %v", err)
	}

	var want []string
	for _, c := range candidates {
		if slices.Contains(all, c) && !slices.Contains(want, c) {
			want = append(want, c)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("FilterResources = %v, want %v (Resources ∩ candidates)", got, want)
	}
	if !slices.Equal(want, []string{"d1", "d3", "d4", "d9"}) {
		t.Fatalf("fixture sanity: Resources ∩ candidates = %v", want)
	}
}

func TestFilterResources_EmptyCandidates(t *testing.T) {
	q := queries()
	user := uuid.New()
	assignRole(t, q, user, "viewer", "document", "d1", nil, nil)
	got, err := q.FilterResources(context.Background(), user, permID(t, q, "read", "document"), "document", []string{})
	if err != nil || len(got) != 0 {
		t.Fatalf("FilterResources(no candidates) = (%v, %v), want nothing", got, err)
	}
}

// --- AC-9: advisory lock and batch delete ---

const testLockKey int64 = 0x7465737434323432

func tryLockElsewhere(t *testing.T, key int64) bool {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		t.Fatalf("try lock: %v", err)
	}
	if locked {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key); err != nil {
			t.Fatalf("unlock: %v", err)
		}
	}
	return locked
}

func TestWithAdvisoryLock_ExcludesOtherSessionsAndUnlocks(t *testing.T) {
	q := queries()
	ctx := context.Background()

	var insideOthers, insideSecond bool
	locked, err := q.WithAdvisoryLock(ctx, testLockKey, func(db.EventLogDeleter) error {
		insideOthers = tryLockElsewhere(t, testLockKey)
		var err error
		insideSecond, err = q.WithAdvisoryLock(ctx, testLockKey, func(db.EventLogDeleter) error {
			t.Error("a second holder ran its fn while the lock was held")
			return nil
		})
		return err
	})
	if err != nil || !locked {
		t.Fatalf("WithAdvisoryLock = (%v, %v), want (true, nil)", locked, err)
	}
	if insideOthers || insideSecond {
		t.Fatalf("lock taken twice: other session %v, second WithAdvisoryLock %v", insideOthers, insideSecond)
	}
	if !tryLockElsewhere(t, testLockKey) {
		t.Fatal("the lock is still held after WithAdvisoryLock returned")
	}
}

func TestWithAdvisoryLock_ReturnsFnErrorAndUnlocks(t *testing.T) {
	boom := errors.New("boom")
	locked, err := queries().WithAdvisoryLock(context.Background(), testLockKey, func(db.EventLogDeleter) error { return boom })
	if !locked || !errors.Is(err, boom) {
		t.Fatalf("WithAdvisoryLock = (%v, %v), want (true, boom)", locked, err)
	}
	if !tryLockElsewhere(t, testLockKey) {
		t.Fatal("the lock is still held after fn failed")
	}
}

func TestWithAdvisoryLock_UnlocksAfterCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	locked, err := queries().WithAdvisoryLock(ctx, testLockKey, func(db.EventLogDeleter) error {
		cancel() // shutdown during the round
		return nil
	})
	if !locked || err != nil {
		t.Fatalf("WithAdvisoryLock = (%v, %v), want (true, nil)", locked, err)
	}
	if !tryLockElsewhere(t, testLockKey) {
		t.Fatal("a cancelled round left the lock held")
	}
}

func TestDeleteExpiredEvents_RetentionAndLimit(t *testing.T) {
	ctx := context.Background()
	eventType := "cleanup.test." + uuid.NewString()[:8]
	insert := func(n int, age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO event_log (event_id, event_type, processed_at)
SELECT gen_random_uuid(), $1, NOW() - $2::interval FROM generate_series(1, $3)`, eventType, age, n); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	count := func() int {
		return countRows(t, `SELECT count(*) FROM event_log WHERE event_type = $1`, eventType)
	}
	insert(7, 48*time.Hour)
	insert(3, time.Hour)

	var batches []int64
	locked, err := queries().WithAdvisoryLock(ctx, testLockKey, func(d db.EventLogDeleter) error {
		for {
			n, err := d.DeleteExpiredEvents(ctx, 24*time.Hour, 3)
			if err != nil {
				return err
			}
			batches = append(batches, n)
			if n < 3 {
				return nil
			}
		}
	})
	if err != nil || !locked {
		t.Fatalf("WithAdvisoryLock = (%v, %v)", locked, err)
	}
	if !slices.Equal(batches, []int64{3, 3, 1}) {
		t.Fatalf("batches = %v, want [3 3 1]", batches)
	}
	if n := count(); n != 3 {
		t.Fatalf("rows left = %d, want the 3 inside the retention", n)
	}
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// --- AC-7: migrations up, down 004/003/002, up again ---

func migrationSQL(t *testing.T, name string) string {
	t.Helper()
	root, err := testinfra.ModuleRoot()
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestMigrations_ConcurrentIndexFilesHoldOneStatement(t *testing.T) {
	for _, name := range []string{
		"003_user_roles_lookup_index.up.sql", "003_user_roles_lookup_index.down.sql",
		"004_event_log_processed_at_index.up.sql", "004_event_log_processed_at_index.down.sql",
	} {
		sql := migrationSQL(t, name)
		var statements int
		for _, line := range strings.Split(sql, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
				statements += strings.Count(line, ";")
			}
		}
		if statements != 1 || !strings.Contains(sql, "CONCURRENTLY") {
			t.Errorf("%s: %d statements, want exactly one CONCURRENTLY statement", name, statements)
		}
	}
}

func TestMigrations_UpDownUp(t *testing.T) {
	p := testinfra.Postgres(t) // fresh database with every up migration applied
	ctx := context.Background()
	exec := func(name string) {
		t.Helper()
		// No arguments: one simple-protocol query per file, as golang-migrate
		// runs it, so CONCURRENTLY must not end up in a transaction.
		if _, err := p.Exec(ctx, migrationSQL(t, name)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	scalar := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := p.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		return n
	}
	validIndex := func(name string) int {
		return scalar(`SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
WHERE c.relname = $1 AND i.indisvalid`, name)
	}
	indexCols := func(name string) string {
		t.Helper()
		var def string
		if err := p.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&def); err != nil {
			t.Fatalf("indexdef %s: %v", name, err)
		}
		return def
	}

	if validIndex("idx_user_roles_lookup") != 1 || validIndex("idx_event_log_processed_at") != 1 {
		t.Fatal("ups did not create both valid indexes")
	}
	if def := indexCols("idx_user_roles_lookup"); !strings.Contains(def, "(user_id, resource_type, resource_id)") {
		t.Fatalf("idx_user_roles_lookup = %s", def)
	}
	if def := indexCols("idx_event_log_processed_at"); !strings.Contains(def, "(processed_at)") {
		t.Fatalf("idx_event_log_processed_at = %s", def)
	}

	// Rows that are not the seed's must survive 002 down.
	if _, err := p.Exec(ctx, `INSERT INTO roles (name) VALUES ('custom-role')`); err != nil {
		t.Fatalf("insert role: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO permissions (action, resource_type) VALUES ('share', 'document')`); err != nil {
		t.Fatalf("insert permission: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r, permissions p WHERE r.name = 'custom-role' AND p.action = 'share'`); err != nil {
		t.Fatalf("insert pair: %v", err)
	}

	exec("004_event_log_processed_at_index.down.sql")
	exec("003_user_roles_lookup_index.down.sql")
	exec("002_seed.down.sql")

	if n := scalar(`SELECT count(*) FROM pg_indexes WHERE indexname IN ('idx_user_roles_lookup', 'idx_event_log_processed_at')`); n != 0 {
		t.Fatalf("%d indexes left after the downs", n)
	}
	if n := scalar(`SELECT count(*) FROM roles WHERE name IN ('viewer', 'editor', 'admin')`); n != 0 {
		t.Fatalf("%d seeded roles left", n)
	}
	if n := scalar(`SELECT count(*) FROM permissions WHERE resource_type = 'document' AND action IN ('read', 'write', 'delete')`); n != 0 {
		t.Fatalf("%d seeded permissions left", n)
	}
	if n := scalar(`SELECT count(*) FROM roles WHERE name = 'custom-role'`); n != 1 {
		t.Fatal("002 down removed a role it did not seed")
	}
	if n := scalar(`SELECT count(*) FROM role_permissions rp JOIN roles r ON r.id = rp.role_id
JOIN permissions p ON p.id = rp.permission_id WHERE r.name = 'custom-role' AND p.action = 'share'`); n != 1 {
		t.Fatal("002 down removed a role-permission pair it did not seed")
	}

	exec("002_seed.up.sql")
	exec("003_user_roles_lookup_index.up.sql")
	exec("004_event_log_processed_at_index.up.sql")

	if validIndex("idx_user_roles_lookup") != 1 || validIndex("idx_event_log_processed_at") != 1 {
		t.Fatal("ups after downs did not recreate both valid indexes")
	}
	if n := scalar(`SELECT count(*) FROM role_permissions`); n != 7 {
		t.Fatalf("role_permissions = %d after re-seeding, want 6 seeded + 1 custom", n)
	}

	// Re-running an up is a no-op (IF NOT EXISTS), and a down twice too (IF EXISTS).
	exec("003_user_roles_lookup_index.up.sql")
	exec("004_event_log_processed_at_index.down.sql")
	exec("004_event_log_processed_at_index.down.sql")
}
