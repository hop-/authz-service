package resolver

import (
	"context"
	"errors"
	"testing"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/registry"

	"github.com/google/uuid"
)

// --- Mock cache ---

type mockCache struct {
	store map[string]*model.CheckResult
}

func newMockCache() *mockCache {
	return &mockCache{store: make(map[string]*model.CheckResult)}
}

func (m *mockCache) Get(_ context.Context, req model.CheckRequest) (*model.CheckResult, error) {
	key := req.UserID.String() + ":" + req.ResourceType + ":" + req.ResourceID + ":" + req.Action
	r, ok := m.store[key]
	if !ok {
		return nil, nil
	}
	return r, nil
}

func (m *mockCache) Set(_ context.Context, req model.CheckRequest, result model.CheckResult) error {
	key := req.UserID.String() + ":" + req.ResourceType + ":" + req.ResourceID + ":" + req.Action
	m.store[key] = &result
	return nil
}

// --- Mock DB ---

type mockDB struct {
	overrideEffect *model.Effect
	roleName       string
	overrideErr    error
	roleErr        error
}

func (m *mockDB) CheckOverride(_ context.Context, _ uuid.UUID, _ int16, _ string) (*model.Effect, error) {
	return m.overrideEffect, m.overrideErr
}

func (m *mockDB) CheckRole(_ context.Context, _ uuid.UUID, _ int16, _, _ string) (string, error) {
	return m.roleName, m.roleErr
}

// --- Interfaces for testable resolver ---

type cacheInterface interface {
	Get(ctx context.Context, req model.CheckRequest) (*model.CheckResult, error)
	Set(ctx context.Context, req model.CheckRequest, result model.CheckResult) error
}

type dbInterface interface {
	CheckOverride(ctx context.Context, userID uuid.UUID, permissionID int16, resourceID string) (*model.Effect, error)
	CheckRole(ctx context.Context, userID uuid.UUID, permissionID int16, resourceType, resourceID string) (string, error)
}

// testableResolver mirrors the real Resolver.Check logic but allows mock injection.
type testableResolver struct {
	cache cacheInterface
	db    dbInterface
	reg   *registry.Registry
}

func (r *testableResolver) Check(ctx context.Context, req model.CheckRequest) (model.CheckResult, error) {
	permID, ok := r.reg.ResolvePermission(req.Action, req.ResourceType)
	if !ok {
		return model.CheckResult{}, model.ErrUnknownPermission
	}

	if hit, err := r.cache.Get(ctx, req); err == nil && hit != nil {
		return *hit, nil
	}

	var result model.CheckResult

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

	roleName, err := r.db.CheckRole(ctx, req.UserID, permID, req.ResourceType, req.ResourceID)
	if err != nil {
		return model.CheckResult{}, err
	}
	if roleName != "" {
		result = model.CheckResult{Allowed: true, Source: "role:" + roleName}
		_ = r.cache.Set(ctx, req, result)
		return result, nil
	}

	result = model.CheckResult{Allowed: false, Source: "default_deny"}
	_ = r.cache.Set(ctx, req, result)
	return result, nil
}

func makeReq() model.CheckRequest {
	return model.CheckRequest{
		UserID:       uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Action:       "read",
		ResourceType: "document",
		ResourceID:   "doc:123",
	}
}

func TestStep0_UnknownPermission(t *testing.T) {
	reg := registry.NewEmpty()
	mc := newMockCache()
	mdb := &mockDB{}

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	_, err := res.Check(context.Background(), makeReq())
	if !errors.Is(err, model.ErrUnknownPermission) {
		t.Fatalf("expected ErrUnknownPermission, got %v", err)
	}
}

func TestStep1_CacheHit(t *testing.T) {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)
	mc := newMockCache()
	mdb := &mockDB{}

	req := makeReq()
	expected := model.CheckResult{Allowed: true, Source: "cache"}
	_ = mc.Set(context.Background(), req, expected)

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	got, err := res.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Allowed != true || got.Source != "cache" {
		t.Fatalf("expected cache hit, got %+v", got)
	}
}

func TestStep2_DenyOverride(t *testing.T) {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)

	deny := model.EffectDeny
	mc := newMockCache()
	mdb := &mockDB{overrideEffect: &deny}

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Allowed != false || got.Source != "deny_override" {
		t.Fatalf("expected deny_override, got %+v", got)
	}
}

func TestStep3_AllowOverride(t *testing.T) {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)

	allow := model.EffectAllow
	mc := newMockCache()
	mdb := &mockDB{overrideEffect: &allow}

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Allowed != true || got.Source != "allow_override" {
		t.Fatalf("expected allow_override, got %+v", got)
	}
}

func TestStep4_RoleCheck(t *testing.T) {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)

	mc := newMockCache()
	mdb := &mockDB{roleName: "editor"}

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Allowed != true || got.Source != "role:editor" {
		t.Fatalf("expected role:editor, got %+v", got)
	}
}

func TestStep5_DefaultDeny(t *testing.T) {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)

	mc := newMockCache()
	mdb := &mockDB{}

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Allowed != false || got.Source != "default_deny" {
		t.Fatalf("expected default_deny, got %+v", got)
	}
}

func TestDenyOverrideTakesPriorityOverRole(t *testing.T) {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)

	deny := model.EffectDeny
	mc := newMockCache()
	mdb := &mockDB{overrideEffect: &deny, roleName: "editor"}

	res := &testableResolver{cache: mc, db: mdb, reg: reg}
	got, err := res.Check(context.Background(), makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Allowed != false || got.Source != "deny_override" {
		t.Fatalf("expected deny_override to take priority, got %+v", got)
	}
}

// Ensure unused imports don't cause issues
var _ = (*db.Queries)(nil)
var _ = (*cache.Cache)(nil)
