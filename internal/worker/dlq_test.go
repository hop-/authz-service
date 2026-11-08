package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"authz-service/internal/model"

	"github.com/segmentio/kafka-go"
)

// logRecord is one captured JSON log line.
type logRecord map[string]any

// captureLogs routes slog's default logger to a buffer for the rest of the
// test and returns a function that parses what was logged.
func captureLogs(t *testing.T) func() []logRecord {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []logRecord {
		var out []logRecord
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var r logRecord
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("parse log line %q: %v", line, err)
			}
			out = append(out, r)
		}
		return out
	}
}

func recordsAt(records []logRecord, level, msgPrefix string) []logRecord {
	var out []logRecord
	for _, r := range records {
		if r["level"] == level && strings.HasPrefix(r["msg"].(string), msgPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// badEnvelope is a message that always goes to the DLQ as permanent.
func badEnvelope() kafka.Message {
	return kafka.Message{Topic: "authz.permission-events", Partition: 1, Offset: 5, Value: []byte(`{not json`)}
}

var errDLQDown = errors.New("dlq topic unavailable")

// --- F-001: a failed DLQ publish is retried, never dropped ---

func TestSendToDLQ_RetriesUntilPublished(t *testing.T) {
	logs := captureLogs(t)
	hs := newHarness(t)
	hs.pub.dlqErrs = []error{errDLQDown, errDLQDown}

	hs.handle(t, badEnvelope())

	if len(hs.pub.dlqTries) != 3 {
		t.Fatalf("PublishDLQ attempts = %d, want 3", len(hs.pub.dlqTries))
	}
	if want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}; !slices.Equal(hs.sleeps, want) {
		t.Fatalf("backoff = %v, want %v", hs.sleeps, want)
	}
	first := hs.pub.dlqTries[0]
	for i, m := range hs.pub.dlqTries {
		if m.DLQEventID != first.DLQEventID || m.Failure != first.Failure {
			t.Fatalf("attempt %d published a different DLQ message: %+v vs %+v", i, m, first)
		}
	}
	hs.requireDLQ(t, model.DLQReasonPermanent, 0)

	records := logs()
	errs := recordsAt(records, "ERROR", "failed to publish to DLQ")
	if len(errs) != 1 {
		t.Fatalf("ERROR logs for the DLQ failure = %d, want exactly 1 (first failure only)", len(errs))
	}
	dlqJSON, _ := errs[0]["dlq_message"].(string)
	var logged model.DLQMessage
	if err := json.Unmarshal([]byte(dlqJSON), &logged); err != nil {
		t.Fatalf("ERROR log does not carry the full DLQ message: %q (%v)", dlqJSON, err)
	}
	if logged.DLQEventID != first.DLQEventID || string(logged.OriginalPayload) != string(first.OriginalPayload) {
		t.Errorf("logged DLQ message %+v does not match the published one", logged)
	}
	if warns := recordsAt(records, "WARN", "DLQ publish failed, retrying"); len(warns) != 2 {
		t.Errorf("per-attempt WARN logs = %d, want 2", len(warns))
	}
}

func TestSendToDLQ_BackoffIsCappedAt30s(t *testing.T) {
	hs := newHarness(t)
	for range 12 {
		hs.pub.dlqErrs = append(hs.pub.dlqErrs, errDLQDown)
	}

	hs.handle(t, badEnvelope())

	want := []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
		1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond, 12800 * time.Millisecond,
		25600 * time.Millisecond, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	if !slices.Equal(hs.sleeps, want) {
		t.Fatalf("backoff = %v, want %v", hs.sleeps, want)
	}
	if len(hs.pub.dlq) != 1 {
		t.Fatalf("published DLQ messages = %d, want 1", len(hs.pub.dlq))
	}
}

func TestSendToDLQ_CancelledDuringBackoffAborts(t *testing.T) {
	hs := newHarness(t)
	hs.pub.dlqErr = errDLQDown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs.h.sleep = func(ctx context.Context, d time.Duration) error {
		hs.sleeps = append(hs.sleeps, d)
		if len(hs.sleeps) == 3 {
			cancel()
		}
		return ctx.Err()
	}

	err := hs.h.Handle(ctx, badEnvelope())

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle = %v, want context.Canceled (abort, no commit)", err)
	}
	if len(hs.pub.dlqTries) != 3 {
		t.Errorf("PublishDLQ attempts = %d, want 3", len(hs.pub.dlqTries))
	}
	hs.requireNoDLQ(t)
}

func TestSendToDLQ_CancelledDuringPublishAborts(t *testing.T) {
	hs := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The publish fails because shutdown cancelled it.
	hs.pub.onDLQ = cancel
	hs.pub.dlqErr = context.Canceled

	err := hs.h.Handle(ctx, badEnvelope())

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle = %v, want context.Canceled", err)
	}
	if len(hs.pub.dlqTries) != 1 {
		t.Errorf("PublishDLQ attempts = %d, want 1 (no retry after cancellation)", len(hs.pub.dlqTries))
	}
	if len(hs.sleeps) != 0 {
		t.Errorf("must not back off after cancellation, slept %v", hs.sleeps)
	}
}

