package worker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"authz-service/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/segmentio/kafka-go"
)

const (
	editorRoleID int16 = 4
	readDocPerm  int16 = 9
)

// seedIDs makes role "editor" and permission (read, document) exist in the
// primary DB, as seen by in-transaction lookups.
func (hs *harness) seedIDs() {
	hs.store.writer.roleIDs["editor"] = editorRoleID
	hs.store.writer.permIDs[[2]string{"read", "document"}] = readDocPerm
}

var wantBackoff = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1600 * time.Millisecond,
}

type wantPublish struct {
	event  string
	fields map[string]any
}

type eventCase struct {
	name          string
	event         string
	payload       map[string]any
	writes        []writerCall
	invalidations []invalidation
	flushes       int
	publish       *wantPublish
	check         func(t *testing.T, hs *harness)
}

// dbEventCases covers the 10 event types that have a DB phase.
func dbEventCases() []eventCase {
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	scope := map[string]string{"team": "a"}
	userInv := []invalidation{{testUserID.String(), "document", "d1"}}

	return []eventCase{
		{
			name:  "role.assigned",
			event: "role.assigned",
			payload: map[string]any{
				"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document",
				"resource_id": "d1", "scope": scope, "granted_by": testActorID.String(),
				"expires_at": expires.Format(time.RFC3339),
			},
			writes: []writerCall{{"InsertUserRole", []any{
				testUserID, editorRoleID, "document", "d1", scope, testActorID, testOccurred, &expires,
			}}},
			invalidations: userInv,
		},
		{
			name:  "role.revoked",
			event: "role.revoked",
			payload: map[string]any{
				"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
			},
			writes:        []writerCall{{"DeleteUserRole", []any{testUserID, editorRoleID, "document", "d1"}}},
			invalidations: userInv,
		},
		{
			name:  "override.set",
			event: "override.set",
			payload: map[string]any{
				"user_id": testUserID.String(), "action": "read", "resource_type": "document", "resource_id": "d1",
				"scope": scope, "effect": "DENY", "reason": "suspended", "set_by": testActorID.String(), "expires_at": nil,
			},
			writes: []writerCall{{"UpsertOverride", []any{
				testUserID, readDocPerm, "d1", scope, model.EffectDeny, "suspended", testActorID, testOccurred, (*time.Time)(nil),
			}}},
			invalidations: userInv,
		},
		{
			name:  "override.removed",
			event: "override.removed",
			payload: map[string]any{
				"user_id": testUserID.String(), "action": "read", "resource_type": "document", "resource_id": "d1",
			},
			writes:        []writerCall{{"DeleteOverride", []any{testUserID, readDocPerm, "d1"}}},
			invalidations: userInv,
		},
		{
			name:    "action.registered",
			event:   "action.registered",
			payload: map[string]any{"action": "write", "resource_type": "project", "description": "Write access"},
			writes:  []writerCall{{"InsertPermission", []any{"write", "project", "Write access"}}},
			publish: &wantPublish{"action.id.assigned", map[string]any{"action": "write", "resource_type": "project", "id": int16(12)}},
		},
		{
			name:    "action.removed",
			event:   "action.removed",
			payload: map[string]any{"action": "read", "resource_type": "document"},
			writes:  []writerCall{{"DeletePermission", []any{"read", "document"}}},
			flushes: 1,
		},
		{
			name:    "role.created",
			event:   "role.created",
			payload: map[string]any{"role_name": "auditor", "description": "Audits"},
			writes:  []writerCall{{"InsertRole", []any{"auditor", "Audits"}}},
			publish: &wantPublish{"role.id.assigned", map[string]any{"role_name": "auditor", "id": int16(13)}},
		},
		{
			name:    "role.removed",
			event:   "role.removed",
			payload: map[string]any{"role_name": "editor"},
			writes:  []writerCall{{"DeleteRole", []any{"editor"}}},
			flushes: 1,
		},
		{
			name:    "role_permissions.assigned",
			event:   "role_permissions.assigned",
			payload: map[string]any{"role_name": "editor", "action": "read", "resource_type": "document"},
			writes:  []writerCall{{"InsertRolePermission", []any{editorRoleID, readDocPerm}}},
			flushes: 1,
		},
		{
			name:    "role_permissions.removed",
			event:   "role_permissions.removed",
			payload: map[string]any{"role_name": "editor", "action": "read", "resource_type": "document"},
			writes:  []writerCall{{"DeleteRolePermission", []any{editorRoleID, readDocPerm}}},
			flushes: 1,
		},
	}
}

