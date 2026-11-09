package metrics_test

import (
	"context"
	"testing"

	"authz-service/internal/metrics"
	"authz-service/internal/metrics/metricstest"
	"authz-service/internal/registry"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AC-1: every metric of plan §1 is registered with its name, type and label
// names. Vec metrics only appear once a series exists, so the test creates
// one series of each.
func TestMetrics_NamesTypesLabels(t *testing.T) {
	metrics.CheckTotal.WithLabelValues(metrics.ResultAllowed, metrics.SourceRole)
	metrics.CheckDuration.WithLabelValues(metrics.SourceRole).Observe(0.001)
	metrics.RegistryUpdatesTotal.WithLabelValues(metrics.EventTypeReload)
	metrics.WorkerEventsProcessedTotal.WithLabelValues("role.assigned", metrics.StatusOK)
	metrics.WorkerProcessingDuration.WithLabelValues("role.assigned").Observe(0.001)
	metrics.SetRegistry(registry.NewEmpty())
	pool := lazyPool(t)
	metrics.SetDBPool(pool)

	tests := []struct {
		name   string
		typ    string
		labels []string
	}{
		{"authz_check_total", "COUNTER", []string{"result", "source"}},
		{"authz_check_duration_seconds", "HISTOGRAM", []string{"source"}},
		{"authz_cache_hit_total", "COUNTER", nil},
		{"authz_cache_miss_total", "COUNTER", nil},
		{"authz_registry_size", "GAUGE", []string{"type"}},
		{"authz_registry_updates_total", "COUNTER", []string{"event_type"}},
		{"authz_db_pool_total_conns", "GAUGE", nil},
		{"authz_db_pool_idle_conns", "GAUGE", nil},
		{"authz_db_pool_acquired_conns", "GAUGE", nil},
		{"authz_worker_events_processed_total", "COUNTER", []string{"event_type", "status"}},
		{"authz_worker_processing_duration_seconds", "HISTOGRAM", []string{"event_type"}},
		{"authz_event_log_deleted_total", "COUNTER", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metricstest.Type(t, tt.name); got != tt.typ {
				t.Fatalf("type = %q, want %q", got, tt.typ)
			}
			series, _ := metricstest.Series(t, tt.name)
			if len(series) == 0 {
				t.Fatal("no series")
			}
			for _, labels := range series {
				if len(labels) != len(tt.labels) {
					t.Fatalf("labels = %v, want names %v", labels, tt.labels)
				}
				for _, l := range tt.labels {
					if _, ok := labels[l]; !ok {
						t.Fatalf("labels = %v, missing %q", labels, l)
					}
				}
			}
		})
	}
}

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// pgxpool connects lazily, so this needs no database.
	pool, err := pgxpool.New(context.Background(), "postgres://authz@127.0.0.1:1/authz?sslmode=disable")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fakeSizer struct{ perms, roles int }

func (s fakeSizer) PermissionCount() int { return s.perms }
func (s fakeSizer) RoleCount() int       { return s.roles }

func TestSetRegistry_ReportsSizeByType(t *testing.T) {
	metrics.SetRegistry(fakeSizer{perms: 7, roles: 3})
	t.Cleanup(func() { metrics.SetRegistry(nil) })

	if got := metricstest.Value(t, "authz_registry_size", map[string]string{"type": "permissions"}); got != 7 {
		t.Errorf("permissions = %v, want 7", got)
	}
	if got := metricstest.Value(t, "authz_registry_size", map[string]string{"type": "roles"}); got != 3 {
		t.Errorf("roles = %v, want 3", got)
	}

	// A later source replaces the earlier one (RunAPI may run more than once).
	metrics.SetRegistry(fakeSizer{perms: 1, roles: 2})
	if got := metricstest.Value(t, "authz_registry_size", map[string]string{"type": "permissions"}); got != 1 {
		t.Errorf("after replace: permissions = %v, want 1", got)
	}
}

func TestSourceGauges_AbsentUntilSet(t *testing.T) {
	metrics.SetRegistry(nil)
	metrics.SetDBPool(nil)
	for _, name := range []string{"authz_registry_size", "authz_db_pool_total_conns", "authz_db_pool_idle_conns", "authz_db_pool_acquired_conns"} {
		if series, ok := metricstest.Series(t, name); ok && len(series) > 0 {
			t.Errorf("%s reported %v without a source", name, series)
		}
	}
}

func TestSetDBPool_ReportsPoolStats(t *testing.T) {
	pool := lazyPool(t)
	metrics.SetDBPool(pool)
	t.Cleanup(func() { metrics.SetDBPool(nil) })

	s := pool.Stat()
	for name, want := range map[string]int32{
		"authz_db_pool_total_conns":    s.TotalConns(),
		"authz_db_pool_idle_conns":     s.IdleConns(),
		"authz_db_pool_acquired_conns": s.AcquiredConns(),
	} {
		series, ok := metricstest.Series(t, name)
		if !ok || len(series) != 1 {
			t.Errorf("%s: series = %v, want exactly one", name, series)
			continue
		}
		if got := metricstest.Value(t, name, nil); got != float64(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestEventTypeLabel(t *testing.T) {
	known := []string{
		"role.assigned", "role.revoked", "override.set", "override.removed",
		"action.registered", "action.removed", "action.id.assigned",
		"role.created", "role.removed", "role.id.assigned",
		"role_permissions.assigned", "role_permissions.removed",
	}
	for _, et := range known {
		if got := metrics.EventTypeLabel(et); got != et {
			t.Errorf("EventTypeLabel(%q) = %q, want it unchanged", et, got)
		}
	}
	for _, et := range []string{"", "role.exploded", "ROLE.ASSIGNED", "role.assigned ", "x" + string(rune(0))} {
		if got := metrics.EventTypeLabel(et); got != metrics.EventTypeUnknown {
			t.Errorf("EventTypeLabel(%q) = %q, want %q", et, got, metrics.EventTypeUnknown)
		}
	}
}
