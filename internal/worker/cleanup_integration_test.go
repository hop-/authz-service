//go:build integration

package worker

import (
	"context"
	"testing"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/metrics/metricstest"
	"authz-service/internal/testinfra"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AC-9 against Postgres: the cleanup deletes only rows older than the
// retention, in batches, counts them, and skips while another session holds
// its advisory lock. It has its own database: the package's TestMain (in
// worker_test) keeps its pool private, and this test empties event_log.
func TestCleanup_Postgres(t *testing.T) {
	p := testinfra.Postgres(t)
	ctx := context.Background()
	q := db.NewQueries(p, testinfra.AcquireTimeout)

	insert := func(n int, age time.Duration) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO event_log (event_id, event_type, processed_at)
SELECT gen_random_uuid(), 'test', NOW() - $1::interval FROM generate_series(1, $2)`, age, n); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	count := func(where string) int {
		t.Helper()
		var n int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM event_log WHERE `+where).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("deletes expired rows in batches and counts them", func(t *testing.T) {
		insert(11, 25*time.Hour)
		insert(4, 23*time.Hour)
		c := NewCleanup(q, 24*time.Hour)
		c.batchSize = 5
		before := metricstest.Value(t, "authz_event_log_deleted_total", nil)

		deleted, locked, err := c.runOnce(ctx)

		if err != nil || !locked || deleted != 11 {
			t.Fatalf("runOnce = (%d, %v, %v), want (11, true, nil)", deleted, locked, err)
		}
		if n := count(`processed_at < NOW() - interval '24 hours'`); n != 0 {
			t.Fatalf("%d expired rows left", n)
		}
		if n := count(`true`); n != 4 {
			t.Fatalf("rows left = %d, want the 4 inside the retention", n)
		}
		if d := metricstest.Value(t, "authz_event_log_deleted_total", nil) - before; d != 11 {
			t.Fatalf("authz_event_log_deleted_total += %v, want 11", d)
		}
	})

	t.Run("skips while another worker holds the lock", func(t *testing.T) {
		insert(3, 48*time.Hour)
		holder := lockElsewhere(t, p)
		c := NewCleanup(q, 24*time.Hour)

		deleted, locked, err := c.runOnce(ctx)

		if err != nil || locked || deleted != 0 {
			t.Fatalf("runOnce = (%d, %v, %v), want (0, false, nil)", deleted, locked, err)
		}
		if n := count(`processed_at < NOW() - interval '24 hours'`); n != 3 {
			t.Fatalf("a worker without the lock deleted rows: %d left, want 3", n)
		}

		holder()
		if deleted, locked, err := c.runOnce(ctx); err != nil || !locked || deleted != 3 {
			t.Fatalf("after the lock is free: runOnce = (%d, %v, %v), want (3, true, nil)", deleted, locked, err)
		}
	})

	t.Run("two workers at once: one deletes, one skips", func(t *testing.T) {
		insert(4, 48*time.Hour)
		c1 := NewCleanup(q, 24*time.Hour)
		q2 := db.NewQueries(p, testinfra.AcquireTimeout)
		c2 := NewCleanup(q2, 24*time.Hour)
		started := make(chan struct{})
		release := make(chan struct{})
		c1.store = pausingStore{eventLogStore: q, started: started, release: release}

		type result struct {
			deleted int64
			locked  bool
			err     error
		}
		r1 := make(chan result, 1)
		go func() {
			d, l, err := c1.runOnce(ctx)
			r1 <- result{d, l, err}
		}()
		<-started // c1 holds the lock
		d2, l2, err2 := c2.runOnce(ctx)
		close(release)
		res1 := <-r1

		if err2 != nil || l2 || d2 != 0 {
			t.Fatalf("second worker = (%d, %v, %v), want it to skip", d2, l2, err2)
		}
		if res1.err != nil || !res1.locked || res1.deleted != 4 {
			t.Fatalf("first worker = %+v, want (4, true, nil)", res1)
		}
	})

	t.Run("lock released after each round", func(t *testing.T) {
		holder := lockElsewhere(t, p)
		holder()
	})
}

// lockElsewhere takes the cleanup's advisory lock on a separate session and
// returns a function that releases it.
func lockElsewhere(t *testing.T, p *pgxpool.Pool) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, cleanupLockKey).Scan(&locked); err != nil || !locked {
		conn.Release()
		t.Fatalf("try lock = (%v, %v): the cleanup left its lock held", locked, err)
	}
	return func() {
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, cleanupLockKey); err != nil {
			t.Fatalf("unlock: %v", err)
		}
	}
}

// pausingStore holds the advisory lock until release is closed.
type pausingStore struct {
	eventLogStore
	started chan struct{}
	release chan struct{}
}

func (s pausingStore) WithAdvisoryLock(ctx context.Context, key int64, fn func(db.EventLogDeleter) error) (bool, error) {
	return s.eventLogStore.WithAdvisoryLock(ctx, key, func(d db.EventLogDeleter) error {
		close(s.started)
		<-s.release
		return fn(d)
	})
}
