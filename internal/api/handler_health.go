package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"authz-service/internal/model"
	"authz-service/internal/registry"
)

// pinger is a dependency whose connectivity readiness checks.
type pinger interface {
	Ping(ctx context.Context) error
}

// topicChecker reports which of the given Kafka topics don't exist.
type topicChecker interface {
	Missing(ctx context.Context, topics ...string) ([]string, error)
}

// HealthHandler serves the liveness and readiness probes.
type HealthHandler struct {
	DB       pinger
	Cache    pinger
	Registry *registry.Registry
	// Kafka is checked only in admin mode, where the API publishes events.
	Kafka pinger
	// Topics and KafkaTopic are checked in every mode: the registry syncer
	// consumes KafkaTopic, and admin mode publishes to it.
	Topics     topicChecker
	KafkaTopic string
	AdminMode  bool
}

// @Summary Liveness check
// @Description Always returns 200 OK if the process is running.
// @Tags health
// @Produce json
// @Success 200 {object} model.HealthResponse
// @Router /healthz [get]
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, model.HealthResponse{Status: "ok"})
}

// @Summary Readiness check
// @Description Checks DB, Redis, registry, that the Kafka topic exists, and Kafka broker reachability (if admin mode). Returns 503 if any check fails.
// @Tags health
// @Produce json
// @Success 200 {object} model.ReadinessResponse
// @Failure 503 {object} model.ReadinessResponse
// @Router /readyz [get]
func (h *HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	checks := make(map[string]string)

	// DB ping
	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel()
	if err := h.DB.Ping(ctx); err != nil {
		checks["db"] = "failed: " + err.Error()
	} else {
		checks["db"] = "ok"
	}

	// Redis ping
	ctx2, cancel2 := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel2()
	if err := h.Cache.Ping(ctx2); err != nil {
		checks["redis"] = "failed: " + err.Error()
	} else {
		checks["redis"] = "ok"
	}

	// Registry loaded
	if h.Registry.PermissionCount() == 0 {
		checks["registry"] = "failed: no permissions loaded"
	} else {
		checks["registry"] = "ok"
	}

	// Kafka topic exists (all modes)
	if h.Topics == nil {
		checks["kafka_topics"] = "failed: topic check not configured"
	} else {
		ctx4, cancel4 := context.WithTimeout(r.Context(), 1*time.Second)
		defer cancel4()
		missing, err := h.Topics.Missing(ctx4, h.KafkaTopic)
		switch {
		case err != nil:
			checks["kafka_topics"] = "failed: " + err.Error()
		case len(missing) > 0:
			checks["kafka_topics"] = "failed: missing " + strings.Join(missing, ", ")
		default:
			checks["kafka_topics"] = "ok"
		}
	}

	// Kafka producer (AdminMode only)
	if h.AdminMode {
		if h.Kafka == nil {
			checks["kafka"] = "failed: producer not configured"
		} else {
			ctx3, cancel3 := context.WithTimeout(r.Context(), 1*time.Second)
			defer cancel3()
			if err := h.Kafka.Ping(ctx3); err != nil {
				checks["kafka"] = "failed: " + err.Error()
			} else {
				checks["kafka"] = "ok"
			}
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

	statusCode := http.StatusOK
	statusStr := "ok"
	if !allOK {
		statusCode = http.StatusServiceUnavailable
		statusStr = "unavailable"
	}

	writeJSON(w, statusCode, model.ReadinessResponse{
		Status: statusStr,
		Checks: checks,
	})
}
