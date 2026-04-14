package resolver

import (
	"context"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/registry"

	"github.com/google/uuid"
)

type Resolver struct {
	cache    *cache.Cache
	db       *db.Queries
	registry *registry.Registry
}

func NewResolver(c *cache.Cache, q *db.Queries, reg *registry.Registry) *Resolver {
	return &Resolver{cache: c, db: q, registry: reg}
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

// List returns all resolved permissions for a user, optionally filtered by resource.
func (r *Resolver) List(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.PermissionEntry, error) {
	return r.db.ListUserPermissions(ctx, userID, resourceType, resourceID)
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
