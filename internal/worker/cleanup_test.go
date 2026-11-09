package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"authz-service/internal/db"
	"authz-service/internal/metrics/metricstest"
)

// The production type must satisfy the cleanup's consumer-side interface.
var _ eventLogStore = (*db.Queries)(nil)

// fakeEventLog serves scripted batch results under a fake advisory lock.
type fakeEventLog struct {
	mu        sync.Mutex
	lockHeld  bool    // another worker holds the lock
	lockErr   error   // returned by the lock attempt
	batches   []int64 // rows deleted by successive DeleteExpiredEvents calls
	deleteErr error   // returned once batches run out, if set
	calls     []deleteCall
	keys      []int64
	rounds    int
	unlocked  int
}

type deleteCall struct {
	retention time.Duration
	limit     int
}

func (f *fakeEventLog) WithAdvisoryLock(_ context.Context, key int64, fn func(db.EventLogDeleter) error) (bool, error) {
	f.mu.Lock()
	f.rounds++
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	if f.lockErr != nil {
		return false, f.lockErr
	}
	if f.lockHeld {
		return false, nil
	}
	err := fn(f)
	f.mu.Lock()
	f.unlocked++
	f.mu.Unlock()
	return true, err
}

func (f *fakeEventLog) DeleteExpiredEvents(_ context.Context, retention time.Duration, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, deleteCall{retention, limit})
	if len(f.batches) == 0 {
		return 0, f.deleteErr
	}
	n := f.batches[0]
	f.batches = f.batches[1:]
	return n, nil
}

func deletedTotal(t *testing.T) float64 {
	t.Helper()
	return metricstest.Value(t, "authz_event_log_deleted_total", nil)
}

func TestNewCleanup_Defaults(t *testing.T) {
	c := NewCleanup(&fakeEventLog{}, 24*time.Hour)
	if c.interval != 15*time.Minute || c.batchSize != 5000 || c.retention != 24*time.Hour {
		t.Fatalf("cleanup = interval %v batch %d retention %v; want 15m, 5000, 24h", c.interval, c.batchSize, c.retention)
	}
}

// AC-9: batches run until one deletes fewer rows than the batch size, and
// every deleted row is counted.
func TestCleanup_RunOnceBatchesUntilShort(t *testing.T) {
	store := &fakeEventLog{batches: []int64{5000, 5000, 1234}}
	c := NewCleanup(store, 24*time.Hour)
	before := deletedTotal(t)

	deleted, locked, err := c.runOnce(context.Background())

	if err != nil || !locked || deleted != 11234 {
		t.Fatalf("runOnce = (%d, %v, %v), want (11234, true, nil)", deleted, locked, err)
	}
	if len(store.calls) != 3 {
		t.Fatalf("DELETE batches = %d, want 3", len(store.calls))
	}
	for _, call := range store.calls {
		if call.retention != 24*time.Hour || call.limit != 5000 {
			t.Fatalf("batch call = %+v, want retention 24h, limit 5000", call)
		}
	}
	if d := deletedTotal(t) - before; d != 11234 {
		t.Fatalf("authz_event_log_deleted_total += %v, want 11234", d)
	}
	if store.keys[0] != cleanupLockKey || store.unlocked != 1 {
		t.Fatalf("lock key %v, unlocks %d; want the cleanup key and one unlock", store.keys, store.unlocked)
	}
}

func TestCleanup_RunOnceExactMultipleNeedsAnEmptyBatch(t *testing.T) {
	store := &fakeEventLog{batches: []int64{5000, 0}}
	deleted, _, err := NewCleanup(store, time.Hour).runOnce(context.Background())
	if err != nil || deleted != 5000 || len(store.calls) != 2 {
		t.Fatalf("runOnce = (%d, %v) after %d batches, want 5000 after 2", deleted, err, len(store.calls))
	}
}

// AC-9: only the worker holding the lock deletes.
func TestCleanup_RunOnceSkipsWhenLockHeld(t *testing.T) {
	store := &fakeEventLog{lockHeld: true, batches: []int64{10}}
	before := deletedTotal(t)

	deleted, locked, err := NewCleanup(store, time.Hour).runOnce(context.Background())

	if err != nil || locked || deleted != 0 {
		t.Fatalf("runOnce = (%d, %v, %v), want (0, false, nil)", deleted, locked, err)
	}
	if len(store.calls) != 0 || deletedTotal(t) != before {
		t.Fatal("a worker without the lock must not delete")
	}
}

