//go:build integration

package cache_test

import (
	"context"
	"testing"
	"time"

	"authz-service/internal/model"

	"github.com/google/uuid"
)

// AC-8: names and ids containing ':' and glob characters neither collide nor
// invalidate outside their own (user, resource_type, resource_id) scope.
func TestInvalidate_NoCrossScopeWithColonsAndGlobs(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	user := uuid.New()
	other := uuid.New()

	all := []model.CheckRequest{
		req(user, "document", "doc:1*", "read"),
		req(user, "document", "doc:1*", "boards:write"),
		req(user, "document", "doc:1", "read"),
		req(user, "document", "doc:1x", "read"),
		req(user, "document", "doc:1*:read", "x"),
		req(user, "document", `doc\:1*`, "read"),
		req(user, "document", "doc?1*", "read"),
		req(user, "document", "[d]oc:1*", "read"),
		req(user, "document:doc", "1*", "read"),
		req(user, "doc*", "doc:1*", "read"),
		req(other, "document", "doc:1*", "read"),
	}
	// Each entry gets a distinct result, so a collision would show as a wrong Get.
	for i, r := range all {
		if err := c.Set(ctx, r, model.CheckResult{Allowed: i%2 == 0, Source: r.ResourceType + "|" + r.ResourceID + "|" + r.Action}, time.Minute); err != nil {
			t.Fatalf("Set %+v: %v", r, err)
		}
	}
	for i, r := range all {
		got, err := c.Get(ctx, r)
		want := model.CheckResult{Allowed: i%2 == 0, Source: r.ResourceType + "|" + r.ResourceID + "|" + r.Action}
		if err != nil || got == nil || *got != want {
			t.Fatalf("Get %+v = (%v, %v), want %+v: keys collide", r, got, err, want)
		}
	}

	if err := c.Invalidate(ctx, user.String(), "document", "doc:1*"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	for i, r := range all {
		got, err := c.Get(ctx, r)
		if err != nil {
			t.Fatalf("Get %+v: %v", r, err)
		}
		inScope := i < 2 // both actions on (user, document, doc:1*)
		if inScope && got != nil {
			t.Errorf("%+v survived the invalidation of its own scope", r)
		}
		if !inScope && got == nil {
			t.Errorf("%+v was invalidated by (document, doc:1*): cross-scope invalidation", r)
		}
	}
}

// AC-8: an id made only of glob characters invalidates only itself.
func TestInvalidate_GlobOnlyID(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	user := uuid.New()
	star := req(user, "document", "*", "read")
	plain := req(user, "document", "d1", "read")
	for _, r := range []model.CheckRequest{star, plain} {
		if err := c.Set(ctx, r, model.CheckResult{Allowed: true, Source: "x"}, time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	if err := c.Invalidate(ctx, user.String(), "document", "*"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	if got, _ := c.Get(ctx, star); got != nil {
		t.Error(`the "*" entry survived its own invalidation`)
	}
	if got, err := c.Get(ctx, plain); err != nil || got == nil {
		t.Errorf(`invalidating id "*" removed d1 (Get = %v, %v)`, got, err)
	}
}

// FlushAll still removes every check entry, escaped or not.
func TestFlushAll_RemovesEscapedKeys(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	r := req(uuid.New(), "a:b", `c\d*`, "e:f")
	if err := c.Set(ctx, r, model.CheckResult{Allowed: true}, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if got, err := c.Get(ctx, r); err != nil || got != nil {
		t.Fatalf("Get after FlushAll = (%v, %v), want a miss", got, err)
	}
}