func (hs *harness) checkFollowUps(t *testing.T, tc eventCase) {
	t.Helper()
	if !reflect.DeepEqual(hs.cache.invalidations, tc.invalidations) {
		t.Errorf("invalidations = %+v, want %+v", hs.cache.invalidations, tc.invalidations)
	}
	if hs.cache.flushes != tc.flushes {
		t.Errorf("FlushAll calls = %d, want %d", hs.cache.flushes, tc.flushes)
	}
	if tc.publish == nil {
		if len(hs.pub.published) != 0 {
			t.Errorf("unexpected publish: %+v", hs.pub.published)
		}
	} else {
		if len(hs.pub.published) != 1 {
			t.Fatalf("got %d publishes, want 1", len(hs.pub.published))
		}
		requirePublished(t, hs.pub.published[0], tc.publish)
	}
	if tc.check != nil {
		tc.check(t, hs)
	}
}

func requirePublished(t *testing.T, got published, want *wantPublish) {
	t.Helper()
	if got.key != "registry" {
		t.Errorf("publish key = %q, want registry", got.key)
	}
	if got.payload["event"] != want.event {
		t.Errorf("published event = %v, want %s", got.payload["event"], want.event)
	}
	if id, ok := got.payload["event_id"].(string); !ok || uuid.Validate(id) != nil {
		t.Errorf("published event_id = %v, want a UUID", got.payload["event_id"])
	}
	for k, v := range want.fields {
		if got.payload[k] != v {
			t.Errorf("published %s = %#v, want %#v", k, got.payload[k], v)
		}
	}
}

// --- Applied path ---

func TestHandle_AppliedPath(t *testing.T) {
	for _, tc := range dbEventCases() {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()
			hs.store.writer.permID = 12
			hs.store.writer.roleID = 13

			hs.handle(t, event(t, tc.event, tc.payload))

			want := []applyCall{{testEventID, tc.event}}
			if !reflect.DeepEqual(hs.store.applyCalls, want) {
				t.Fatalf("ApplyEvent calls = %+v, want %+v", hs.store.applyCalls, want)
			}
			if !reflect.DeepEqual(hs.store.writer.calls, tc.writes) {
				t.Fatalf("writer calls = %+v\nwant %+v", hs.store.writer.calls, tc.writes)
			}
			hs.checkFollowUps(t, tc)
			hs.requireNoDLQ(t)
			if len(hs.sleeps) != 0 {
				t.Errorf("unexpected retries: %v", hs.sleeps)
			}
		})
	}
}