func TestSendToDLQ_TransientExhaustedAlsoRetried(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	hs.store.applyErr = errors.New("connection reset")
	hs.pub.dlqErrs = []error{errDLQDown}

	hs.handle(t, event(t, "role.revoked", map[string]any{
		"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
	}))

	hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
	if len(hs.pub.dlqTries) != 2 {
		t.Fatalf("PublishDLQ attempts = %d, want 2", len(hs.pub.dlqTries))
	}
	// DB retry backoff, then one DLQ backoff.
	want := append(slices.Clone(wantBackoff), 100*time.Millisecond)
	if !slices.Equal(hs.sleeps, want) {
		t.Fatalf("backoff = %v, want %v", hs.sleeps, want)
	}
}

func TestConsumer_DLQOutageNotCommitted(t *testing.T) {
	hs := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs.pub.dlqErr = errDLQDown
	hs.h.sleep = func(ctx context.Context, d time.Duration) error {
		hs.sleeps = append(hs.sleeps, d)
		if len(hs.sleeps) == 5 {
			cancel() // shutdown while the DLQ is still down
		}
		return ctx.Err()
	}
	reader := &fakeReader{msgs: []kafka.Message{badEnvelope()}}
	c := newConsumer(reader, hs.h)

	err := c.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.committed) != 0 {
		t.Fatalf("a message whose DLQ publish never succeeded must not be committed, committed %+v", reader.committed)
	}
}

func TestConsumer_CommitsAfterDLQRecovers(t *testing.T) {
	hs := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs.pub.dlqErrs = []error{errDLQDown, errDLQDown, errDLQDown}
	reader := &fakeReader{msgs: []kafka.Message{badEnvelope()}, onEmpty: cancel}
	c := newConsumer(reader, hs.h)

	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(reader.committed) != 1 || reader.committed[0].Offset != 5 {
		t.Fatalf("committed = %+v, want offset 5 once the DLQ publish succeeded", reader.committed)
	}
	if len(hs.pub.dlq) != 1 {
		t.Fatalf("published DLQ messages = %d, want 1", len(hs.pub.dlq))
	}
}

// --- F-002: every DLQ path checks ctx first ---

func TestHandle_DLQPathsCheckContextFirst(t *testing.T) {
	tests := []struct {
		name string
		msg  func(t *testing.T) kafka.Message
	}{
		{"bad envelope", func(*testing.T) kafka.Message { return badEnvelope() }},
		{"invalid event_id", func(t *testing.T) kafka.Message {
			return message(t, "role.revoked", "not-a-uuid", map[string]any{"user_id": testUserID.String()})
		}},
		{"prepare validation error", func(t *testing.T) kafka.Message {
			return event(t, "role.exploded", nil)
		}},
		{"unknown role (rejected in the tx)", func(t *testing.T) kafka.Message {
			return event(t, "role.assigned", map[string]any{
				"user_id": testUserID.String(), "role_name": "ghost", "granted_by": testActorID.String(),
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := hs.h.Handle(ctx, tt.msg(t))

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Handle = %v, want context.Canceled", err)
			}
			if len(hs.pub.dlqTries) != 0 {
				t.Fatalf("a cancelled context must not publish to the DLQ, got %d attempts", len(hs.pub.dlqTries))
			}
		})
	}
}
