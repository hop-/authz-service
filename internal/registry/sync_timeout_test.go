package registry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"authz-service/internal/metrics/metricstest"
)

// hungLoader blocks every load until its ctx is done, like a hung replica query.
type hungLoader struct{ calls chan struct{} }

func (l *hungLoader) LoadPermissions(ctx context.Context) (map[[2]string]int16, error) {
	select {
	case l.calls <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l *hungLoader) LoadRoles(ctx context.Context) (map[string]int16, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestNewSyncer_ReloadTimeoutIs30s(t *testing.T) {
	if s := NewSyncer(nil, "t", NewEmpty(), &fakeLoader{}, time.Minute); s.reloadTimeout != 30*time.Second {
		t.Fatalf("reloadTimeout = %v, want 30s", s.reloadTimeout)
	}
}

// 008 F-002: a hung periodic reload gives up after reloadTimeout, leaves the
// registry unchanged, and releases applyMu.
func TestReloadBounded_TimesOutHungLoad(t *testing.T) {
	reg := NewEmpty()
	reg.AddPermission("read", "document", 1)
	s := NewSyncer(nil, "t", reg, &hungLoader{calls: make(chan struct{}, 1)}, time.Hour)
	s.reloadTimeout = 50 * time.Millisecond

	start := time.Now()
	err := s.reloadBounded(context.Background())

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reloadBounded = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("reload took %v despite the 50ms timeout", elapsed)
	}
	if _, ok := reg.ResolvePermission("read", "document"); !ok {
		t.Fatal("a failed reload must leave the registry unchanged")
	}
	if !s.applyMu.TryLock() {
		t.Fatal("applyMu still held after the reload gave up")
	}
	s.applyMu.Unlock()
}

// 008 F-002: event application waits at most about reloadTimeout behind a
// hung reload, instead of stalling indefinitely.
func TestReloadLoop_HungReloadDoesNotStallEvents(t *testing.T) {
	reg := NewEmpty()
	l := &hungLoader{calls: make(chan struct{}, 1)}
	s := NewSyncer(nil, "t", reg, l, 5*time.Millisecond)
	s.reloadTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.reloadLoop(ctx)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()
	<-l.calls // a reload is in flight and holds applyMu

	applied := make(chan struct{})
	go func() {
		defer close(applied)
		s.applyMu.Lock()
		applyEvent(reg, []byte(`{"event":"role.id.assigned","role_name":"auditor","id":9}`))
		s.applyMu.Unlock()
	}()

	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("event application stalled behind a hung reload")
	}
	if id, ok := reg.ResolveRole("auditor"); !ok || id != 9 {
		t.Fatalf("event not applied: (%d, %v)", id, ok)
	}
}

func updates(t *testing.T, eventType string) float64 {
	t.Helper()
	return metricstest.Value(t, "authz_registry_updates_total", map[string]string{"event_type": eventType})
}

// AC-1: the syncer counts applied registry events by type; other events and
// malformed registry events are not counted.
func TestApplyEvent_CountsRegistryUpdates(t *testing.T) {
	enc := func(m map[string]any) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	tests := []struct {
		name      string
		value     []byte
		eventType string
		want      float64
	}{
		{"action.id.assigned", enc(map[string]any{"event": "action.id.assigned", "action": "a", "resource_type": "r", "id": 1}), "action.id.assigned", 1},
		{"action.removed", enc(map[string]any{"event": "action.removed", "action": "a", "resource_type": "r"}), "action.removed", 1},
		{"role.id.assigned", enc(map[string]any{"event": "role.id.assigned", "role_name": "x", "id": 2}), "role.id.assigned", 1},
		{"role.removed", enc(map[string]any{"event": "role.removed", "role_name": "x"}), "role.removed", 1},
		{"not a registry event", enc(map[string]any{"event": "role.created", "role_name": "x"}), "role.created", 0},
		{"malformed registry event", []byte(`{"event":"role.id.assigned","id":"not-a-number"}`), "role.id.assigned", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := updates(t, tt.eventType)
			applyEvent(NewEmpty(), tt.value)
			if d := updates(t, tt.eventType) - before; d != tt.want {
				t.Fatalf("authz_registry_updates_total{event_type=%q} += %v, want %v", tt.eventType, d, tt.want)
			}
		})
	}
}

// AC-1: a successful periodic reload counts as event_type="reload"; a failed one doesn't.
func TestReloadLoop_CountsReloads(t *testing.T) {
	reloaded := make(chan struct{})
	l := &fakeLoader{perms: map[[2]string]int16{{"read", "document"}: 1}, roles: map[string]int16{}}
	l.block = func(call int) {
		if call == 2 {
			close(reloaded)
		}
	}
	s := NewSyncer(nil, "t", NewEmpty(), l, 5*time.Millisecond)
	before := updates(t, "reload")
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.reloadLoop(ctx)
	}()
	<-reloaded // the second reload is inside LoadRoles; the first has been counted
	cancel()
	<-loopDone
	if d := updates(t, "reload") - before; d < 1 {
		t.Fatalf("reload counter += %v, want at least 1", d)
	}

	l2 := &fakeLoader{permErr: errors.New("replica down")}
	failed := make(chan struct{})
	l2.snapshot = func(call int) (map[[2]string]int16, map[string]int16) {
		if call == 2 {
			close(failed)
		}
		return nil, nil
	}
	s2 := NewSyncer(nil, "t", NewEmpty(), l2, 5*time.Millisecond)
	before = updates(t, "reload")
	ctx2, cancel2 := context.WithCancel(context.Background())
	loop2Done := make(chan struct{})
	go func() {
		defer close(loop2Done)
		s2.reloadLoop(ctx2)
	}()
	<-failed
	cancel2()
	<-loop2Done
	if d := updates(t, "reload") - before; d != 0 {
		t.Fatalf("failed reloads changed the reload counter by %v", d)
	}
}