// The worker keeps no registry (CL-1), so *.id.assigned events, which feed the
// API registries, are no-ops for it: handled, with no DB, cache or publish work.
func TestHandle_IDAssignedEventsAreNoOps(t *testing.T) {
	tests := []struct {
		name string
		msg  func(t *testing.T) kafka.Message
	}{
		{"action.id.assigned", func(t *testing.T) kafka.Message {
			return event(t, "action.id.assigned", map[string]any{"action": "write", "resource_type": "project", "id": 21})
		}},
		{"role.id.assigned", func(t *testing.T) kafka.Message {
			return event(t, "role.id.assigned", map[string]any{"role_name": "auditor", "id": 22})
		}},
		{"malformed id is ignored too", func(t *testing.T) kafka.Message {
			return event(t, "action.id.assigned", map[string]any{"action": "a", "resource_type": "b", "id": "x"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)

			hs.handle(t, tt.msg(t))

			if len(hs.store.applyCalls) != 0 || hs.store.roleLookups+hs.store.permLookups != 0 {
				t.Fatalf("*.id.assigned must not touch the DB: apply=%d lookups=%d",
					len(hs.store.applyCalls), hs.store.roleLookups+hs.store.permLookups)
			}
			if len(hs.cache.invalidations) != 0 || hs.cache.flushes != 0 || len(hs.pub.published) != 0 {
				t.Fatal("*.id.assigned must not run follow-ups")
			}
			hs.requireNoDLQ(t)
		})
	}
}

// AC-4: a new event for an existing natural key succeeds and publishes the existing id.
func TestHandle_RegistrationOfExistingKeyPublishesExistingID(t *testing.T) {
	t.Run("action.registered", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.writer.permID = 3 // InsertPermission's insert-or-select returns the existing id
		hs.handle(t, event(t, "action.registered", map[string]any{"action": "read", "resource_type": "document"}))

		hs.requireNoDLQ(t)
		if len(hs.pub.published) != 1 {
			t.Fatalf("got %d publishes, want 1", len(hs.pub.published))
		}
		requirePublished(t, hs.pub.published[0], &wantPublish{"action.id.assigned", map[string]any{"id": int16(3)}})
	})

	t.Run("role.created", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.writer.roleID = 5
		hs.handle(t, event(t, "role.created", map[string]any{"role_name": "editor"}))

		hs.requireNoDLQ(t)
		if len(hs.pub.published) != 1 {
			t.Fatalf("got %d publishes, want 1", len(hs.pub.published))
		}
		requirePublished(t, hs.pub.published[0], &wantPublish{"role.id.assigned", map[string]any{"id": int16(5)}})
	})
}

// --- Duplicate path ---

func TestHandle_DuplicateRunsFollowUpsWithoutApplying(t *testing.T) {
	for _, tc := range dbEventCases() {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()
			hs.store.duplicate = true
			// Duplicate-path id lookups fall back to the DB.
			hs.store.permIDs[[2]string{"write", "project"}] = 12
			hs.store.roleIDs["auditor"] = 13

			hs.handle(t, event(t, tc.event, tc.payload))

			if len(hs.store.applyCalls) != 1 {
				t.Fatalf("ApplyEvent calls = %d, want 1", len(hs.store.applyCalls))
			}
			if hs.store.fnCalls != 0 || len(hs.store.writer.calls) != 0 {
				t.Fatalf("apply must not run for a duplicate: fn=%d writes=%+v", hs.store.fnCalls, hs.store.writer.calls)
			}
			hs.checkFollowUps(t, tc)
			hs.requireNoDLQ(t)
		})
	}
}

func TestHandle_DuplicateRegistrationResolvesID(t *testing.T) {
	actionMsg := func(t *testing.T) kafka.Message {
		return event(t, "action.registered", map[string]any{"action": "write", "resource_type": "project"})
	}
	roleMsg := func(t *testing.T) kafka.Message {
		return event(t, "role.created", map[string]any{"role_name": "auditor"})
	}

	t.Run("action id from DB", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.duplicate = true
		hs.store.permIDs[[2]string{"write", "project"}] = 12

		hs.handle(t, actionMsg(t))

		if hs.store.permLookups != 1 {
			t.Errorf("PermissionID lookups = %d, want 1", hs.store.permLookups)
		}
		if len(hs.pub.published) != 1 {
			t.Fatalf("got %d publishes, want 1", len(hs.pub.published))
		}
		requirePublished(t, hs.pub.published[0], &wantPublish{"action.id.assigned", map[string]any{"id": int16(12)}})
		hs.requireNoDLQ(t)
	})

	t.Run("action no longer exists", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.duplicate = true

		hs.handle(t, actionMsg(t))

		if len(hs.pub.published) != 0 {
			t.Errorf("removed permission must not be re-published: %+v", hs.pub.published)
		}
		hs.requireNoDLQ(t)
	})

	t.Run("role id from DB", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.duplicate = true
		hs.store.roleIDs["auditor"] = 13

		hs.handle(t, roleMsg(t))

		if hs.store.roleLookups != 1 {
			t.Errorf("RoleID lookups = %d, want 1", hs.store.roleLookups)
		}
		if len(hs.pub.published) != 1 {
			t.Fatalf("got %d publishes, want 1", len(hs.pub.published))
		}
		requirePublished(t, hs.pub.published[0], &wantPublish{"role.id.assigned", map[string]any{"id": int16(13)}})
		hs.requireNoDLQ(t)
	})

	t.Run("role no longer exists", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.duplicate = true

		hs.handle(t, roleMsg(t))

		if len(hs.pub.published) != 0 {
			t.Errorf("removed role must not be re-published: %+v", hs.pub.published)
		}
		hs.requireNoDLQ(t)
	})

	t.Run("lookup error is retried then dead-lettered", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.duplicate = true
		hs.store.lookupErr = errors.New("primary unreachable")

		hs.handle(t, actionMsg(t))

		if hs.store.permLookups != 1+maxRetries {
			t.Errorf("PermissionID lookups = %d, want %d", hs.store.permLookups, 1+maxRetries)
		}
		if len(hs.store.applyCalls) != 1 {
			t.Errorf("ApplyEvent calls = %d, want 1 (no tx re-run)", len(hs.store.applyCalls))
		}
		hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
	})
}

