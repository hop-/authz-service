package worker

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"authz-service/internal/model"
)

// AC-2 (revised by CL-1): the worker keeps no registry. Every event resolves
// its role and permission ids from the primary DB inside its ApplyEvent
// transaction. An unknown name is permanent; a lookup error is transient.

func TestApply_ResolvesIDsInTransaction(t *testing.T) {
	tests := []struct {
		name      string
		event     string
		payload   map[string]any
		wantWrite writerCall
		roles     []string
		perms     [][2]string
	}{
		{
			name:  "role.assigned",
			event: "role.assigned",
			payload: map[string]any{
				"user_id": testUserID.String(), "role_name": "auditor", "resource_type": "document", "resource_id": "d1",
				"granted_by": testActorID.String(),
			},
			wantWrite: writerCall{"InsertUserRole", []any{testUserID, int16(31), "document", "d1", map[string]string(nil), testActorID, testOccurred, (*time.Time)(nil)}},
			roles:     []string{"auditor"},
		},
		{
			name:  "role.revoked",
			event: "role.revoked",
			payload: map[string]any{
				"user_id": testUserID.String(), "role_name": "auditor", "resource_type": "document", "resource_id": "d1",
			},
			wantWrite: writerCall{"DeleteUserRole", []any{testUserID, int16(31), "document", "d1"}},
			roles:     []string{"auditor"},
		},
		{
			name:  "override.set",
			event: "override.set",
			payload: map[string]any{
				"user_id": testUserID.String(), "action": "share", "resource_type": "document", "resource_id": "d1",
				"effect": "ALLOW", "set_by": testActorID.String(),
			},
			wantWrite: writerCall{"UpsertOverride", []any{testUserID, int16(41), "d1", map[string]string(nil), model.EffectAllow, "", testActorID, testOccurred, (*time.Time)(nil)}},
			perms:     [][2]string{{"share", "document"}},
		},
		{
			name:  "override.removed",
			event: "override.removed",
			payload: map[string]any{
				"user_id": testUserID.String(), "action": "share", "resource_type": "document", "resource_id": "d1",
			},
			wantWrite: writerCall{"DeleteOverride", []any{testUserID, int16(41), "d1"}},
			perms:     [][2]string{{"share", "document"}},
		},
		{
			name:      "role_permissions.assigned",
			event:     "role_permissions.assigned",
			payload:   map[string]any{"role_name": "auditor", "action": "share", "resource_type": "document"},
			wantWrite: writerCall{"InsertRolePermission", []any{int16(31), int16(41)}},
			roles:     []string{"auditor"},
			perms:     [][2]string{{"share", "document"}},
		},
		{
			name:      "role_permissions.removed",
			event:     "role_permissions.removed",
			payload:   map[string]any{"role_name": "auditor", "action": "share", "resource_type": "document"},
			wantWrite: writerCall{"DeleteRolePermission", []any{int16(31), int16(41)}},
			roles:     []string{"auditor"},
			perms:     [][2]string{{"share", "document"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			w := hs.store.writer
			w.roleIDs["auditor"] = 31
			w.permIDs[[2]string{"share", "document"}] = 41

			hs.handle(t, event(t, tt.event, tt.payload))

			hs.requireNoDLQ(t)
			if len(w.calls) != 1 || !reflect.DeepEqual(w.calls[0], tt.wantWrite) {
				t.Fatalf("writes = %+v\nwant [%+v]", w.calls, tt.wantWrite)
			}
			if !slices.Equal(w.roleLookups, tt.roles) {
				t.Errorf("in-tx role lookups = %v, want %v", w.roleLookups, tt.roles)
			}
			if !slices.Equal(w.permLookups, tt.perms) {
				t.Errorf("in-tx permission lookups = %v, want %v", w.permLookups, tt.perms)
			}
			if hs.store.roleLookups+hs.store.permLookups != 0 {
				t.Errorf("ids must be resolved inside the transaction, not through the store (store lookups role=%d perm=%d)",
					hs.store.roleLookups, hs.store.permLookups)
			}
		})
	}
}

// TestApply_NoIDCaching: each event resolves again, so a role that was
// removed and re-created (new id) is applied against its current id.
func TestApply_NoIDCaching(t *testing.T) {
	hs := newHarness(t)
	w := hs.store.writer
	assign := map[string]any{
		"user_id": testUserID.String(), "role_name": "auditor", "resource_type": "document", "resource_id": "d1",
		"granted_by": testActorID.String(),
	}
	revoke := map[string]any{
		"user_id": testUserID.String(), "role_name": "auditor", "resource_type": "document", "resource_id": "d1",
	}

	w.roleIDs["auditor"] = 31
	hs.handle(t, message(t, "role.assigned", "0192f000-0000-7000-8000-000000000021", assign))
	// role.removed then role.created were processed elsewhere: same name, new id.
	w.roleIDs["auditor"] = 57
	hs.handle(t, message(t, "role.assigned", "0192f000-0000-7000-8000-000000000022", assign))
	hs.handle(t, message(t, "role.revoked", "0192f000-0000-7000-8000-000000000023", revoke))

	if len(w.calls) != 3 {
		t.Fatalf("writes = %+v, want 3", w.calls)
	}
	if got := w.calls[0].args[1]; got != int16(31) {
		t.Errorf("first assignment role id = %v, want 31", got)
	}
	if got := w.calls[1].args[1]; got != int16(57) {
		t.Errorf("assignment after re-create used role id %v, want the new id 57", got)
	}
	if got := w.calls[2].args[1]; got != int16(57) {
		t.Errorf("revoke after re-create used role id %v, want the new id 57", got)
	}
	if len(w.roleLookups) != 3 {
		t.Errorf("in-tx role lookups = %d, want one per event (3)", len(w.roleLookups))
	}
	hs.requireNoDLQ(t)
}

func TestApply_UnknownNameIsPermanent(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		payload map[string]any
		wantErr string
	}{
		{"unknown role", "role.assigned", map[string]any{
			"user_id": testUserID.String(), "role_name": "ghost", "granted_by": testActorID.String(),
		}, "unknown role: ghost"},
		{"unknown role on revoke", "role.revoked", map[string]any{
			"user_id": testUserID.String(), "role_name": "ghost",
		}, "unknown role: ghost"},
		{"unknown permission", "override.set", map[string]any{
			"user_id": testUserID.String(), "action": "fly", "resource_type": "document", "set_by": testActorID.String(),
		}, "unknown permission: (fly, document)"},
		{"role permission with unknown permission", "role_permissions.assigned", map[string]any{
			"role_name": "editor", "action": "fly", "resource_type": "document",
		}, "unknown permission: (fly, document)"},
		{"role permission with unknown role", "role_permissions.removed", map[string]any{
			"role_name": "ghost", "action": "read", "resource_type": "document",
		}, "unknown role: ghost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()

			hs.handle(t, event(t, tt.event, tt.payload))

			if len(hs.store.applyCalls) != 1 || hs.store.fnCalls != 1 {
				t.Errorf("ApplyEvent calls = %d, fn calls = %d; want 1 and 1 (resolved in the tx)", len(hs.store.applyCalls), hs.store.fnCalls)
			}
			if len(hs.store.writer.calls) != 0 {
				t.Errorf("an unknown name must not write: %+v", hs.store.writer.calls)
			}
			if len(hs.sleeps) != 0 {
				t.Errorf("an unknown name must not be retried, slept %v", hs.sleeps)
			}
			if len(hs.cache.invalidations) != 0 || hs.cache.flushes != 0 {
				t.Error("follow-ups must not run for a dead-lettered event")
			}
			m := hs.requireDLQ(t, model.DLQReasonPermanent, 0)
			if !strings.Contains(m.Failure.Error, tt.wantErr) {
				t.Errorf("DLQ error = %q, want it to contain %q", m.Failure.Error, tt.wantErr)
			}
		})
	}
}

