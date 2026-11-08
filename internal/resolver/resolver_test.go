package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/registry"

	"github.com/google/uuid"
)

// The production types must satisfy the resolver's consumer-side interfaces.
var (
	_ checkCache = (*cache.Cache)(nil)
	_ store      = (*db.Queries)(nil)
)

// --- Fakes ---

type fakeCache struct {
	entries map[model.CheckRequest]model.CheckResult
	getErr  error
	setErr  error
	gets    int
	sets    []model.CheckResult
	ttls    []time.Duration
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[model.CheckRequest]model.CheckResult)}
}

func (c *fakeCache) Get(_ context.Context, req model.CheckRequest) (*model.CheckResult, error) {
	c.gets++
	if c.getErr != nil {
		return nil, c.getErr
	}
	r, ok := c.entries[req]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (c *fakeCache) Set(_ context.Context, req model.CheckRequest, result model.CheckResult, ttl time.Duration) error {
	c.sets = append(c.sets, result)
	c.ttls = append(c.ttls, ttl)
	if c.setErr != nil {
		return c.setErr
	}
	c.entries[req] = result
	return nil
}

type resourcesCall struct {
	userID       uuid.UUID
	permissionID int16
	resourceType string
	scopeFilter  map[string]string
}

type listCall struct {
	userID                   uuid.UUID
	resourceType, resourceID string
}

type fakeStore struct {
	overrideEffect  *model.Effect
	overrideExpires *time.Time
	overrideErr     error
	roleName        string
	roleExpires     *time.Time
	roleErr         error

	resourceIDs  []string
	resourcesErr error

	grants      []model.RoleGrant
	grantsErr   error
	overrides   []model.OverrideGrant
	overrideRow error

	overrideCalls  int
	roleCalls      int
	resourcesCalls []resourcesCall
	grantCalls     []listCall
	overrideLists  []listCall
}

func (s *fakeStore) CheckOverride(_ context.Context, _ uuid.UUID, _ int16, _ string) (*model.Effect, *time.Time, error) {
	s.overrideCalls++
	return s.overrideEffect, s.overrideExpires, s.overrideErr
}

func (s *fakeStore) CheckRole(_ context.Context, _ uuid.UUID, _ int16, _, _ string) (string, *time.Time, error) {
	s.roleCalls++
	return s.roleName, s.roleExpires, s.roleErr
}

func (s *fakeStore) Resources(_ context.Context, userID uuid.UUID, permissionID int16, resourceType string, scopeFilter map[string]string) ([]string, error) {
	s.resourcesCalls = append(s.resourcesCalls, resourcesCall{userID, permissionID, resourceType, scopeFilter})
	return s.resourceIDs, s.resourcesErr
}

func (s *fakeStore) UserRoleGrants(_ context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.RoleGrant, error) {
	s.grantCalls = append(s.grantCalls, listCall{userID, resourceType, resourceID})
	return s.grants, s.grantsErr
}

func (s *fakeStore) UserOverrides(_ context.Context, userID uuid.UUID, resourceType, resourceID string) ([]model.OverrideGrant, error) {
	s.overrideLists = append(s.overrideLists, listCall{userID, resourceType, resourceID})
	return s.overrides, s.overrideRow
}

// --- Helpers ---

var testUser = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func makeReq() model.CheckRequest {
	return model.CheckRequest{
		UserID:       testUser,
		Action:       "read",
		ResourceType: "document",
		ResourceID:   "doc:123",
	}
}

func newRegistry() *registry.Registry {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)
	return reg
}

func effect(e model.Effect) *model.Effect { return &e }

// testTTL is the resolver's configured (maximum) cache TTL in tests.
const testTTL = 5 * time.Minute

func expiresIn(d time.Duration) *time.Time {
	t := time.Now().Add(d)
	return &t
}

// --- Check ---

func TestCheck_UnknownPermission(t *testing.T) {
	c := newFakeCache()
	s := &fakeStore{}
	res := NewResolver(c, s, registry.NewEmpty(), testTTL)

	_, err := res.Check(context.Background(), makeReq())
	if !errors.Is(err, model.ErrUnknownPermission) {
		t.Fatalf("expected ErrUnknownPermission, got %v", err)
	}
	if c.gets != 0 || s.overrideCalls != 0 || s.roleCalls != 0 {
		t.Fatalf("unknown permission must not touch cache or DB: gets=%d override=%d role=%d", c.gets, s.overrideCalls, s.roleCalls)
	}
}

