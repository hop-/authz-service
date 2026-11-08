package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLoader serves a fixed snapshot; block, if set, runs inside LoadRoles
// (the last read before the swap).
type fakeLoader struct {
	mu       sync.Mutex
	perms    map[[2]string]int16
	roles    map[string]int16
	permErr  error
	roleErr  error
	calls    int
	block    func(call int)
	snapshot func(call int) (map[[2]string]int16, map[string]int16)
}

func (l *fakeLoader) LoadPermissions(context.Context) (map[[2]string]int16, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.snapshot != nil {
		l.perms, l.roles = l.snapshot(l.calls)
	}
	return l.perms, l.permErr
}

func (l *fakeLoader) LoadRoles(context.Context) (map[string]int16, error) {
	l.mu.Lock()
	call, roles, err, block := l.calls, l.roles, l.roleErr, l.block
	l.mu.Unlock()
	if block != nil {
		block(call)
	}
	return roles, err
}

func TestNew_LoadsSnapshot(t *testing.T) {
	l := &fakeLoader{
		perms: map[[2]string]int16{{"read", "document"}: 1, {"write", "document"}: 2},
		roles: map[string]int16{"editor": 7},
	}

	r, err := New(context.Background(), l)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	if id, ok := r.ResolvePermission("write", "document"); !ok || id != 2 {
		t.Errorf("ResolvePermission(write, document) = (%d, %v)", id, ok)
	}
	if id, ok := r.ResolveRole("editor"); !ok || id != 7 || r.RoleName(7) != "editor" {
		t.Errorf("role editor not loaded: (%d, %v) name %q", id, ok, r.RoleName(7))
	}
}

func TestNew_LoadErrorFails(t *testing.T) {
	boom := errors.New("db down")
	for _, l := range []*fakeLoader{{permErr: boom}, {roleErr: boom}} {
		r, err := New(context.Background(), l)
		if !errors.Is(err, boom) || r != nil {
			t.Fatalf("New = (%v, %v), want (nil, db down)", r, err)
		}
	}
}

func TestReload_ReplacesContents(t *testing.T) {
	r := NewEmpty()
	r.AddPermission("delete", "document", 3)
	r.AddRole("ghost", 9)
	r.AddRole("editor", 7)
	l := &fakeLoader{
		perms: map[[2]string]int16{{"read", "document"}: 1},
		roles: map[string]int16{"editor": 8, "viewer": 5},
	}

	if err := r.Reload(context.Background(), l); err != nil {
		t.Fatalf("Reload = %v", err)
	}

	if _, ok := r.ResolvePermission("delete", "document"); ok {
		t.Error("permission absent from the snapshot survived the reload")
	}
	if _, ok := r.ResolveRole("ghost"); ok || r.RoleName(9) != "" {
		t.Error("role absent from the snapshot survived the reload")
	}
	if id, ok := r.ResolveRole("editor"); !ok || id != 8 || r.RoleName(8) != "editor" || r.RoleName(7) != "" {
		t.Errorf("editor = (%d, %v), names 8=%q 7=%q; want the snapshot's id 8 only", id, ok, r.RoleName(8), r.RoleName(7))
	}
	if r.PermissionCount() != 1 || r.RoleCount() != 2 {
		t.Errorf("counts = %d perms, %d roles; want 1, 2", r.PermissionCount(), r.RoleCount())
	}
}

func TestReload_ErrorLeavesRegistryUnchanged(t *testing.T) {
	boom := errors.New("replica down")
	for name, l := range map[string]*fakeLoader{
		"permissions": {permErr: boom},
		"roles":       {perms: map[[2]string]int16{}, roleErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewEmpty()
			r.AddPermission("read", "document", 1)
			r.AddRole("editor", 7)

			if err := r.Reload(context.Background(), l); !errors.Is(err, boom) {
				t.Fatalf("Reload = %v, want the load error", err)
			}
			if _, ok := r.ResolvePermission("read", "document"); !ok {
				t.Error("permission lost after a failed reload")
			}
			if _, ok := r.ResolveRole("editor"); !ok {
				t.Error("role lost after a failed reload")
			}
		})
	}
}

func TestReload_ConcurrentReadersSeeAWholeSnapshot(t *testing.T) {
	r := NewEmpty()
	l := &fakeLoader{
		perms: map[[2]string]int16{{"read", "document"}: 1},
		roles: map[string]int16{"editor": 7},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			if err := r.Reload(ctx, l); err != nil {
				t.Errorf("Reload = %v", err)
				return
			}
		}
	})
	for range 1000 {
		r.ResolvePermission("read", "document")
		r.ResolveRole("editor")
		r.RoleName(7)
	}
	cancel()
	wg.Wait()
}

