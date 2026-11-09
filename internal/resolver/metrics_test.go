package resolver

import (
	"context"
	"errors"
	"testing"

	"authz-service/internal/metrics"
	"authz-service/internal/metrics/metricstest"
	"authz-service/internal/model"
	"authz-service/internal/registry"
)

// checkSnapshot is the check metrics for one (result, source) pair.
type checkSnapshot struct {
	total, durations, hits, misses float64
}

func snapshotCheck(t *testing.T, result, source string) checkSnapshot {
	t.Helper()
	return checkSnapshot{
		total:     metricstest.Value(t, "authz_check_total", map[string]string{"result": result, "source": source}),
		durations: metricstest.Value(t, "authz_check_duration_seconds", map[string]string{"source": source}),
		hits:      metricstest.Value(t, "authz_cache_hit_total", nil),
		misses:    metricstest.Value(t, "authz_cache_miss_total", nil),
	}
}

// AC-1: each decision is counted once by result and source category, timed by
// source, and counted as a cache hit or miss.
func TestCheck_Metrics(t *testing.T) {
	tests := []struct {
		name     string
		cached   *model.CheckResult
		store    *fakeStore
		result   string
		source   string
		cacheHit bool
	}{
		{
			name:     "cache hit",
			cached:   &model.CheckResult{Allowed: true, Source: "role:editor"},
			store:    &fakeStore{},
			result:   metrics.ResultAllowed,
			source:   metrics.SourceCache,
			cacheHit: true,
		},
		{
			name:     "cached denial",
			cached:   &model.CheckResult{Allowed: false, Source: "default_deny"},
			store:    &fakeStore{},
			result:   metrics.ResultDenied,
			source:   metrics.SourceCache,
			cacheHit: true,
		},
		{
			name:   "deny override",
			store:  &fakeStore{overrideEffect: effect(model.EffectDeny)},
			result: metrics.ResultDenied,
			source: metrics.SourceDenyOverride,
		},
		{
			name:   "allow override",
			store:  &fakeStore{overrideEffect: effect(model.EffectAllow)},
			result: metrics.ResultAllowed,
			source: metrics.SourceAllowOverride,
		},
		{
			name:   "role, labelled by category not role name",
			store:  &fakeStore{roleName: "editor"},
			result: metrics.ResultAllowed,
			source: metrics.SourceRole,
		},
		{
			name:   "default deny",
			store:  &fakeStore{},
			result: metrics.ResultDenied,
			source: metrics.SourceDefaultDeny,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCache()
			if tt.cached != nil {
				c.entries[makeReq()] = *tt.cached
			}
			res := NewResolver(c, tt.store, newRegistry(), testTTL)
			before := snapshotCheck(t, tt.result, tt.source)

			if _, err := res.Check(context.Background(), makeReq()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			after := snapshotCheck(t, tt.result, tt.source)
			if d := after.total - before.total; d != 1 {
				t.Errorf("authz_check_total{result=%q,source=%q} += %v, want 1", tt.result, tt.source, d)
			}
			if d := after.durations - before.durations; d != 1 {
				t.Errorf("authz_check_duration_seconds{source=%q} count += %v, want 1", tt.source, d)
			}
			wantHit, wantMiss := 0.0, 1.0
			if tt.cacheHit {
				wantHit, wantMiss = 1, 0
			}
			if d := after.hits - before.hits; d != wantHit {
				t.Errorf("authz_cache_hit_total += %v, want %v", d, wantHit)
			}
			if d := after.misses - before.misses; d != wantMiss {
				t.Errorf("authz_cache_miss_total += %v, want %v", d, wantMiss)
			}
		})
	}
}

func TestCheck_MetricsCacheErrorIsAMiss(t *testing.T) {
	c := newFakeCache()
	c.getErr = errors.New("redis down")
	res := NewResolver(c, &fakeStore{}, newRegistry(), testTTL)
	before := snapshotCheck(t, metrics.ResultDenied, metrics.SourceDefaultDeny)

	if _, err := res.Check(context.Background(), makeReq()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	after := snapshotCheck(t, metrics.ResultDenied, metrics.SourceDefaultDeny)
	if after.misses-before.misses != 1 || after.hits != before.hits {
		t.Errorf("cache error: hits += %v, misses += %v; want 0 and 1", after.hits-before.hits, after.misses-before.misses)
	}
}

func TestCheck_MetricsUnknownPermission(t *testing.T) {
	res := NewResolver(newFakeCache(), &fakeStore{}, registry.NewEmpty(), testTTL)
	before := snapshotCheck(t, metrics.ResultUnknown, metrics.SourceUnknownPermission)

	if _, err := res.Check(context.Background(), makeReq()); !errors.Is(err, model.ErrUnknownPermission) {
		t.Fatalf("expected ErrUnknownPermission, got %v", err)
	}

	after := snapshotCheck(t, metrics.ResultUnknown, metrics.SourceUnknownPermission)
	if d := after.total - before.total; d != 1 {
		t.Errorf("authz_check_total{result=unknown,source=unknown_permission} += %v, want 1", d)
	}
	if d := after.durations - before.durations; d != 1 {
		t.Errorf("duration count += %v, want 1", d)
	}
	if after.hits != before.hits || after.misses != before.misses {
		t.Error("an unknown permission never reaches the cache, so it is neither a hit nor a miss")
	}
}

// sumCheckTotal adds up authz_check_total over every series.
func sumCheckTotal(t *testing.T) float64 {
	t.Helper()
	series, _ := metricstest.Series(t, "authz_check_total")
	var sum float64
	for _, labels := range series {
		sum += metricstest.Value(t, "authz_check_total", labels)
	}
	return sum
}

func TestCheck_MetricsNotCountedOnDBError(t *testing.T) {
	for _, store := range []*fakeStore{
		{overrideErr: errors.New("db down")},
		{roleErr: errors.New("db down")},
	} {
		res := NewResolver(newFakeCache(), store, newRegistry(), testTTL)
		before := sumCheckTotal(t)

		if _, err := res.Check(context.Background(), makeReq()); err == nil {
			t.Fatal("expected the DB error")
		}

		if d := sumCheckTotal(t) - before; d != 0 {
			t.Errorf("a failed check changed authz_check_total by %v", d)
		}
	}
}

// AC-1: label values are bounded: no series carries a role name.
func TestCheck_MetricLabelsBounded(t *testing.T) {
	res := NewResolver(newFakeCache(), &fakeStore{roleName: "some-very-specific-role"}, newRegistry(), testTTL)
	if _, err := res.Check(context.Background(), makeReq()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	allowedSources := map[string]bool{
		metrics.SourceCache: true, metrics.SourceDenyOverride: true, metrics.SourceAllowOverride: true,
		metrics.SourceRole: true, metrics.SourceDefaultDeny: true, metrics.SourceUnknownPermission: true,
	}
	allowedResults := map[string]bool{metrics.ResultAllowed: true, metrics.ResultDenied: true, metrics.ResultUnknown: true}
	for _, name := range []string{"authz_check_total", "authz_check_duration_seconds"} {
		series, ok := metricstest.Series(t, name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		for _, labels := range series {
			if !allowedSources[labels["source"]] {
				t.Errorf("%s has unbounded source label %q", name, labels["source"])
			}
			if r, ok := labels["result"]; ok && !allowedResults[r] {
				t.Errorf("%s has unexpected result label %q", name, r)
			}
		}
	}
}