func TestPrepare_DoesNotResolveIDs(t *testing.T) {
	hs := newHarness(t)

	// Invalid user_id and an unknown role: prepare rejects the payload before
	// any lookup or transaction.
	hs.handle(t, event(t, "role.assigned", map[string]any{
		"user_id": "nope", "role_name": "ghost", "granted_by": testActorID.String(),
	}))

	if len(hs.store.applyCalls) != 0 || len(hs.store.writer.roleLookups) != 0 || hs.store.roleLookups != 0 {
		t.Fatalf("validation failure must not reach the DB: apply=%d lookups=%d/%d",
			len(hs.store.applyCalls), len(hs.store.writer.roleLookups), hs.store.roleLookups)
	}
	m := hs.requireDLQ(t, model.DLQReasonPermanent, 0)
	if !strings.Contains(m.Failure.Error, "user_id") {
		t.Errorf("DLQ error = %q, want the user_id validation error", m.Failure.Error)
	}
}

func TestApply_LookupErrorIsTransient(t *testing.T) {
	assign := map[string]any{
		"user_id": testUserID.String(), "role_name": "auditor", "resource_type": "document", "resource_id": "d1",
		"granted_by": testActorID.String(),
	}

	t.Run("exhausted", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.writer.lookupErr = errors.New("connection reset")

		hs.handle(t, event(t, "role.assigned", assign))

		if len(hs.store.applyCalls) != 1+maxRetries {
			t.Errorf("ApplyEvent calls = %d, want %d (the whole tx is retried)", len(hs.store.applyCalls), 1+maxRetries)
		}
		if !slices.Equal(hs.sleeps, wantBackoff) {
			t.Errorf("backoff = %v, want %v", hs.sleeps, wantBackoff)
		}
		if len(hs.store.writer.calls) != 0 {
			t.Errorf("no write may happen without an id: %+v", hs.store.writer.calls)
		}
		m := hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
		if !strings.Contains(m.Failure.Error, "connection reset") {
			t.Errorf("DLQ error = %q, want the lookup error", m.Failure.Error)
		}
	})

	t.Run("recovers", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.writer.roleIDs["auditor"] = 31
		hs.store.writer.lookupErr = errors.New("connection reset")
		hs.h.sleep = func(ctx context.Context, d time.Duration) error {
			hs.sleeps = append(hs.sleeps, d)
			hs.store.writer.lookupErr = nil // the DB is back
			return ctx.Err()
		}

		hs.handle(t, event(t, "role.assigned", assign))

		hs.requireNoDLQ(t)
		if len(hs.store.writer.calls) != 1 || hs.store.writer.calls[0].args[1] != int16(31) {
			t.Fatalf("writes = %+v, want InsertUserRole with role id 31", hs.store.writer.calls)
		}
		if len(hs.sleeps) != 1 || len(hs.store.applyCalls) != 2 {
			t.Errorf("backoff = %v, ApplyEvent calls = %d; want one retry", hs.sleeps, len(hs.store.applyCalls))
		}
	})

	t.Run("cancelled during backoff", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.writer.lookupErr = errors.New("connection reset")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs.h.sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}

		err := hs.h.Handle(ctx, event(t, "role.assigned", assign))

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle = %v, want context.Canceled", err)
		}
		if len(hs.pub.dlqTries) != 0 {
			t.Fatal("an aborted lookup must not be dead-lettered")
		}
	})
}
