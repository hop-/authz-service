//go:build integration

package resolver_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/model"
	"authz-service/internal/registry"
	"authz-service/internal/resolver"
	"authz-service/internal/testinfra"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	pool      *pgxpool.Pool
	redisAddr string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	p, stopPG, err := testinfra.StartPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(1)
	}
	addr, stopRedis, err := testinfra.StartRedis(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		if err := stopPG(); err != nil {
			fmt.Fprintln(os.Stderr, "stop postgres:", err)
		}
		os.Exit(1)
	}
	pool, redisAddr = p, addr
	code := m.Run()
	for _, stop := range []func() error{stopRedis, stopPG} {
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stop container:", err)
		}
	}
	os.Exit(code)
}

const cacheTTL = 5 * time.Minute

var actor = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")

type env struct {
	q     *db.Queries
	cache *cache.Cache
	res   *resolver.Resolver
}

func newEnv(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	q := db.NewQueries(pool)
	reg, err := registry.New(ctx, q)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	c := cache.NewCache(redisAddr)
	t.Cleanup(func() { _ = c.Close() }) // close error is irrelevant once the test is done
	return env{q: q, cache: c, res: resolver.NewResolver(c, q, reg, cacheTTL)}
}

func (e env) assignRole(t *testing.T, user uuid.UUID, role string, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	rid, found, err := e.q.RoleID(ctx, role)
	if err != nil || !found {
		t.Fatalf("RoleID(%s) = (%d, %v, %v)", role, rid, found, err)
	}
	applied, err := e.q.ApplyEvent(ctx, uuid.New(), "role.assigned", func(w db.Writer) error {
		return w.InsertUserRole(ctx, user, rid, "document", "d1", nil, actor, time.Now(), expiresAt)
	})
	if err != nil || !applied {
		t.Fatalf("assign role: (%v, %v)", applied, err)
	}
}

func (e env) setOverride(t *testing.T, user uuid.UUID, effect model.Effect, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	pid, found, err := e.q.PermissionID(ctx, "read", "document")
	if err != nil || !found {
		t.Fatalf("PermissionID = (%d, %v, %v)", pid, found, err)
	}
	applied, err := e.q.ApplyEvent(ctx, uuid.New(), "override.set", func(w db.Writer) error {
		return w.UpsertOverride(ctx, user, pid, "d1", nil, effect, "test", actor, time.Now(), expiresAt)
	})
	if err != nil || !applied {
		t.Fatalf("set override: (%v, %v)", applied, err)
	}
}

func checkReq(user uuid.UUID) model.CheckRequest {
	return model.CheckRequest{UserID: user, Action: "read", ResourceType: "document", ResourceID: "d1"}
}

func (e env) cachedTTL(t *testing.T, user uuid.UUID) time.Duration {
	t.Helper()
	key := fmt.Sprintf("authz:check:%s:document:d1:read", user)
	pttl, err := e.cache.Client().PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	return pttl // -2 when the key doesn't exist
}

func in(d time.Duration) *time.Time {
	ts := time.Now().Add(d)
	return &ts
}

func TestCheck_TTLCappedAtGrantExpiry(t *testing.T) {
	t.Run("role expiring in 3s", func(t *testing.T) {
		e := newEnv(t)
		user := uuid.New()
		e.assignRole(t, user, "viewer", in(3*time.Second))

		got, err := e.res.Check(context.Background(), checkReq(user))
		if err != nil || got != (model.CheckResult{Allowed: true, Source: "role:viewer"}) {
			t.Fatalf("Check = (%+v, %v)", got, err)
		}
		if ttl := e.cachedTTL(t, user); ttl <= 0 || ttl > 3*time.Second {
			t.Fatalf("cached TTL = %v, want in (0, 3s]", ttl)
		}

		// After the grant expires, the cached allow must be gone too.
		time.Sleep(3500 * time.Millisecond)
		got, err = e.res.Check(context.Background(), checkReq(user))
		if err != nil || got.Allowed {
			t.Fatalf("Check after expiry = (%+v, %v), want denied", got, err)
		}
	})

	t.Run("override expiring in 3s", func(t *testing.T) {
		e := newEnv(t)
		user := uuid.New()
		e.setOverride(t, user, model.EffectAllow, in(3*time.Second))

		got, err := e.res.Check(context.Background(), checkReq(user))
		if err != nil || got != (model.CheckResult{Allowed: true, Source: "allow_override"}) {
			t.Fatalf("Check = (%+v, %v)", got, err)
		}
		if ttl := e.cachedTTL(t, user); ttl <= 0 || ttl > 3*time.Second {
			t.Fatalf("cached TTL = %v, want in (0, 3s]", ttl)
		}
	})

	t.Run("longest-lived role decides the TTL", func(t *testing.T) {
		e := newEnv(t)
		user := uuid.New()
		e.assignRole(t, user, "viewer", in(2*time.Second))
		e.assignRole(t, user, "editor", in(time.Minute))

		got, err := e.res.Check(context.Background(), checkReq(user))
		if err != nil || got != (model.CheckResult{Allowed: true, Source: "role:editor"}) {
			t.Fatalf("Check = (%+v, %v)", got, err)
		}
		if ttl := e.cachedTTL(t, user); ttl <= 50*time.Second || ttl > time.Minute {
			t.Fatalf("cached TTL = %v, want about 1m", ttl)
		}
	})

	t.Run("grant without expiry uses the configured TTL", func(t *testing.T) {
		e := newEnv(t)
		user := uuid.New()
		e.assignRole(t, user, "viewer", nil)

		if _, err := e.res.Check(context.Background(), checkReq(user)); err != nil {
			t.Fatalf("Check: %v", err)
		}
		if ttl := e.cachedTTL(t, user); ttl <= cacheTTL-5*time.Second || ttl > cacheTTL {
			t.Fatalf("cached TTL = %v, want about %v", ttl, cacheTTL)
		}
	})
}

func TestCheck_GrantExpiringWithinASecondNotCached(t *testing.T) {
	e := newEnv(t)
	user := uuid.New()
	e.assignRole(t, user, "viewer", in(900*time.Millisecond))

	got, err := e.res.Check(context.Background(), checkReq(user))
	if err != nil || got != (model.CheckResult{Allowed: true, Source: "role:viewer"}) {
		t.Fatalf("Check = (%+v, %v)", got, err)
	}
	if ttl := e.cachedTTL(t, user); ttl != -2 {
		t.Fatalf("a result with under 1s left was cached (PTTL %v)", ttl)
	}
}
