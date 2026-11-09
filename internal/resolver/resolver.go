package resolver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"authz-service/internal/metrics"
	"authz-service/internal/model"
	"authz-service/internal/registry"

	"github.com/google/uuid"
)

// checkCache caches single check results.
type checkCache interface {
	Get(ctx context.Context, req model.CheckRequest) (*model.CheckResult, error)
	Set(ctx context.Context, req model.CheckRequest, result model.CheckResult, ttl time.Duration) error
}

// minCacheTTL is the shortest TTL worth caching. A result whose deciding grant
// expires sooner is not cached.
const minCacheTTL = time.Second

// store is the resolver's read-only view of the database.
type store interface {
	CheckOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) (*model.Effect, *time.Time, error)
	CheckRole(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType, resourceID string) (string, *time.Time, error)
	Resources(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType string, scopeFilter map[string]string) ([]string, error)
	FilterResources(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType string, candidates []string) ([]string, error)
	UserRoleGrants(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.RoleGrant, error)
	UserOverrides(ctx context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.OverrideGrant, error)
}

type Resolver struct {
	cache    checkCache
	db       store
	registry *registry.Registry
	cacheTTL time.Duration
}

// NewResolver creates a Resolver. cacheTTL is the longest a check result is
// cached; results decided by an expiring grant are cached for less.
func NewResolver(c checkCache, s store, reg *registry.Registry, cacheTTL time.Duration) *Resolver {
	return &Resolver{cache: c, db: s, registry: reg, cacheTTL: cacheTTL}
}

// Check evaluates a single permission request.
// Returns ErrUnknownPermission if (action, resource_type) is not in the registry.
//
// Each decision is counted in authz_check_total and timed in
// authz_check_duration_seconds by its source category; failed checks (DB
// errors) are not.
func (r *Resolver) Check(ctx context.Context, req model.CheckRequest) (model.CheckResult, error) {
	start := time.Now()
	result, source, err := r.check(ctx, req)
	switch {
	case errors.Is(err, model.ErrUnknownPermission):
		observeCheck(start, metrics.ResultUnknown, metrics.SourceUnknownPermission)
	case err == nil:
		observeCheck(start, checkResultLabel(result), source)
	}
	return result, err
}

func observeCheck(start time.Time, result, source string) {
	metrics.CheckTotal.WithLabelValues(result, source).Inc()
	metrics.CheckDuration.WithLabelValues(source).Observe(time.Since(start).Seconds())
}

func checkResultLabel(res model.CheckResult) string {
	if res.Allowed {
		return metrics.ResultAllowed
	}
	return metrics.ResultDenied
}

// check is Check without the metrics. It also returns the decision's source
// category for the metric labels.
func (r *Resolver) check(ctx context.Context, req model.CheckRequest) (model.CheckResult, string, error) {
	// Step 0: registry resolve
	permID, ok := r.registry.ResolvePermission(req.Action, req.ResourceType)
	if !ok {
		return model.CheckResult{}, "", model.ErrUnknownPermission
	}

	// Step 1: cache lookup. A cache error counts as a miss.
	if hit, err := r.cache.Get(ctx, req); err == nil && hit != nil {
		metrics.CacheHitTotal.Inc()
		return *hit, metrics.SourceCache, nil
	}
	metrics.CacheMissTotal.Inc()

	var result model.CheckResult

	// Step 2 & 3: override check
	effect, overrideExpires, err := r.db.CheckOverride(ctx, req.UserID, permID, req.ResourceID)
	if err != nil {
		return model.CheckResult{}, "", err
	}
	if effect != nil {
		switch *effect {
		case model.EffectDeny:
			result = model.CheckResult{Allowed: false, Source: "deny_override"}
			r.cacheResult(ctx, req, result, r.ttlUntil(overrideExpires))
			return result, metrics.SourceDenyOverride, nil
		case model.EffectAllow:
			result = model.CheckResult{Allowed: true, Source: "allow_override"}
			r.cacheResult(ctx, req, result, r.ttlUntil(overrideExpires))
			return result, metrics.SourceAllowOverride, nil
		}
	}

	// Step 4: role expansion
	roleName, roleExpires, err := r.db.CheckRole(ctx, req.UserID, permID, req.ResourceType, req.ResourceID)
	if err != nil {
		return model.CheckResult{}, "", err
	}
	if roleName != "" {
		result = model.CheckResult{Allowed: true, Source: "role:" + roleName}
		r.cacheResult(ctx, req, result, r.ttlUntil(roleExpires))
		return result, metrics.SourceRole, nil
	}

	// Step 5: default deny (no grant decides it, so the TTL is not capped)
	result = model.CheckResult{Allowed: false, Source: "default_deny"}
	r.cacheResult(ctx, req, result, r.cacheTTL)
	return result, metrics.SourceDefaultDeny, nil
}

// ttlUntil caps the cache TTL at the remaining lifetime of the deciding grant;
// expiresAt nil means the grant never expires.
func (r *Resolver) ttlUntil(expiresAt *time.Time) time.Duration {
	if expiresAt == nil {
		return r.cacheTTL
	}
	return min(r.cacheTTL, time.Until(*expiresAt))
}

// cacheResult caches a check result for ttl, or not at all when ttl is under
// minCacheTTL. A failed cache write only costs a later cache miss, so its
// error is ignored.
//
// Known limitations (deferred, see README "Known limitations"): C6, a check
// that read a lagging replica right after a change can cache the pre-change
// result for up to the TTL; H7, invalidation scans all cached keys.
func (r *Resolver) cacheResult(ctx context.Context, req model.CheckRequest, result model.CheckResult, ttl time.Duration) {
	if ttl < minCacheTTL {
		return
	}
	_ = r.cache.Set(ctx, req, result, ttl)
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
// The result is sorted by resource_type, resource_id, action, and is never
// nil; an entry's scope is never nil either ({} when the grant has none).
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
		// An unscoped grant serializes as "scope": {}, not null.
		if e.Scope == nil {
			e.Scope = map[string]string{}
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
// The database checks only the candidates (db.FilterResources), and the
// result keeps the candidates' order (duplicates included). It is never nil.
func (r *Resolver) Filter(ctx context.Context, userID uuid.UUID, action, resourceType string, candidateIDs []string) ([]string, error) {
	permID, ok := r.registry.ResolvePermission(action, resourceType)
	if !ok {
		return nil, model.ErrUnknownPermission
	}
	if len(candidateIDs) == 0 {
		return []string{}, nil
	}

	allowedIDs, err := r.db.FilterResources(ctx, userID, permID, resourceType, candidateIDs)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(allowedIDs))
	for _, id := range allowedIDs {
		allowed[id] = struct{}{}
	}

	result := make([]string, 0, len(allowedIDs))
	for _, id := range candidateIDs {
		if _, ok := allowed[id]; ok {
			result = append(result, id)
		}
	}
	return result, nil
}
