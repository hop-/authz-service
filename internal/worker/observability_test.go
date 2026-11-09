package worker

import (
	"context"
	"errors"
	"testing"

	"authz-service/internal/metrics"
	"authz-service/internal/metrics/metricstest"

	"github.com/segmentio/kafka-go"
)

func processed(t *testing.T, eventType, status string) float64 {
	t.Helper()
	return metricstest.Value(t, "authz_worker_events_processed_total", map[string]string{"event_type": eventType, "status": status})
}

func durations(t *testing.T, eventType string) float64 {
	t.Helper()
	return metricstest.Value(t, "authz_worker_processing_duration_seconds", map[string]string{"event_type": eventType})
}

// AC-1: Handle counts each handled event by type and outcome and times it.
func TestHandle_Metrics(t *testing.T) {
	revoke := func(t *testing.T) kafka.Message {
		return event(t, "role.revoked", map[string]any{
			"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
		})
	}
	tests := []struct {
		name      string
		setup     func(hs *harness)
		msg       func(t *testing.T) kafka.Message
		eventType string
		status    string
	}{
		{"applied", func(hs *harness) { hs.seedIDs() }, revoke, "role.revoked", metrics.StatusOK},
		{"duplicate", func(hs *harness) { hs.seedIDs(); hs.store.duplicate = true }, revoke, "role.revoked", metrics.StatusDuplicate},
		{"no DB phase", func(*harness) {}, func(t *testing.T) kafka.Message {
			return event(t, "role.id.assigned", map[string]any{"role_name": "editor", "id": 1})
		}, "role.id.assigned", metrics.StatusOK},
		{"dead-lettered", func(hs *harness) {}, func(t *testing.T) kafka.Message {
			return event(t, "role.assigned", map[string]any{
				"user_id": testUserID.String(), "role_name": "ghost", "granted_by": testActorID.String(),
			})
		}, "role.assigned", metrics.StatusDLQ},
		{"unknown type is bounded", func(*harness) {}, func(t *testing.T) kafka.Message {
			return event(t, "role.exploded", nil)
		}, metrics.EventTypeUnknown, metrics.StatusDLQ},
		{"bad envelope", func(*harness) {}, func(*testing.T) kafka.Message { return badEnvelope() },
			metrics.EventTypeUnknown, metrics.StatusDLQ},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			tt.setup(hs)
			before, beforeDur := processed(t, tt.eventType, tt.status), durations(t, tt.eventType)

			hs.handle(t, tt.msg(t))

			if d := processed(t, tt.eventType, tt.status) - before; d != 1 {
				t.Errorf("events_processed_total{event_type=%q,status=%q} += %v, want 1", tt.eventType, tt.status, d)
			}
			if d := durations(t, tt.eventType) - beforeDur; d != 1 {
				t.Errorf("processing_duration_seconds{event_type=%q} count += %v, want 1", tt.eventType, d)
			}
		})
	}
}

func TestHandle_AbortIsNotCounted(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := event(t, "role.revoked", map[string]any{
		"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
	})
	hs.store.applyErr = context.Canceled
	var before float64
	for _, s := range []string{metrics.StatusOK, metrics.StatusDuplicate, metrics.StatusDLQ} {
		before += processed(t, "role.revoked", s)
	}
	beforeDur := durations(t, "role.revoked")

	if err := hs.h.Handle(ctx, msg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle = %v, want context.Canceled", err)
	}

	var after float64
	for _, s := range []string{metrics.StatusOK, metrics.StatusDuplicate, metrics.StatusDLQ} {
		after += processed(t, "role.revoked", s)
	}
	if after != before || durations(t, "role.revoked") != beforeDur {
		t.Fatal("an aborted event must not be counted or timed")
	}
}

// AC-2: every entry about an event carries event_id and event_type, plus
// user_id when the event has one.
func TestHandle_EventLogFields(t *testing.T) {
	t.Run("per-user event", func(t *testing.T) {
		logs := captureLogs(t)
		hs := newHarness(t)
		hs.seedIDs()
		hs.store.duplicate = true
		hs.handle(t, event(t, "role.revoked", map[string]any{
			"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
		}))

		records := logs()
		if len(records) < 2 {
			t.Fatalf("want 'processing event' and 'duplicate event' entries, got %v", records)
		}
		for _, r := range records {
			if r["event_id"] != testEventID.String() || r["event_type"] != "role.revoked" || r["user_id"] != testUserID.String() {
				t.Errorf("entry %q lacks event fields: %v", r["msg"], r)
			}
		}
	})

	t.Run("registry event has no user_id", func(t *testing.T) {
		logs := captureLogs(t)
		hs := newHarness(t)
		hs.handle(t, event(t, "role.created", map[string]any{"role_name": "auditor"}))
		for _, r := range logs() {
			if r["event_id"] != testEventID.String() || r["event_type"] != "role.created" {
				t.Errorf("entry %q lacks event fields: %v", r["msg"], r)
			}
			if _, ok := r["user_id"]; ok {
				t.Errorf("entry %q has a user_id for a registry event", r["msg"])
			}
		}
	})

	t.Run("retries and DLQ entries", func(t *testing.T) {
		logs := captureLogs(t)
		hs := newHarness(t)
		hs.seedIDs()
		hs.store.applyErr = errors.New("connection reset")
		hs.handle(t, event(t, "role.revoked", map[string]any{
			"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
		}))
		records := logs()
		if len(recordsAt(records, "WARN", "transient error, retrying")) == 0 || len(recordsAt(records, "WARN", "message sent to DLQ")) != 1 {
			t.Fatalf("expected retry and DLQ entries, got %v", records)
		}
		for _, r := range records {
			if r["event_id"] != testEventID.String() || r["event_type"] != "role.revoked" || r["user_id"] != testUserID.String() {
				t.Errorf("entry %q lacks event fields: %v", r["msg"], r)
			}
		}
	})

	t.Run("consumer commit failure", func(t *testing.T) {
		logs := captureLogs(t)
		hs := newHarness(t)
		hs.seedIDs()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reader := &fakeReader{msgs: []kafka.Message{revokeMsg(t, 3)}, onEmpty: cancel, commitErr: errors.New("commit failed")}
		if err := newConsumer(reader, hs.h).Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
		got := recordsAt(logs(), "ERROR", "failed to commit message")
		if len(got) != 1 || got[0]["event_id"] != testEventID.String() || got[0]["event_type"] != "role.revoked" {
			t.Fatalf("commit failure entry = %v, want event fields", got)
		}
	})
}
