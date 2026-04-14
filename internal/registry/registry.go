package registry

import (
	"context"
	"sync"

	"authz-service/internal/db"
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

func New(ctx context.Context, q *db.Queries) (*Registry, error) {
	r := &Registry{
		permissions: make(map[permKey]int16),
		roles:       make(map[string]int16),
		roleNames:   make(map[int16]string),
	}
	return r, r.load(ctx, q)
}

// NewEmpty creates an empty registry for testing or manual population.
func NewEmpty() *Registry {
	return &Registry{
		permissions: make(map[permKey]int16),
		roles:       make(map[string]int16),
		roleNames:   make(map[int16]string),
	}
}

func (r *Registry) load(ctx context.Context, q *db.Queries) error {
	perms, err := q.LoadPermissions(ctx)
	if err != nil {
		return err
	}
	for key, id := range perms {
		r.permissions[permKey{Action: key[0], ResourceType: key[1]}] = id
	}

	roles, err := q.LoadRoles(ctx)
	if err != nil {
		return err
	}
	for name, id := range roles {
		r.roles[name] = id
		r.roleNames[id] = name
	}
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
func (r *Registry) RemovePermission(action, resourceType string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := permKey{action, resourceType}
	id, ok := r.permissions[key]
	if ok {
		delete(r.roleNames, id)
	}
	delete(r.permissions, key)
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
