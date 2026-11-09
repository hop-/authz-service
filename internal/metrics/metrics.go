// Package metrics defines the service's Prometheus metrics. They are
// registered once, on the default registry, and served on /metrics
// (METRICS_PORT) by both binaries.
//
// Every label value comes from a fixed set, so cardinality is bounded:
// check sources are categories (never role names), and event types outside
// the known set are reported as "unknown".
package metrics

import (
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Check results (label "result").
const (
	ResultAllowed = "allowed"
	ResultDenied  = "denied"
	ResultUnknown = "unknown" // the (action, resource_type) is not registered
)

// Check source categories (label "source").
const (
	SourceCache             = "cache"
	SourceDenyOverride      = "deny_override"
	SourceAllowOverride     = "allow_override"
	SourceRole              = "role"
	SourceDefaultDeny       = "default_deny"
	SourceUnknownPermission = "unknown_permission"
)

// Worker outcomes (label "status").
const (
	StatusOK        = "ok"
	StatusDuplicate = "duplicate"
	StatusDLQ       = "dlq"
)

// EventTypeReload is the registry_updates_total event_type of a full reload
// from the database.
const EventTypeReload = "reload"

// EventTypeUnknown labels an event type outside the known set.
const EventTypeUnknown = "unknown"

// knownEventTypes are the event types the services publish.
var knownEventTypes = map[string]struct{}{
	"role.assigned":             {},
	"role.revoked":              {},
	"override.set":              {},
	"override.removed":          {},
	"action.registered":         {},
	"action.removed":            {},
	"action.id.assigned":        {},
	"role.created":              {},
	"role.removed":              {},
	"role.id.assigned":          {},
	"role_permissions.assigned": {},
	"role_permissions.removed":  {},
}

// EventTypeLabel returns t if it is a known event type, else "unknown".
func EventTypeLabel(t string) string {
	if _, ok := knownEventTypes[t]; ok {
		return t
	}
	return EventTypeUnknown
}

var (
	// CheckTotal counts /check decisions by result and source category.
	CheckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "authz_check_total",
		Help: "Permission checks by result (allowed, denied, unknown) and source category.",
	}, []string{"result", "source"})

	// CheckDuration observes the latency of /check decisions by source category.
	CheckDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "authz_check_duration_seconds",
		Help:    "Permission check latency by source category.",
		Buckets: prometheus.DefBuckets,
	}, []string{"source"})

	// CacheHitTotal and CacheMissTotal count check-cache lookups.
	CacheHitTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "authz_cache_hit_total",
		Help: "Check cache hits.",
	})
	CacheMissTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "authz_cache_miss_total",
		Help: "Check cache misses (including cache errors).",
	})

	// RegistryUpdatesTotal counts registry changes applied by the API's
	// registry syncer: registry events, and full reloads as event_type="reload".
	RegistryUpdatesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "authz_registry_updates_total",
		Help: "Registry updates applied by the API registry syncer, by event type (reload for a full reload).",
	}, []string{"event_type"})

	// WorkerEventsProcessedTotal counts handled events by type and outcome.
	WorkerEventsProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "authz_worker_events_processed_total",
		Help: "Events handled by the worker, by event type and status (ok, duplicate, dlq).",
	}, []string{"event_type", "status"})

	// WorkerProcessingDuration observes how long the worker took per event.
	WorkerProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "authz_worker_processing_duration_seconds",
		Help:    "Worker event processing latency by event type.",
		Buckets: prometheus.DefBuckets,
	}, []string{"event_type"})

	// EventLogDeletedTotal counts event_log rows removed by the cleanup.
	EventLogDeletedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "authz_event_log_deleted_total",
		Help: "event_log rows deleted by the retention cleanup.",
	})
)

// The gauges below read their values when scraped, from a source set once
// at startup. They report nothing until their source is set.

// RegistrySizer reports the registry's size. *registry.Registry satisfies it.
type RegistrySizer interface {
	PermissionCount() int
	RoleCount() int
}

// PoolStatter reports pool statistics. *pgxpool.Pool satisfies it.
type PoolStatter interface {
	Stat() *pgxpool.Stat
}

var (
	registrySizeDesc = prometheus.NewDesc("authz_registry_size",
		"Entries in the API's in-memory registry, by type (permissions, roles).", []string{"type"}, nil)
	poolTotalDesc = prometheus.NewDesc("authz_db_pool_total_conns",
		"Connections in the DB pool.", nil, nil)
	poolIdleDesc = prometheus.NewDesc("authz_db_pool_idle_conns",
		"Idle connections in the DB pool.", nil, nil)
	poolAcquiredDesc = prometheus.NewDesc("authz_db_pool_acquired_conns",
		"Acquired (in use) connections in the DB pool.", nil, nil)
)

// sourceGauges is a collector for the gauges read from a source on scrape.
type sourceGauges struct {
	mu       sync.RWMutex
	registry RegistrySizer
	pool     PoolStatter
}

var gauges = &sourceGauges{}

func init() {
	prometheus.MustRegister(gauges)
}

// SetRegistry makes authz_registry_size report r. Later calls replace it.
func SetRegistry(r RegistrySizer) {
	gauges.mu.Lock()
	defer gauges.mu.Unlock()
	gauges.registry = r
}

// SetDBPool makes the authz_db_pool_* gauges report p. Later calls replace it.
func SetDBPool(p PoolStatter) {
	gauges.mu.Lock()
	defer gauges.mu.Unlock()
	gauges.pool = p
}

func (g *sourceGauges) Describe(ch chan<- *prometheus.Desc) {
	ch <- registrySizeDesc
	ch <- poolTotalDesc
	ch <- poolIdleDesc
	ch <- poolAcquiredDesc
}

func (g *sourceGauges) Collect(ch chan<- prometheus.Metric) {
	g.mu.RLock()
	reg, pool := g.registry, g.pool
	g.mu.RUnlock()

	if reg != nil {
		ch <- prometheus.MustNewConstMetric(registrySizeDesc, prometheus.GaugeValue, float64(reg.PermissionCount()), "permissions")
		ch <- prometheus.MustNewConstMetric(registrySizeDesc, prometheus.GaugeValue, float64(reg.RoleCount()), "roles")
	}
	if pool != nil {
		s := pool.Stat()
		ch <- prometheus.MustNewConstMetric(poolTotalDesc, prometheus.GaugeValue, float64(s.TotalConns()))
		ch <- prometheus.MustNewConstMetric(poolIdleDesc, prometheus.GaugeValue, float64(s.IdleConns()))
		ch <- prometheus.MustNewConstMetric(poolAcquiredDesc, prometheus.GaugeValue, float64(s.AcquiredConns()))
	}
}
