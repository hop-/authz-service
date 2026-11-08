package registry

import (
	"sync"
	"testing"
)

func TestRemovePermission_KeepsRoleNames(t *testing.T) {
	r := NewEmpty()
	// Permission and role ids come from separate sequences, so they collide.
	r.AddRole("editor", 1)
	r.AddPermission("read", "document", 1)

	r.RemovePermission("read", "document")

	if got := r.RoleName(1); got != "editor" {
		t.Fatalf("RoleName(1) = %q after RemovePermission, want %q", got, "editor")
	}
	if id, ok := r.ResolveRole("editor"); !ok || id != 1 {
		t.Fatalf("ResolveRole(editor) = (%d, %v), want (1, true)", id, ok)
	}
	if _, ok := r.ResolvePermission("read", "document"); ok {
		t.Fatal("permission still resolvable after RemovePermission")
	}
}

func TestRemovePermission_Unknown(t *testing.T) {
	r := NewEmpty()
	r.AddRole("editor", 3)
	r.AddPermission("read", "document", 3)

	r.RemovePermission("write", "document")

	if r.PermissionCount() != 1 || r.RoleName(3) != "editor" {
		t.Fatalf("removing an unknown permission changed state: perms=%d role=%q", r.PermissionCount(), r.RoleName(3))
	}
}

func TestPermissions_AddResolveRemove(t *testing.T) {
	r := NewEmpty()
	if _, ok := r.ResolvePermission("read", "document"); ok {
		t.Fatal("empty registry resolved a permission")
	}

	r.AddPermission("read", "document", 7)
	r.AddPermission("read", "project", 8)

	tests := []struct {
		action, resourceType string
		wantID               int16
		wantOK               bool
	}{
		{"read", "document", 7, true},
		{"read", "project", 8, true},
		{"write", "document", 0, false},
	}
	for _, tt := range tests {
		id, ok := r.ResolvePermission(tt.action, tt.resourceType)
		if id != tt.wantID || ok != tt.wantOK {
			t.Errorf("ResolvePermission(%s, %s) = (%d, %v), want (%d, %v)", tt.action, tt.resourceType, id, ok, tt.wantID, tt.wantOK)
		}
	}
	if r.PermissionCount() != 2 {
		t.Fatalf("PermissionCount = %d, want 2", r.PermissionCount())
	}

	// Re-adding is idempotent.
	r.AddPermission("read", "document", 7)
	if r.PermissionCount() != 2 {
		t.Fatalf("PermissionCount after re-add = %d, want 2", r.PermissionCount())
	}

	r.RemovePermission("read", "document")
	if _, ok := r.ResolvePermission("read", "document"); ok {
		t.Fatal("removed permission still resolves")
	}
	if id, ok := r.ResolvePermission("read", "project"); !ok || id != 8 {
		t.Fatal("removing one permission affected another")
	}
	if r.PermissionCount() != 1 {
		t.Fatalf("PermissionCount = %d, want 1", r.PermissionCount())
	}
}

func TestRoles_AddResolveRemove(t *testing.T) {
	r := NewEmpty()
	r.AddRole("editor", 2)
	r.AddRole("viewer", 3)

	if id, ok := r.ResolveRole("editor"); !ok || id != 2 {
		t.Fatalf("ResolveRole(editor) = (%d, %v)", id, ok)
	}
	if r.RoleName(3) != "viewer" {
		t.Fatalf("RoleName(3) = %q", r.RoleName(3))
	}
	if r.RoleCount() != 2 {
		t.Fatalf("RoleCount = %d, want 2", r.RoleCount())
	}

	r.RemoveRole("editor")
	if _, ok := r.ResolveRole("editor"); ok {
		t.Fatal("removed role still resolves")
	}
	if r.RoleName(2) != "" {
		t.Fatalf("RoleName(2) = %q after RemoveRole, want empty", r.RoleName(2))
	}
	if r.RoleName(3) != "viewer" || r.RoleCount() != 1 {
		t.Fatal("removing one role affected another")
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := NewEmpty()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(id int16) {
			defer wg.Done()
			for range 100 {
				r.AddPermission("read", "document", id)
				r.ResolvePermission("read", "document")
				r.AddRole("editor", id)
				r.RoleName(id)
				r.RemovePermission("read", "document")
			}
		}(int16(i))
	}
	wg.Wait()
}