// --- Follow-up failures (AC-2) ---

func TestHandle_FollowUpFailureRetriedThenDeadLettered(t *testing.T) {
	t.Run("cache invalidation", func(t *testing.T) {
		hs := newHarness(t)
		hs.seedIDs()
		hs.cache.err = errors.New("redis down")

		hs.handle(t, event(t, "role.revoked", map[string]any{
			"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
		}))

		if len(hs.store.applyCalls) != 1 || hs.store.fnCalls != 1 {
			t.Fatalf("tx must run exactly once: ApplyEvent=%d fn=%d", len(hs.store.applyCalls), hs.store.fnCalls)
		}
		if len(hs.cache.invalidations) != 1+maxRetries {
			t.Fatalf("Invalidate calls = %d, want %d", len(hs.cache.invalidations), 1+maxRetries)
		}
		if !slices.Equal(hs.sleeps, wantBackoff) {
			t.Fatalf("backoff = %v, want %v", hs.sleeps, wantBackoff)
		}
		m := hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
		if !strings.Contains(m.Failure.Error, "redis down") {
			t.Errorf("DLQ error %q does not carry the cause", m.Failure.Error)
		}
	})

	t.Run("cache flush", func(t *testing.T) {
		hs := newHarness(t)
		hs.seedIDs()
		hs.cache.err = errors.New("redis down")

		hs.handle(t, event(t, "role_permissions.assigned", map[string]any{
			"role_name": "editor", "action": "read", "resource_type": "document",
		}))

		if len(hs.store.applyCalls) != 1 {
			t.Fatalf("ApplyEvent calls = %d, want 1", len(hs.store.applyCalls))
		}
		if hs.cache.flushes != 1+maxRetries {
			t.Fatalf("FlushAll calls = %d, want %d", hs.cache.flushes, 1+maxRetries)
		}
		hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
	})

	t.Run("id.assigned publish", func(t *testing.T) {
		hs := newHarness(t)
		hs.store.writer.permID = 12
		hs.pub.publishErr = errors.New("kafka down")

		hs.handle(t, event(t, "action.registered", map[string]any{"action": "write", "resource_type": "project"}))

		if len(hs.store.applyCalls) != 1 || hs.store.fnCalls != 1 {
			t.Fatalf("tx must run exactly once: ApplyEvent=%d fn=%d", len(hs.store.applyCalls), hs.store.fnCalls)
		}
		if len(hs.pub.published) != 1+maxRetries {
			t.Fatalf("Publish calls = %d, want %d", len(hs.pub.published), 1+maxRetries)
		}
		for i, p := range hs.pub.published {
			if p.payload["id"] != int16(12) {
				t.Fatalf("publish attempt %d used id %v, want 12 from the applied tx", i, p.payload["id"])
			}
		}
		hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
	})
}

