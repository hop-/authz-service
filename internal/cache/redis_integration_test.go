//go:build integration

package cache_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/model"
	"authz-service/internal/testinfra"

	"github.com/google/uuid"
)

var redisAddr string

func TestMain(m *testing.M) {
	addr, stop, err := testinfra.StartRedis(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		os.Exit(1)
	}
	redisAddr = addr
	code := m.Run()
	if err := stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop redis:", err)
	}
	os.Exit(code)
}

func newCache(t *testing.T) *cache.Cache {
	t.Helper()
	c := cache.NewCache(redisAddr)
	t.Cleanup(func() { _ = c.Close() }) // close error is irrelevant once the test is done
	return c
}

func req(user uuid.UUID, resourceType, resourceID, action string) model.CheckRequest {
	return model.CheckRequest{UserID: user, ResourceType: resourceType, ResourceID: resourceID, Action: action}
}

func TestSetGet_RespectsTTL(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	r := req(uuid.New(), "document", "d1", "read")
	result := model.CheckResult{Allowed: true, Source: "role:editor"}

	if err := c.Set(ctx, r, result, 1500*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	key := fmt.Sprintf("authz:check:%s:document:d1:read", r.UserID)
	pttl, err := c.Client().PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if pttl <= 0 || pttl > 1500*time.Millisecond {
		t.Fatalf("PTTL = %v, want in (0, 1.5s]", pttl)
	}
	got, err := c.Get(ctx, r)
	if err != nil || got == nil || *got != result {
		t.Fatalf("Get = (%v, %v), want %+v", got, err, result)
	}

	time.Sleep(2 * time.Second)
	got, err = c.Get(ctx, r)
	if err != nil || got != nil {
		t.Fatalf("Get after the TTL = (%+v, %v), want a miss", got, err)
	}
}

func TestInvalidate_RemovesOnlyItsScope(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	u1, u2 := uuid.New(), uuid.New()
	result := model.CheckResult{Allowed: true, Source: "role:editor"}
	targets := []model.CheckRequest{req(u1, "document", "d1", "read"), req(u1, "document", "d1", "write")}
	others := []model.CheckRequest{
		req(u1, "document", "d2", "read"),
		req(u1, "project", "d1", "read"),
		req(u2, "document", "d1", "read"),
	}
	for _, r := range slices.Concat(targets, others) {
		if err := c.Set(ctx, r, result, time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	if err := c.Invalidate(ctx, u1.String(), "document", "d1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	for _, r := range targets {
		if got, err := c.Get(ctx, r); err != nil || got != nil {
			t.Errorf("%+v still cached after Invalidate: (%v, %v)", r, got, err)
		}
	}
	for _, r := range others {
		if got, err := c.Get(ctx, r); err != nil || got == nil {
			t.Errorf("%+v outside the scope was removed: (%v, %v)", r, got, err)
		}
	}
}

func TestFlushAll_RemovesOnlyCheckKeys(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	rdb := c.Client()
	for _, k := range []string{"other:key", "authz:sessions:1"} {
		if err := rdb.Set(ctx, k, "keep", time.Minute).Err(); err != nil {
			t.Fatalf("SET %s: %v", k, err)
		}
	}
	var checks []model.CheckRequest
	for i := range 3 {
		r := req(uuid.New(), "document", fmt.Sprintf("d%d", i), "read")
		checks = append(checks, r)
		if err := c.Set(ctx, r, model.CheckResult{Source: "default_deny"}, time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	if err := c.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	for _, r := range checks {
		if got, err := c.Get(ctx, r); err != nil || got != nil {
			t.Errorf("check key %+v survived FlushAll", r)
		}
	}
	for _, k := range []string{"other:key", "authz:sessions:1"} {
		if v, err := rdb.Get(ctx, k).Result(); err != nil || v != "keep" {
			t.Errorf("non-check key %s removed by FlushAll: (%q, %v)", k, v, err)
		}
	}
}
