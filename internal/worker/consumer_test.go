package worker

import (
	"context"
	"errors"
	"testing"

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
	hs.seedRegistry()
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
	hs.seedRegistry()
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
	hs.seedRegistry()
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
	hs.seedRegistry()
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