func TestHandle_FollowUpRecoversWithinRetries(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	hs.cache.errs = []error{errors.New("blip"), errors.New("blip")}

	hs.handle(t, event(t, "override.removed", map[string]any{
		"user_id": testUserID.String(), "action": "read", "resource_type": "document", "resource_id": "d1",
	}))

	if len(hs.store.applyCalls) != 1 {
		t.Fatalf("ApplyEvent calls = %d, want 1", len(hs.store.applyCalls))
	}
	if len(hs.cache.invalidations) != 3 {
		t.Fatalf("Invalidate calls = %d, want 3", len(hs.cache.invalidations))
	}
	if !slices.Equal(hs.sleeps, wantBackoff[:2]) {
		t.Fatalf("backoff = %v, want %v", hs.sleeps, wantBackoff[:2])
	}
	hs.requireNoDLQ(t)
}

// --- Classification (AC-3) ---

func TestIsPermanent(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, true},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, true},
		{"check violation", &pgconn.PgError{Code: "23514"}, true},
		{"invalid text representation", &pgconn.PgError{Code: "22P02"}, true},
		{"numeric out of range", &pgconn.PgError{Code: "22003"}, true},
		{"wrapped integrity violation", fmt.Errorf("insert user_role: %w", &pgconn.PgError{Code: "23505"}), true},
		{"undefined table (class 42)", &pgconn.PgError{Code: "42P01"}, false},
		{"syntax error (class 42)", &pgconn.PgError{Code: "42601"}, false},
		{"connection exception (class 08)", &pgconn.PgError{Code: "08006"}, false},
		{"serialization failure (class 40)", &pgconn.PgError{Code: "40001"}, false},
		{"malformed code", &pgconn.PgError{Code: "2"}, false},
		{"validation error", invalid("unknown role: %s", "x"), true},
		{"wrapped validation error", fmt.Errorf("prepare: %w", invalid("bad")), true},
		{"generic error", errors.New("connection reset"), false},
		{"deadline exceeded", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanent(tt.err); got != tt.want {
				t.Fatalf("isPermanent(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestHandle_DBErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"23505 unique violation", &pgconn.PgError{Code: "23505"}, true},
		{"22P02 invalid text representation", &pgconn.PgError{Code: "22P02"}, true},
		{"42P01 undefined table", &pgconn.PgError{Code: "42P01"}, false},
		{"generic error", errors.New("connection reset by peer"), false},
		{"per-operation deadline", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()
			// The write fails inside the transaction, wrapped the way the db layer wraps it.
			hs.store.writer.err = fmt.Errorf("insert user_role: %w", tt.err)

			hs.handle(t, event(t, "role.assigned", map[string]any{
				"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document",
				"resource_id": "d1", "granted_by": testActorID.String(),
			}))

			if len(hs.cache.invalidations) != 0 {
				t.Errorf("follow-ups must not run when the tx failed")
			}
			if tt.permanent {
				if len(hs.store.applyCalls) != 1 {
					t.Fatalf("permanent error: ApplyEvent calls = %d, want exactly 1", len(hs.store.applyCalls))
				}
				if len(hs.sleeps) != 0 {
					t.Fatalf("permanent error must not back off, slept %v", hs.sleeps)
				}
				hs.requireDLQ(t, model.DLQReasonPermanent, 0)
				return
			}
			if len(hs.store.applyCalls) != 1+maxRetries {
				t.Fatalf("transient error: ApplyEvent calls = %d, want %d", len(hs.store.applyCalls), 1+maxRetries)
			}
			if !slices.Equal(hs.sleeps, wantBackoff) {
				t.Fatalf("backoff = %v, want %v", hs.sleeps, wantBackoff)
			}
			hs.requireDLQ(t, model.DLQReasonTransientExhausted, maxRetries)
		})
	}
}

func TestHandle_TransientDBErrorRecovers(t *testing.T) {
	hs := newHarness(t)
	hs.seedIDs()
	hs.store.applyErrs = []error{errors.New("connection reset"), nil}

	hs.handle(t, event(t, "role.revoked", map[string]any{
		"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
	}))

	if len(hs.store.applyCalls) != 2 || hs.store.fnCalls != 1 {
		t.Fatalf("ApplyEvent=%d fn=%d, want 2 and 1", len(hs.store.applyCalls), hs.store.fnCalls)
	}
	if !slices.Equal(hs.sleeps, wantBackoff[:1]) {
		t.Fatalf("backoff = %v, want %v", hs.sleeps, wantBackoff[:1])
	}
	if len(hs.cache.invalidations) != 1 {
		t.Fatalf("Invalidate calls = %d, want 1", len(hs.cache.invalidations))
	}
	hs.requireNoDLQ(t)
}

