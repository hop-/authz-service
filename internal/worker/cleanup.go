package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/metrics"
)

// event_log cleanup policy.
const (
	// CleanupInterval is how often a cleanup round runs.
	CleanupInterval = 15 * time.Minute
	// cleanupBatchSize caps the rows one DELETE removes, so no statement
	// holds locks or bloats WAL for long.
	cleanupBatchSize = 5000
	// cleanupLockKey is the Postgres advisory lock key that lets only one
	// worker clean up at a time ("authz_el" in ASCII).
	cleanupLockKey int64 = 0x617574687a5f656c
)

// eventLogStore runs fn while holding an advisory lock on a dedicated
// connection; *db.Queries satisfies it.
type eventLogStore interface {
	WithAdvisoryLock(ctx context.Context, key int64, fn func(db.EventLogDeleter) error) (bool, error)
}

// Cleanup deletes event_log rows older than the retention.
//
// event_log is the worker's dedup marker, committed with each event's writes
// (db.ApplyEvent); it is not an audit log. A row only matters while its
// event can still be redelivered, so DLQ replays and offset resets must
// happen within the retention, or a replayed event may be applied again.
type Cleanup struct {
	store     eventLogStore
	retention time.Duration
	interval  time.Duration
	batchSize int
	// newTicker returns a channel that fires every d and its stop function.
	// Tests replace it to trigger rounds.
	newTicker func(d time.Duration) (<-chan time.Time, func())
}

// NewCleanup returns a Cleanup removing rows processed more than retention
// ago, every CleanupInterval, in batches of 5000.
func NewCleanup(store eventLogStore, retention time.Duration) *Cleanup {
	return &Cleanup{
		store:     store,
		retention: retention,
		interval:  CleanupInterval,
		batchSize: cleanupBatchSize,
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
	}
}

// Run runs a cleanup round on every tick until ctx is done. A failed round
// is logged and retried on the next tick; it never stops the loop.
func (c *Cleanup) Run(ctx context.Context) {
	tick, stop := c.newTicker(c.interval)
	defer stop()
	slog.Info("event_log cleanup scheduled", "retention", c.retention, "interval", c.interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			c.round(ctx)
		}
	}
}

// round runs one cleanup round and logs its outcome.
func (c *Cleanup) round(ctx context.Context) {
	start := time.Now()
	deleted, locked, err := c.runOnce(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		slog.Info("event_log cleanup interrupted by shutdown", "deleted", deleted)
	case err != nil:
		slog.Error("event_log cleanup failed, retrying next round", "deleted", deleted, "error", err)
	case !locked:
		slog.Info("event_log cleanup skipped: another worker holds the lock")
	default:
		slog.Info("event_log cleanup done", "deleted", deleted, "duration_ms", time.Since(start).Milliseconds())
	}
}

// runOnce takes the advisory lock and deletes expired rows in batches until
// a batch deletes fewer than batchSize. It returns the rows deleted, and
// whether this worker got the lock.
func (c *Cleanup) runOnce(ctx context.Context) (deleted int64, locked bool, err error) {
	locked, err = c.store.WithAdvisoryLock(ctx, cleanupLockKey, func(d db.EventLogDeleter) error {
		for {
			n, err := d.DeleteExpiredEvents(ctx, c.retention, c.batchSize)
			if err != nil {
				return err
			}
			deleted += n
			metrics.EventLogDeletedTotal.Add(float64(n))
			if n < int64(c.batchSize) {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	})
	if err != nil {
		return deleted, locked, fmt.Errorf("event_log cleanup: %w", err)
	}
	return deleted, locked, nil
}
