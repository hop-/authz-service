package registry

import (
	"context"
	"fmt"
	"sync"
)

type permKey struct {
	Action       string
	ResourceType string
}

type Registry struct {
	mu          sync.RWMutex
	permissions map[permKey]int16 // (action, resource_type) -> id
	roles       map[string]int16  // role_name -> id
	roleNames   map[int16]string  // id -> role_name (for source labels)
}

// Loader reads the full registry from the database. *db.Queries satisfies it.
type Loader interface {
	LoadPermissions(ctx context.Context) (map[[2]string]int16, error)
	LoadRoles(ctx context.Context) (map[string]int16, error)
}

// New creates a registry loaded from l.
func New(ctx context.Context, l Loader) (*Registry, error) {
	r := NewEmpty()
	if err := r.Reload(ctx, l); err != nil {
		return nil, err
	}
	return r, nil
}

// NewEmpty creates an empty registry for testing or manual population.
func NewEmpty() *Registry {
	return &Registry{
		permissions: make(map[permKey]int16),
		roles:       make(map[string]int16),
		roleNames:   make(map[int16]string),
	}
}

// Reload replaces the registry's contents with a fresh snapshot from l. The
// new maps are built first and swapped in under the lock, so readers see
// either the old or the new snapshot. On error the registry is unchanged.
func (r *Registry) Reload(ctx context.Context, l Loader) error {
	perms, err := l.LoadPermissions(ctx)
	if err != nil {
		return fmt.Errorf("load permissions: %w", err)
	}
	roles, err := l.LoadRoles(ctx)
	if err != nil {
		return fmt.Errorf("load roles: %w", err)
	}

	permissions := make(map[permKey]int16, len(perms))
	for key, id := range perms {
		permissions[permKey{Action: key[0], ResourceType: key[1]}] = id
	}
	roleIDs := make(map[string]int16, len(roles))
	roleNames := make(map[int16]string, len(roles))
	for name, id := range roles {
		roleIDs[name] = id
		roleNames[id] = name
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.permissions = permissions
	r.roles = roleIDs
	r.roleNames = roleNames
	return nil
}

// ResolvePermission returns the SMALLINT id for (action, resource_type).
// Returns (0, false) if not registered.
func (r *Registry) ResolvePermission(action, resourceType string) (int16, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.permissions[permKey{action, resourceType}]
	return id, ok
}

// ResolveRole returns the SMALLINT id for a role name.
// Returns (0, false) if not registered.
func (r *Registry) ResolveRole(name string) (int16, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.roles[name]
	return id, ok
}

// RoleName returns the human-readable name for a role id.
func (r *Registry) RoleName(id int16) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.roleNames[id]
}

// AddPermission adds a newly registered permission to the live map.
func (r *Registry) AddPermission(action, resourceType string, id int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.permissions[permKey{action, resourceType}] = id
}

// RemovePermission removes a permission from the live map.
// Permission and role ids are separate sequences, so role names are never touched.
func (r *Registry) RemovePermission(action, resourceType string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.permissions, permKey{action, resourceType})
}

// AddRole adds a role to the live map.
func (r *Registry) AddRole(name string, id int16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roles[name] = id
	r.roleNames[id] = name
}

// RemoveRole removes a role from the live map.
func (r *Registry) RemoveRole(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.roles[name]
	if ok {
		delete(r.roleNames, id)
	}
	delete(r.roles, name)
}

// PermissionCount returns the number of registered permissions.
func (r *Registry) PermissionCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.permissions)
}

// RoleCount returns the number of registered roles.
func (r *Registry) RoleCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.roles)
}