func TestNewHandler_DefaultSleepHonoursContext(t *testing.T) {
	h := NewHandler(newFakeStore(), &fakeCache{}, &fakePublisher{}, "w")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := h.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep on a cancelled ctx = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("sleep did not return promptly on cancellation")
	}
	if err := h.sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleep = %v, want nil", err)
	}
}

// --- event_id validation (AC-5) ---

func TestHandle_InvalidEventIDDeadLetteredBeforeDB(t *testing.T) {
	payload := map[string]any{
		"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
	}
	tests := []struct {
		name string
		msg  func(t *testing.T) kafka.Message
	}{
		{"missing", func(t *testing.T) kafka.Message {
			return kafka.Message{Value: []byte(`{"event":"role.revoked","occurred_at":"2026-01-02T03:04:05Z","user_id":"` +
				testUserID.String() + `","role_name":"editor","resource_type":"document","resource_id":"d1"}`)}
		}},
		{"empty", func(t *testing.T) kafka.Message { return message(t, "role.revoked", "", payload) }},
		{"not a uuid", func(t *testing.T) kafka.Message { return message(t, "role.revoked", "evt-123", payload) }},
		{"truncated uuid", func(t *testing.T) kafka.Message {
			return message(t, "role.revoked", "0192f000-0000-7000-8000", payload)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()

			hs.handle(t, tt.msg(t))

			if len(hs.store.applyCalls) != 0 || hs.store.permLookups != 0 || hs.store.roleLookups != 0 {
				t.Fatalf("invalid event_id must not reach the store: apply=%d", len(hs.store.applyCalls))
			}
			if len(hs.cache.invalidations) != 0 || hs.cache.flushes != 0 || len(hs.pub.published) != 0 {
				t.Fatal("invalid event_id must not run follow-ups")
			}
			m := hs.requireDLQ(t, model.DLQReasonPermanent, 0)
			if !strings.Contains(m.Failure.Error, "event_id") {
				t.Errorf("DLQ error %q does not mention event_id", m.Failure.Error)
			}
		})
	}
}

// --- Bad input ---