func TestCleanup_RunOnceErrorKeepsCount(t *testing.T) {
	boom := errors.New("db down")
	store := &fakeEventLog{batches: []int64{5000}, deleteErr: boom}
	before := deletedTotal(t)

	deleted, locked, err := NewCleanup(store, time.Hour).runOnce(context.Background())

	if !errors.Is(err, boom) || !locked || deleted != 5000 {
		t.Fatalf("runOnce = (%d, %v, %v), want (5000, true, db down)", deleted, locked, err)
	}
	if d := deletedTotal(t) - before; d != 5000 {
		t.Fatalf("rows deleted before the error must be counted, += %v", d)
	}
}

func TestCleanup_RunOnceStopsBetweenBatchesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeEventLog{batches: []int64{5000, 5000, 5000}}
	c := NewCleanup(store, time.Hour)
	// Cancel after the first batch.
	c.store = cancelAfterFirstBatch{store, cancel}

	_, _, err := c.runOnce(ctx)

	if !errors.Is(err, context.Canceled) || len(store.calls) != 1 {
		t.Fatalf("runOnce = %v after %d batches, want context.Canceled after 1", err, len(store.calls))
	}
}

type cancelAfterFirstBatch struct {
	*fakeEventLog
	cancel context.CancelFunc
}

func (c cancelAfterFirstBatch) WithAdvisoryLock(ctx context.Context, key int64, fn func(db.EventLogDeleter) error) (bool, error) {
	return c.fakeEventLog.WithAdvisoryLock(ctx, key, func(d db.EventLogDeleter) error {
		return fn(cancellingDeleter{d, c.cancel})
	})
}

type cancellingDeleter struct {
	db.EventLogDeleter
	cancel context.CancelFunc
}

func (d cancellingDeleter) DeleteExpiredEvents(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	n, err := d.EventLogDeleter.DeleteExpiredEvents(ctx, retention, limit)
	d.cancel()
	return n, err
}

// AC-9: Run runs a round per tick, survives failed rounds, and stops on cancel.
func TestCleanup_RunTicksAndStops(t *testing.T) {
	logs := captureLogs(t)
	store := &fakeEventLog{lockErr: errors.New("db down")}
	c := NewCleanup(store, time.Hour)
	tick := make(chan time.Time)
	stopped := make(chan struct{})
	var gotInterval time.Duration
	c.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		gotInterval = d
		return tick, func() { close(stopped) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	tick <- time.Now() // a failing round
	tick <- time.Now() // the loop survived it
	tick <- time.Now() // received only after round 2 finished and logged
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Run must stop its ticker")
	}
	if gotInterval != 15*time.Minute {
		t.Fatalf("ticker interval = %v, want 15m", gotInterval)
	}
	store.mu.Lock()
	rounds := store.rounds
	store.mu.Unlock()
	if rounds != 3 {
		t.Fatalf("rounds = %d, want 3 (one per tick)", rounds)
	}
	// Round 3 may see the cancel and log as interrupted instead.
	if got := recordsAt(logs(), "ERROR", "event_log cleanup failed"); len(got) < 2 {
		t.Fatalf("failed rounds logged = %d, want at least 2", len(got))
	}
}

func TestCleanup_RoundLogsOutcome(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeEventLog
		level string
		msg   string
	}{
		{"done", &fakeEventLog{batches: []int64{3}}, "INFO", "event_log cleanup done"},
		{"skipped", &fakeEventLog{lockHeld: true}, "INFO", "event_log cleanup skipped"},
		{"failed", &fakeEventLog{lockErr: errors.New("x")}, "ERROR", "event_log cleanup failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			NewCleanup(tt.store, time.Hour).round(context.Background())
			got := recordsAt(logs(), tt.level, tt.msg)
			if len(got) != 1 {
				t.Fatalf("logs = %v, want one %s %q", logs(), tt.level, tt.msg)
			}
			if tt.name == "done" && got[0]["deleted"] != float64(3) {
				t.Fatalf("summary deleted = %v, want 3", got[0]["deleted"])
			}
		})
	}
}