func TestCheck_CacheHit(t *testing.T) {
	c := newFakeCache()
	req := makeReq()
	c.entries[req] = model.CheckResult{Allowed: true, Source: "role:cached"}
	s := &fakeStore{}
	res := NewResolver(c, s, newRegistry(), testTTL)

	got, err := res.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (model.CheckResult{Allowed: true, Source: "role:cached"}) {
		t.Fatalf("expected cached result, got %+v", got)
	}
	if s.overrideCalls != 0 || s.roleCalls != 0 {
		t.Fatalf("cache hit must not query the DB")
	}
}

func TestCheck_Resolution(t *testing.T) {
	tests := []struct {
		name      string
		store     *fakeStore
		want      model.CheckResult
		roleCalls int
	}{
		{
			name:      "deny override",
			store:     &fakeStore{overrideEffect: effect(model.EffectDeny)},
			want:      model.CheckResult{Allowed: false, Source: "deny_override"},
			roleCalls: 0,
		},
		{
			name:      "allow override",
			store:     &fakeStore{overrideEffect: effect(model.EffectAllow)},
			want:      model.CheckResult{Allowed: true, Source: "allow_override"},
			roleCalls: 0,
		},
		{
			name:      "deny override beats role",
			store:     &fakeStore{overrideEffect: effect(model.EffectDeny), roleName: "editor"},
			want:      model.CheckResult{Allowed: false, Source: "deny_override"},
			roleCalls: 0,
		},
		{
			name:      "role grant",
			store:     &fakeStore{roleName: "editor"},
			want:      model.CheckResult{Allowed: true, Source: "role:editor"},
			roleCalls: 1,
		},
		{
			name:      "default deny",
			store:     &fakeStore{},
			want:      model.CheckResult{Allowed: false, Source: "default_deny"},
			roleCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCache()
			res := NewResolver(c, tt.store, newRegistry(), testTTL)

			got, err := res.Check(context.Background(), makeReq())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if tt.store.roleCalls != tt.roleCalls {
				t.Fatalf("CheckRole calls = %d, want %d", tt.store.roleCalls, tt.roleCalls)
			}
			if len(c.sets) != 1 || c.sets[0] != tt.want {
				t.Fatalf("expected the result to be cached once, got %+v", c.sets)
			}
		})
	}
}

// TestCheck_CacheTTL covers H1: a cached result never outlives the grant that
// decided it, and results with under a second left are not cached.
func TestCheck_CacheTTL(t *testing.T) {
	tests := []struct {
		name   string
		store  *fakeStore
		want   model.CheckResult
		cached bool
		ttlMin time.Duration // inclusive bounds on the TTL passed to Set
		ttlMax time.Duration
	}{
		{
			name:   "deny override without expiry uses the configured TTL",
			store:  &fakeStore{overrideEffect: effect(model.EffectDeny)},
			want:   model.CheckResult{Allowed: false, Source: "deny_override"},
			cached: true, ttlMin: testTTL, ttlMax: testTTL,
		},
		{
			name:   "allow override expiring in 1m is capped",
			store:  &fakeStore{overrideEffect: effect(model.EffectAllow), overrideExpires: expiresIn(time.Minute)},
			want:   model.CheckResult{Allowed: true, Source: "allow_override"},
			cached: true, ttlMin: 55 * time.Second, ttlMax: time.Minute,
		},
		{
			name:   "deny override expiring in 30s is capped",
			store:  &fakeStore{overrideEffect: effect(model.EffectDeny), overrideExpires: expiresIn(30 * time.Second)},
			want:   model.CheckResult{Allowed: false, Source: "deny_override"},
			cached: true, ttlMin: 25 * time.Second, ttlMax: 30 * time.Second,
		},
		{
			name:   "override expiring after the TTL keeps the TTL",
			store:  &fakeStore{overrideEffect: effect(model.EffectAllow), overrideExpires: expiresIn(time.Hour)},
			want:   model.CheckResult{Allowed: true, Source: "allow_override"},
			cached: true, ttlMin: testTTL, ttlMax: testTTL,
		},
		{
			name:   "override with under 1s left is not cached",
			store:  &fakeStore{overrideEffect: effect(model.EffectAllow), overrideExpires: expiresIn(900 * time.Millisecond)},
			want:   model.CheckResult{Allowed: true, Source: "allow_override"},
			cached: false,
		},
		{
			name:   "role without expiry uses the configured TTL",
			store:  &fakeStore{roleName: "editor"},
			want:   model.CheckResult{Allowed: true, Source: "role:editor"},
			cached: true, ttlMin: testTTL, ttlMax: testTTL,
		},
		{
			name:   "role expiring in 3s is capped",
			store:  &fakeStore{roleName: "editor", roleExpires: expiresIn(3 * time.Second)},
			want:   model.CheckResult{Allowed: true, Source: "role:editor"},
			cached: true, ttlMin: 2 * time.Second, ttlMax: 3 * time.Second,
		},
		{
			name:   "role with under 1s left is not cached",
			store:  &fakeStore{roleName: "editor", roleExpires: expiresIn(500 * time.Millisecond)},
			want:   model.CheckResult{Allowed: true, Source: "role:editor"},
			cached: false,
		},
		{
			name:   "role that has just expired is not cached",
			store:  &fakeStore{roleName: "editor", roleExpires: expiresIn(-time.Second)},
			want:   model.CheckResult{Allowed: true, Source: "role:editor"},
			cached: false,
		},
		{
			name:   "default deny is not capped",
			store:  &fakeStore{},
			want:   model.CheckResult{Allowed: false, Source: "default_deny"},
			cached: true, ttlMin: testTTL, ttlMax: testTTL,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCache()
			res := NewResolver(c, tt.store, newRegistry(), testTTL)

			got, err := res.Check(context.Background(), makeReq())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if !tt.cached {
				if len(c.sets) != 0 {
					t.Fatalf("result must not be cached, got Set with ttl %v", c.ttls)
				}
				return
			}
			if len(c.sets) != 1 {
				t.Fatalf("expected one cache write, got %d", len(c.sets))
			}
			if ttl := c.ttls[0]; ttl < tt.ttlMin || ttl > tt.ttlMax {
				t.Fatalf("ttl = %v, want in [%v, %v]", ttl, tt.ttlMin, tt.ttlMax)
			}
		})
	}
}

