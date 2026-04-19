package api

import (
	"context"
	"net/http"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/producer"
	"authz-service/internal/registry"
)

// HealthDeps holds dependencies for health checks.
var HealthDeps struct {
	DB        *db.Queries
	Cache     *cache.Cache
	Registry  *registry.Registry
	Producer  *producer.Producer
	AdminMode bool
}

// @Summary Liveness check
// @Description Always returns 200 OK if the process is running.
// @Tags health
// @Produce plain
// @Success 200 {string} string "ok"
// @Router /healthz [get]
func handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// @Summary Readiness check
// @Description Checks DB, Redis, registry, and Kafka (if admin mode). Returns 503 if any check fails.
// @Tags health
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Failure 503 {object} map[string]interface{}
// @Router /readyz [get]
func handleReadiness(w http.ResponseWriter, r *http.Request) {
	checks := make(map[string]string)

	// DB ping
	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel()
	if err := HealthDeps.DB.Ping(ctx); err != nil {
		checks["db"] = "failed: " + err.Error()
	} else {
		checks["db"] = "ok"
	}

	// Redis ping
	ctx2, cancel2 := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel2()
	if err := HealthDeps.Cache.Ping(ctx2); err != nil {
		checks["redis"] = "failed: " + err.Error()
	} else {
		checks["redis"] = "ok"
	}

	// Registry loaded
	if HealthDeps.Registry.PermissionCount() == 0 {
		checks["registry"] = "failed: no permissions loaded"
	} else {
		checks["registry"] = "ok"
	}

	// Kafka producer (AdminMode only)
	if HealthDeps.AdminMode {
		ctx3, cancel3 := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel3()
		if err := HealthDeps.Producer.Ping(ctx3); err != nil {
			checks["kafka"] = "failed: " + err.Error()
		} else {
			checks["kafka"] = "ok"
		}
	}

	// Determine overall status
	allOK := true
	for _, v := range checks {
		if v != "ok" {
			allOK = false
			break
		}
	}

	status := http.StatusOK
	if !allOK {
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, map[string]interface{}{
		"checks": checks,
	})
}
