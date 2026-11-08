package worker

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// fakeReader serves queued messages, then runs onEmpty and blocks until ctx is done.
type fakeReader struct {
	msgs      []kafka.Message
	fetchErrs []error // returned before the queued messages, one per fetch
	onEmpty   func()
	committed []kafka.Message
	commitErr error
	closed    bool
}

func (r *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if len(r.fetchErrs) > 0 {
		err := r.fetchErrs[0]
		r.fetchErrs = r.fetchErrs[1:]
		return kafka.Message{}, err
	}
	if len(r.msgs) > 0 {
		m := r.msgs[0]
		r.msgs = r.msgs[1:]
		return m, nil
	}
	if r.onEmpty != nil {
		r.onEmpty()
	}
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (r *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	r.committed = append(r.committed, msgs...)
	return r.commitErr
}

func (r *fakeReader) Close() error {
	r.closed = true
	return nil
}

func revokeMsg(t *testing.T, offset int64) kafka.Message {
	t.Helper()
	m := event(t, "role.revoked", map[string]any{
		"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
	})
	m.Offset = offset
	return m
}

func TestConsumer_CommitsHandledMessages(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	applied := revokeMsg(t, 1)
	deadLettered := kafka.Message{Offset: 2, Value: []byte(`{not json`)}
	reader := &fakeReader{msgs: []kafka.Message{applied, deadLettered}, onEmpty: cancel}
	c := newConsumer(reader, hs.h)

	err := c.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.committed) != 2 || reader.committed[0].Offset != 1 || reader.committed[1].Offset != 2 {
		t.Fatalf("committed = %+v, want offsets 1 and 2 (applied and DLQ-routed)", reader.committed)
	}
	if len(hs.pub.dlq) != 1 {
		t.Fatalf("expected the bad message in the DLQ, got %d", len(hs.pub.dlq))
	}
}

func TestConsumer_NoCommitAfterAbort(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shutdown arrives while the transaction runs.
	hs.store.onApply = cancel
	hs.store.applyErr = context.Canceled

	reader := &fakeReader{msgs: []kafka.Message{revokeMsg(t, 7), revokeMsg(t, 8)}}
	c := newConsumer(reader, hs.h)

	err := c.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.committed) != 0 {
		t.Fatalf("aborted message must not be committed, committed %+v", reader.committed)
	}
	if len(reader.msgs) != 1 {
		t.Fatalf("Run must stop after an abort; %d messages left, want 1", len(reader.msgs))
	}
	if len(hs.pub.dlq) != 0 {
		t.Fatal("aborted message must not be dead-lettered")
	}
}

func TestConsumer_FetchErrorIsRetried(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &fakeReader{
		fetchErrs: []error{errors.New("broker hiccup")},
		msgs:      []kafka.Message{revokeMsg(t, 3)},
		onEmpty:   cancel,
	}
	c := newConsumer(reader, hs.h)

	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.committed) != 1 || reader.committed[0].Offset != 3 {
		t.Fatalf("committed = %+v, want offset 3", reader.committed)
	}
}

func TestConsumer_CommitErrorDoesNotStopRun(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &fakeReader{
		msgs:      []kafka.Message{revokeMsg(t, 1), revokeMsg(t, 2)},
		onEmpty:   cancel,
		commitErr: errors.New("commit failed"),
	}
	c := newConsumer(reader, hs.h)

	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.committed) != 2 {
		t.Fatalf("commit attempts = %d, want 2", len(reader.committed))
	}
}

func TestConsumer_Close(t *testing.T) {
	reader := &fakeReader{}
	c := newConsumer(reader, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !reader.closed {
		t.Fatal("Close did not close the reader")
	}
}

// --- Fetch backoff (AC-5, F-003) ---

// fetchStep is one scripted FetchMessage result.
type fetchStep struct {
	msg kafka.Message
	err error
}

// scriptedReader returns steps in order, then cancels via onEmpty and blocks
// until ctx is done.
type scriptedReader struct {
	steps     []fetchStep
	onEmpty   func()
	committed []kafka.Message
}

func (r *scriptedReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if len(r.steps) > 0 {
		s := r.steps[0]
		r.steps = r.steps[1:]
		return s.msg, s.err
	}
	if r.onEmpty != nil {
		r.onEmpty()
	}
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (r *scriptedReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	r.committed = append(r.committed, msgs...)
	return nil
}

func (r *scriptedReader) Close() error { return nil }

func recordingSleep(sleeps *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return ctx.Err()
	}
}

func TestConsumer_FetchErrorBackoff(t *testing.T) {
	hiccup := errors.New("broker hiccup")
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	tests := []struct {
		name  string
		steps func(t *testing.T) []fetchStep
		want  []time.Duration
	}{
		{
			name: "doubles and caps at 5s",
			steps: func(*testing.T) []fetchStep {
				var s []fetchStep
				for range 9 {
					s = append(s, fetchStep{err: hiccup})
				}
				return s
			},
			want: []time.Duration{ms(100), ms(200), ms(400), ms(800), ms(1600), ms(3200), 5 * time.Second, 5 * time.Second, 5 * time.Second},
		},
		{
			name: "resets after a successful fetch",
			steps: func(t *testing.T) []fetchStep {
				return []fetchStep{
					{err: hiccup}, {err: hiccup}, {err: hiccup},
					{msg: revokeMsg(t, 1)},
					{err: hiccup},
				}
			},
			want: []time.Duration{ms(100), ms(200), ms(400), ms(100)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &scriptedReader{steps: tt.steps(t), onEmpty: cancel}
			c := newConsumer(reader, hs.h)
			var sleeps []time.Duration
			c.sleep = recordingSleep(&sleeps)

			if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run = %v, want context.Canceled", err)
			}
			if !slices.Equal(sleeps, tt.want) {
				t.Fatalf("backoff = %v, want %v", sleeps, tt.want)
			}
		})
	}
}

func TestConsumer_CancelledDuringFetchBackoff(t *testing.T) {
	hs := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &scriptedReader{steps: []fetchStep{{err: errors.New("broker down")}, {msg: revokeMsg(t, 1)}}}
	c := newConsumer(reader, hs.h)
	c.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.steps) != 1 {
		t.Fatalf("Run must stop during the backoff; %d steps left, want 1", len(reader.steps))
	}
}

func TestNewConsumer_WatchesPartitionChanges(t *testing.T) {
	c := NewConsumer([]string{"localhost:1"}, "authz.permission-events", "authz-worker", nil)
	t.Cleanup(func() { _ = c.Close() }) // nothing was fetched, so Close has nothing to fail on
	r, ok := c.reader.(*kafka.Reader)
	if !ok {
		t.Fatalf("reader is %T, want *kafka.Reader", c.reader)
	}
	cfg := r.Config()
	if !cfg.WatchPartitionChanges {
		t.Fatal("group reader must watch partition changes so topics created later are consumed (F-012)")
	}
	if cfg.GroupID != "authz-worker" || cfg.Topic != "authz.permission-events" {
		t.Fatalf("reader config = group %q topic %q", cfg.GroupID, cfg.Topic)
	}
}