func TestCheck_SubSecondConfiguredTTLDisablesCaching(t *testing.T) {
	c := newFakeCache()
	res := NewResolver(c, &fakeStore{}, newRegistry(), 0)

	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (model.CheckResult{Allowed: false, Source: "default_deny"}) {
		t.Fatalf("got %+v", got)
	}
	if len(c.sets) != 0 {
		t.Fatalf("a zero TTL must not be written (Redis would keep it forever), got ttls %v", c.ttls)
	}
}

func TestCheck_CacheGetErrorFallsThroughToDB(t *testing.T) {
	c := newFakeCache()
	c.getErr = errors.New("redis down")
	s := &fakeStore{roleName: "viewer"}
	res := NewResolver(c, s, newRegistry(), testTTL)

	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("cache Get error must not fail the check: %v", err)
	}
	if got != (model.CheckResult{Allowed: true, Source: "role:viewer"}) {
		t.Fatalf("got %+v", got)
	}
	if s.overrideCalls != 1 || s.roleCalls != 1 {
		t.Fatalf("expected DB lookups after cache error, override=%d role=%d", s.overrideCalls, s.roleCalls)
	}
}

func TestCheck_CacheSetErrorIgnored(t *testing.T) {
	c := newFakeCache()
	c.setErr = errors.New("redis down")
	res := NewResolver(c, &fakeStore{overrideEffect: effect(model.EffectAllow)}, newRegistry(), testTTL)

	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("cache Set error must be ignored: %v", err)
	}
	if got != (model.CheckResult{Allowed: true, Source: "allow_override"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestCheck_DBErrorsReturned(t *testing.T) {
	dbErr := errors.New("db down")
	tests := []struct {
		name  string
		store *fakeStore
	}{
		{"override lookup", &fakeStore{overrideErr: dbErr}},
		{"role lookup", &fakeStore{roleErr: dbErr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCache()
			res := NewResolver(c, tt.store, newRegistry(), testTTL)

			got, err := res.Check(context.Background(), makeReq())
			if !errors.Is(err, dbErr) {
				t.Fatalf("expected DB error, got %v", err)
			}
			if got != (model.CheckResult{}) {
				t.Fatalf("expected zero result on error, got %+v", got)
			}
			if len(c.sets) != 0 {
				t.Fatalf("errors must not be cached, got %+v", c.sets)
			}
		})
	}
}

// --- List ---

func TestList_Merge(t *testing.T) {
	scopeA := map[string]string{"team": "a"}
	scopeB := map[string]string{"team": "b"}
	tests := []struct {
		name      string
		grants    []model.RoleGrant
		overrides []model.OverrideGrant
		want      []model.PermissionEntry
	}{
		{
			name: "empty result is an empty slice",
			want: []model.PermissionEntry{},
		},
		{
			name: "role grant",
			grants: []model.RoleGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeA, RoleName: "viewer"},
			},
			want: []model.PermissionEntry{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeA, Source: "role:viewer"},
			},
		},
		{
			name: "deny override excludes a role grant",
			grants: []model.RoleGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", RoleName: "viewer"},
				{Action: "write", ResourceType: "document", ResourceID: "d1", RoleName: "viewer"},
			},
			overrides: []model.OverrideGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Effect: model.EffectDeny},
			},
			want: []model.PermissionEntry{
				{Action: "write", ResourceType: "document", ResourceID: "d1", Source: "role:viewer"},
			},
		},
		{
			name: "deny override excludes an allow override on the same key",
			overrides: []model.OverrideGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Effect: model.EffectAllow},
				{Action: "read", ResourceType: "document", ResourceID: "d1", Effect: model.EffectDeny},
			},
			want: []model.PermissionEntry{},
		},
		{
			name: "deny only applies to its own resource",
			grants: []model.RoleGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", RoleName: "viewer"},
				{Action: "read", ResourceType: "document", ResourceID: "d2", RoleName: "viewer"},
			},
			overrides: []model.OverrideGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Effect: model.EffectDeny},
			},
			want: []model.PermissionEntry{
				{Action: "read", ResourceType: "document", ResourceID: "d2", Source: "role:viewer"},
			},
		},
		{
			name: "allow override beats a role and carries its own scope",
			grants: []model.RoleGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeA, RoleName: "viewer"},
			},
			overrides: []model.OverrideGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeB, Effect: model.EffectAllow},
			},
			want: []model.PermissionEntry{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeB, Source: "allow_override"},
			},
		},
		{
			name: "allow override beats a role listed before it is seen",
			overrides: []model.OverrideGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Effect: model.EffectAllow},
			},
			grants: []model.RoleGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", RoleName: "aaa"},
			},
			want: []model.PermissionEntry{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Source: "allow_override"},
			},
		},
		{
			name: "several roles pick the lexicographically smallest name and its scope",
			grants: []model.RoleGrant{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeA, RoleName: "viewer"},
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeB, RoleName: "editor"},
				{Action: "read", ResourceType: "document", ResourceID: "d1", RoleName: "owner"},
			},
			want: []model.PermissionEntry{
				{Action: "read", ResourceType: "document", ResourceID: "d1", Scope: scopeB, Source: "role:editor"},
			},
		},
		{
			name: "sorted by resource_type, resource_id, action",
			grants: []model.RoleGrant{
				{Action: "write", ResourceType: "project", ResourceID: "p1", RoleName: "r"},
				{Action: "read", ResourceType: "project", ResourceID: "p1", RoleName: "r"},
				{Action: "read", ResourceType: "document", ResourceID: "d2", RoleName: "r"},
				{Action: "write", ResourceType: "document", ResourceID: "d1", RoleName: "r"},
			},
			overrides: []model.OverrideGrant{
				{Action: "admin", ResourceType: "document", ResourceID: "d1", Effect: model.EffectAllow},
			},
			want: []model.PermissionEntry{
				{Action: "admin", ResourceType: "document", ResourceID: "d1", Source: "allow_override"},
				{Action: "write", ResourceType: "document", ResourceID: "d1", Source: "role:r"},
				{Action: "read", ResourceType: "document", ResourceID: "d2", Source: "role:r"},
				{Action: "read", ResourceType: "project", ResourceID: "p1", Source: "role:r"},
				{Action: "write", ResourceType: "project", ResourceID: "p1", Source: "role:r"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeStore{grants: tt.grants, overrides: tt.overrides}
			res := NewResolver(newFakeCache(), s, newRegistry(), testTTL)

			got, err := res.List(context.Background(), testUser, "", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("List must never return nil")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestList_EmptyResultSerializesAsArray(t *testing.T) {
	res := NewResolver(newFakeCache(), &fakeStore{}, newRegistry(), testTTL)
	got, err := res.List(context.Background(), testUser, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Fatalf("expected [], got %s", b)
	}
}

func TestList_PassesFilters(t *testing.T) {
	s := &fakeStore{}
	res := NewResolver(newFakeCache(), s, newRegistry(), testTTL)

	if _, err := res.List(context.Background(), testUser, "document", "d1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []listCall{{testUser, "document", "d1"}}
	if !reflect.DeepEqual(s.grantCalls, want) || !reflect.DeepEqual(s.overrideLists, want) {
		t.Fatalf("filters not passed through: grants=%+v overrides=%+v", s.grantCalls, s.overrideLists)
	}
}

func TestList_StoreErrorsReturned(t *testing.T) {
	dbErr := errors.New("db down")
	tests := []struct {
		name  string
		store *fakeStore
	}{
		{"role grants", &fakeStore{grantsErr: dbErr}},
		{"overrides", &fakeStore{overrideRow: dbErr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := NewResolver(newFakeCache(), tt.store, newRegistry(), testTTL)
			got, err := res.List(context.Background(), testUser, "", "")
			if !errors.Is(err, dbErr) {
				t.Fatalf("expected wrapped DB error, got %v", err)
			}
			if got != nil {
				t.Fatalf("expected nil entries on error, got %+v", got)
			}
		})
	}
}

// --- Resources / Filter ---

func TestResources(t *testing.T) {
	t.Run("unknown permission", func(t *testing.T) {
		s := &fakeStore{}
		res := NewResolver(newFakeCache(), s, registry.NewEmpty(), testTTL)
		_, err := res.Resources(context.Background(), model.ResourcesRequest{UserID: testUser, Action: "read", ResourceType: "document"})
		if !errors.Is(err, model.ErrUnknownPermission) {
			t.Fatalf("expected ErrUnknownPermission, got %v", err)
		}
		if len(s.resourcesCalls) != 0 {
			t.Fatal("unknown permission must not query the DB")
		}
	})

	t.Run("nil becomes empty slice", func(t *testing.T) {
		res := NewResolver(newFakeCache(), &fakeStore{}, newRegistry(), testTTL)
		got, err := res.Resources(context.Background(), model.ResourcesRequest{UserID: testUser, Action: "read", ResourceType: "document"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ResourceIDs == nil || len(got.ResourceIDs) != 0 {
			t.Fatalf("expected empty non-nil slice, got %#v", got.ResourceIDs)
		}
	})

	t.Run("passes permission id and scope filter", func(t *testing.T) {
		s := &fakeStore{resourceIDs: []string{"d1", "d2"}}
		res := NewResolver(newFakeCache(), s, newRegistry(), testTTL)
		scope := map[string]string{"team": "a"}
		got, err := res.Resources(context.Background(), model.ResourcesRequest{UserID: testUser, Action: "read", ResourceType: "document", ScopeFilter: scope})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(got.ResourceIDs, []string{"d1", "d2"}) {
			t.Fatalf("got %v", got.ResourceIDs)
		}
		want := []resourcesCall{{testUser, 1, "document", scope}}
		if !reflect.DeepEqual(s.resourcesCalls, want) {
			t.Fatalf("store call = %+v, want %+v", s.resourcesCalls, want)
		}
	})

	t.Run("db error", func(t *testing.T) {
		dbErr := errors.New("db down")
		res := NewResolver(newFakeCache(), &fakeStore{resourcesErr: dbErr}, newRegistry(), testTTL)
		_, err := res.Resources(context.Background(), model.ResourcesRequest{UserID: testUser, Action: "read", ResourceType: "document"})
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected DB error, got %v", err)
		}
	})
}

func TestFilter(t *testing.T) {
	t.Run("unknown permission", func(t *testing.T) {
		res := NewResolver(newFakeCache(), &fakeStore{}, registry.NewEmpty(), testTTL)
		_, err := res.Filter(context.Background(), testUser, "read", "document", []string{"d1"})
		if !errors.Is(err, model.ErrUnknownPermission) {
			t.Fatalf("expected ErrUnknownPermission, got %v", err)
		}
	})

	t.Run("intersection keeps candidate order", func(t *testing.T) {
		s := &fakeStore{resourceIDs: []string{"d3", "d1", "d9"}}
		res := NewResolver(newFakeCache(), s, newRegistry(), testTTL)
		got, err := res.Filter(context.Background(), testUser, "read", "document", []string{"d1", "d2", "d3"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(got, []string{"d1", "d3"}) {
			t.Fatalf("got %v, want [d1 d3]", got)
		}
		if len(s.resourcesCalls) != 1 || s.resourcesCalls[0].scopeFilter != nil {
			t.Fatalf("expected one unscoped Resources call, got %+v", s.resourcesCalls)
		}
	})

	t.Run("no match is an empty slice", func(t *testing.T) {
		res := NewResolver(newFakeCache(), &fakeStore{resourceIDs: []string{"d9"}}, newRegistry(), testTTL)
		got, err := res.Filter(context.Background(), testUser, "read", "document", []string{"d1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("expected empty non-nil slice, got %#v", got)
		}
	})

	t.Run("db error", func(t *testing.T) {
		dbErr := errors.New("db down")
		res := NewResolver(newFakeCache(), &fakeStore{resourcesErr: dbErr}, newRegistry(), testTTL)
		_, err := res.Filter(context.Background(), testUser, "read", "document", []string{"d1"})
		if !errors.Is(err, dbErr) {
			t.Fatalf("expected DB error, got %v", err)
		}
	})
}