func TestApplyEvent(t *testing.T) {
	tests := []struct {
		name  string
		value string
		check func(t *testing.T, r *Registry)
	}{
		{"action.id.assigned adds the permission",
			`{"event":"action.id.assigned","action":"share","resource_type":"document","id":12}`,
			func(t *testing.T, r *Registry) {
				if id, ok := r.ResolvePermission("share", "document"); !ok || id != 12 {
					t.Errorf("ResolvePermission = (%d, %v), want (12, true)", id, ok)
				}
			}},
		{"action.removed removes the permission",
			`{"event":"action.removed","action":"read","resource_type":"document"}`,
			func(t *testing.T, r *Registry) {
				if _, ok := r.ResolvePermission("read", "document"); ok {
					t.Error("permission still present")
				}
			}},
		{"role.id.assigned adds the role",
			`{"event":"role.id.assigned","role_name":"auditor","id":13}`,
			func(t *testing.T, r *Registry) {
				if id, ok := r.ResolveRole("auditor"); !ok || id != 13 || r.RoleName(13) != "auditor" {
					t.Errorf("ResolveRole = (%d, %v)", id, ok)
				}
			}},
		{"role.removed removes the role",
			`{"event":"role.removed","role_name":"editor"}`,
			func(t *testing.T, r *Registry) {
				if _, ok := r.ResolveRole("editor"); ok || r.RoleName(7) != "" {
					t.Error("role still present")
				}
			}},
		{"action.registered waits for the id",
			`{"event":"action.registered","action":"share","resource_type":"document"}`,
			unchanged},
		{"role.created waits for the id",
			`{"event":"role.created","role_name":"auditor"}`,
			unchanged},
		{"user events are ignored",
			`{"event":"role.assigned","user_id":"u","role_name":"editor"}`,
			unchanged},
		{"bad envelope is skipped",
			`{not json`,
			unchanged},
		{"malformed registry event is skipped",
			`{"event":"action.id.assigned","action":"share","resource_type":"document","id":"x"}`,
			unchanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewEmpty()
			r.AddPermission("read", "document", 1)
			r.AddRole("editor", 7)

			applyEvent(r, []byte(tt.value))

			tt.check(t, r)
		})
	}
}

func unchanged(t *testing.T, r *Registry) {
	t.Helper()
	if r.PermissionCount() != 1 || r.RoleCount() != 1 {
		t.Errorf("registry changed: %d perms, %d roles; want 1, 1", r.PermissionCount(), r.RoleCount())
	}
	if _, ok := r.ResolvePermission("read", "document"); !ok {
		t.Error("existing permission lost")
	}
	if _, ok := r.ResolveRole("editor"); !ok {
		t.Error("existing role lost")
	}
}

// TestReloadLoop_EventDuringReloadIsAppliedAfterSwap covers the code-review-1
// race: an event read while a periodic reload is loading must not be
// overwritten by that reload's older snapshot.
func TestReloadLoop_EventDuringReloadIsAppliedAfterSwap(t *testing.T) {
	reg := NewEmpty()
	loading := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	l := &fakeLoader{
		// The first periodic snapshot predates permission (share, document); later
		// ones include it.
		snapshot: func(call int) (map[[2]string]int16, map[string]int16) {
			perms := map[[2]string]int16{{"read", "document"}: 1}
			if call > 1 {
				perms[[2]string{"share", "document"}] = 12
			}
			return perms, map[string]int16{}
		},
		block: func(call int) {
			if call == 1 {
				once.Do(func() { close(loading) })
				<-release
			}
		},
	}
	s := NewSyncer(nil, "t", reg, l, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.reloadLoop(ctx)
	}()

	<-loading
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		// What consume does for each message.
		s.applyMu.Lock()
		applyEvent(reg, []byte(`{"event":"action.id.assigned","action":"share","resource_type":"document","id":12}`))
		s.applyMu.Unlock()
	}()

	select {
	case <-applied:
		t.Fatal("event applied while a reload was in flight; the reload's swap would overwrite it")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-applied

	if id, ok := reg.ResolvePermission("share", "document"); !ok || id != 12 {
		t.Fatalf("event applied during the reload was lost: (%d, %v)", id, ok)
	}
	cancel()
	<-loopDone
}

func TestReloadLoop_ErrorsAreLoggedAndRetried(t *testing.T) {
	reg := NewEmpty()
	reg.AddPermission("read", "document", 1)
	reloaded := make(chan struct{})
	var once sync.Once
	l := &fakeLoader{permErr: errors.New("replica down")}
	l.block = func(call int) {
		if call >= 3 {
			once.Do(func() { close(reloaded) })
		}
	}
	// Fail the first two reloads, then succeed.
	l.snapshot = func(call int) (map[[2]string]int16, map[string]int16) {
		if call >= 3 {
			l.permErr = nil
		}
		return map[[2]string]int16{{"read", "document"}: 1, {"write", "document"}: 2}, map[string]int16{}
	}
	s := NewSyncer(nil, "t", reg, l, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.reloadLoop(ctx)
	}()

	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("reload loop stopped after errors")
	}
	cancel()
	<-loopDone
	if _, ok := reg.ResolvePermission("write", "document"); !ok {
		t.Fatal("successful reload after errors did not apply")
	}
}

func TestReloadLoop_StopsOnCancel(t *testing.T) {
	s := NewSyncer(nil, "t", NewEmpty(), &fakeLoader{}, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.reloadLoop(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reloadLoop did not return after cancellation")
	}
}
