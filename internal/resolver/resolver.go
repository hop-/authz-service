package resolver

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"authz-service/internal/model"
	"authz-service/internal/registry"

	"github.com/google/uuid"
)

// checkCache caches single check results.
type checkCache interface {
	Get(ctx context.Context, req model.CheckRequest) (*model.CheckResult, error)
	Set(ctx context.Context, req model.CheckRequest, result model.CheckResult) error
}

// store is the resolver's read-only view of the database.
type store interface {
	CheckOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) (*model.Effect, error)
	CheckRole(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType, resourceID string) (string, error)
	Resources(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType string, scopeFilter map[string]string) ([]string, error)
	UserRoleGrants(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.RoleGrant, error)
	UserOverrides(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.OverrideGrant, error)
}

type Resolver struct {
	cache    checkCache
	db       store
	registry *registry.Registry
}

func NewResolver(c checkCache, s store, reg *registry.Registry) *Resolver {
	return &Resolver{cache: c, db: s, registry: reg}
}

// Check evaluates a single permission request.
// Returns ErrUnknownPermission if (action, resource_type) is not in the registry.
func (r *Resolver) Check(ctx context.Context, req model.CheckRequest) (model.CheckResult, error) {
	// Step 0: registry resolve
	permID, ok := r.registry.ResolvePermission(req.Action, req.ResourceType)
	if !ok {
		return model.CheckResult{}, model.ErrUnknownPermission
	}

	// Step 1: cache lookup
	if hit, err := r.cache.Get(ctx, req); err == nil && hit != nil {
		return *hit, nil
	}

	var result model.CheckResult

	// Step 2 & 3: override check
	effect, err := r.db.CheckOverride(ctx, req.UserID, permID, req.ResourceID)
	if err != nil {
		return model.CheckResult{}, err
	}
	if effect != nil {
		switch *effect {
		case model.EffectDeny:
			result = model.CheckResult{Allowed: false, Source: "deny_override"}
			_ = r.cache.Set(ctx, req, result)
			return result, nil
		case model.EffectAllow:
			result = model.CheckResult{Allowed: true, Source: "allow_override"}
			_ = r.cache.Set(ctx, req, result)
			return result, nil
		}
	}

	// Step 4: role expansion
	roleName, err := r.db.CheckRole(ctx, req.UserID, permID, req.ResourceType, req.ResourceID)
	if err != nil {
		return model.CheckResult{}, err
	}
	if roleName != "" {
		result = model.CheckResult{Allowed: true, Source: "role:" + roleName}
		_ = r.cache.Set(ctx, req, result)
		return result, nil
	}

	// Step 5: default deny
	result = model.CheckResult{Allowed: false, Source: "default_deny"}
	_ = r.cache.Set(ctx, req, result)
	return result, nil
}

// List returns the user's effective permissions, the same set Check would
// allow, optionally filtered by resource type, or by resource type and id.
//
// Entries are keyed by (action, resource_type, resource_id):
//  1. an active DENY override excludes the key;
//  2. otherwise an ALLOW override yields source "allow_override" with the override's scope;
//  3. otherwise a role grant yields source "role:<name>", the lexicographically
//     smallest role name winning when several roles grant the key.
//
// The result is sorted by resource_type, resource_id, action, and is never nil.
func (r *Resolver) List(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.PermissionEntry, error) {
	grants, err := r.db.UserRoleGrants(ctx, userID, resourceType, resourceID)
	if err != nil {
		return nil, fmt.Errorf("list role grants: %w", err)
	}
	overrides, err := r.db.UserOverrides(ctx, userID, resourceType, resourceID)
	if err != nil {
		return nil, fmt.Errorf("list overrides: %w", err)
	}
	return mergePermissions(grants, overrides), nil
}

type permissionKey struct {
	action, resourceType, resourceID string
}

func mergePermissions(grants []model.RoleGrant, overrides []model.OverrideGrant) []model.PermissionEntry {
	denied := make(map[permissionKey]struct{})
	merged := make(map[permissionKey]model.PermissionEntry)

	for _, o := range overrides {
		k := permissionKey{o.Action, o.ResourceType, o.ResourceID}
		switch o.Effect {
		case model.EffectDeny:
			denied[k] = struct{}{}
		case model.EffectAllow:
			merged[k] = model.PermissionEntry{
				Action:       o.Action,
				ResourceType: o.ResourceType,
				ResourceID:   o.ResourceID,
				Scope:        o.Scope,
				Source:       "allow_override",
			}
		}
	}

	roleOf := make(map[permissionKey]string)
	for _, g := range grants {
		k := permissionKey{g.Action, g.ResourceType, g.ResourceID}
		if e, ok := merged[k]; ok && e.Source == "allow_override" {
			continue
		}
		if prev, ok := roleOf[k]; ok && prev <= g.RoleName {
			continue
		}
		roleOf[k] = g.RoleName
		merged[k] = model.PermissionEntry{
			Action:       g.Action,
			ResourceType: g.ResourceType,
			ResourceID:   g.ResourceID,
			Scope:        g.Scope,
			Source:       "role:" + g.RoleName,
		}
	}

	entries := make([]model.PermissionEntry, 0, len(merged))
	for k, e := range merged {
		if _, ok := denied[k]; ok {
			continue
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b model.PermissionEntry) int {
		return cmp.Or(
			cmp.Compare(a.ResourceType, b.ResourceType),
			cmp.Compare(a.ResourceID, b.ResourceID),
			cmp.Compare(a.Action, b.Action),
		)
	})
	return entries
}

// Resources returns all resource IDs where user has the given action.
func (r *Resolver) Resources(ctx context.Context, req model.ResourcesRequest) (model.ResourcesResult, error) {
	permID, ok := r.registry.ResolvePermission(req.Action, req.ResourceType)
	if !ok {
		return model.ResourcesResult{}, model.ErrUnknownPermission
	}

	ids, err := r.db.Resources(ctx, req.UserID, permID, req.ResourceType, req.ScopeFilter)
	if err != nil {
		return model.ResourcesResult{}, err
	}
	if ids == nil {
		ids = []string{}
	}
	return model.ResourcesResult{ResourceIDs: ids}, nil
}

// Filter narrows a candidate list to the subset the user has the action on.
func (r *Resolver) Filter(ctx context.Context, userID uuid.UUID, action, resourceType string, candidateIDs []string) ([]string, error) {
	permID, ok := r.registry.ResolvePermission(action, resourceType)
	if !ok {
		return nil, model.ErrUnknownPermission
	}

	// Get all resource IDs the user has access to
	allIDs, err := r.db.Resources(ctx, userID, permID, resourceType, nil)
	if err != nil {
		return nil, err
	}

	// Build a set for O(1) lookups
	allowed := make(map[string]struct{}, len(allIDs))
	for _, id := range allIDs {
		allowed[id] = struct{}{}
	}

	// Filter candidates
	var result []string
	for _, id := range candidateIDs {
		if _, ok := allowed[id]; ok {
			result = append(result, id)
		}
	}
	if result == nil {
		result = []string{}
	}
	return result, nil
}