func TestHandle_BadInputDeadLetteredPermanent(t *testing.T) {
	tests := []struct {
		name string
		msg  func(t *testing.T) kafka.Message
	}{
		{"bad JSON envelope", func(t *testing.T) kafka.Message {
			return kafka.Message{Value: []byte(`{not json`)}
		}},
		{"unknown event type", func(t *testing.T) kafka.Message {
			return event(t, "role.exploded", nil)
		}},
		{"payload type mismatch", func(t *testing.T) kafka.Message {
			return event(t, "role.revoked", map[string]any{"user_id": 123})
		}},
		{"invalid user_id", func(t *testing.T) kafka.Message {
			return event(t, "role.revoked", map[string]any{"user_id": "nope", "role_name": "editor"})
		}},
		{"invalid granted_by", func(t *testing.T) kafka.Message {
			return event(t, "role.assigned", map[string]any{
				"user_id": testUserID.String(), "role_name": "editor", "granted_by": "",
			})
		}},
		{"invalid set_by", func(t *testing.T) kafka.Message {
			return event(t, "override.set", map[string]any{
				"user_id": testUserID.String(), "action": "read", "resource_type": "document", "set_by": "x",
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hs := newHarness(t)
			hs.seedIDs()

			hs.handle(t, tt.msg(t))

			if len(hs.store.applyCalls) != 0 {
				t.Fatalf("bad input must not reach ApplyEvent, got %d calls", len(hs.store.applyCalls))
			}
			if len(hs.sleeps) != 0 {
				t.Fatalf("bad input must not be retried, slept %v", hs.sleeps)
			}
			hs.requireDLQ(t, model.DLQReasonPermanent, 0)
		})
	}
}

func TestHandle_DLQMessageContents(t *testing.T) {
	hs := newHarness(t)
	msg := event(t, "role.exploded", map[string]any{"x": 1})

	hs.handle(t, msg)

	m := hs.requireDLQ(t, model.DLQReasonPermanent, 0)
	if uuid.Validate(m.DLQEventID) != nil {
		t.Errorf("dlq_event_id %q is not a UUID", m.DLQEventID)
	}
	if m.Failure.WorkerID != "worker-1" {
		t.Errorf("worker_id = %q", m.Failure.WorkerID)
	}
	if m.Source != (model.DLQSource{Topic: msg.Topic, Partition: msg.Partition, Offset: msg.Offset}) {
		t.Errorf("source = %+v", m.Source)
	}
	if m.OriginalEventID != testEventID.String() || m.OriginalEventType != "role.exploded" {
		t.Errorf("original id/type = %q/%q", m.OriginalEventID, m.OriginalEventType)
	}
	if !m.OriginalOccurredAt.Equal(testOccurred) {
		t.Errorf("original_occurred_at = %v, want %v", m.OriginalOccurredAt, testOccurred)
	}
	if string(m.OriginalPayload) != string(msg.Value) {
		t.Errorf("original payload not preserved")
	}
	if len(hs.pub.dlqKeys) != 1 || hs.pub.dlqKeys[0] != testEventID.String() {
		t.Errorf("DLQ key = %v, want the original event_id", hs.pub.dlqKeys)
	}
}

// --- Cancellation (AC-3) ---

func TestHandle_CancelledContextAbortsWithoutDLQ(t *testing.T) {
	revoke := map[string]any{
		"user_id": testUserID.String(), "role_name": "editor", "resource_type": "document", "resource_id": "d1",
	}

	t.Run("during the transaction", func(t *testing.T) {
		hs := newHarness(t)
		hs.seedIDs()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs.store.onApply = cancel
		hs.store.applyErr = fmt.Errorf("begin tx: %w", context.Canceled)

		err := hs.h.Handle(ctx, event(t, "role.revoked", revoke))

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle = %v, want context.Canceled", err)
		}
		if len(hs.store.applyCalls) != 1 {
			t.Errorf("ApplyEvent calls = %d, want 1 (no retry after abort)", len(hs.store.applyCalls))
		}
		hs.requireNoDLQ(t)
		if len(hs.cache.invalidations) != 0 {
			t.Error("follow-ups must not run after abort")
		}
	})

	t.Run("during DB backoff", func(t *testing.T) {
		hs := newHarness(t)
		hs.seedIDs()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs.store.applyErr = errors.New("connection reset")
		hs.h.sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}

		err := hs.h.Handle(ctx, event(t, "role.revoked", revoke))

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle = %v, want context.Canceled", err)
		}
		if len(hs.store.applyCalls) != 1 {
			t.Errorf("ApplyEvent calls = %d, want 1", len(hs.store.applyCalls))
		}
		hs.requireNoDLQ(t)
	})

	t.Run("during follow-up backoff", func(t *testing.T) {
		hs := newHarness(t)
		hs.seedIDs()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs.cache.err = errors.New("redis down")
		hs.h.sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}

		err := hs.h.Handle(ctx, event(t, "role.revoked", revoke))

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle = %v, want context.Canceled", err)
		}
		if len(hs.store.applyCalls) != 1 {
			t.Errorf("ApplyEvent calls = %d, want 1", len(hs.store.applyCalls))
		}
		hs.requireNoDLQ(t)
	})

	t.Run("permanent error after cancellation is not dead-lettered", func(t *testing.T) {
		hs := newHarness(t)
		hs.seedIDs()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs.store.onApply = cancel
		hs.store.applyErr = &pgconn.PgError{Code: "23505"}

		err := hs.h.Handle(ctx, event(t, "role.revoked", revoke))

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle = %v, want context.Canceled", err)
		}
		hs.requireNoDLQ(t)
	})
}
