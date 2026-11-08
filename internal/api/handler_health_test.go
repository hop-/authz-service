package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"authz-service/internal/model"
	"authz-service/internal/registry"
)

func loadedRegistry() *registry.Registry {
	reg := registry.NewEmpty()
	reg.AddPermission("read", "document", 1)
	return reg
}

func readiness(t *testing.T, h *HealthHandler) (int, model.ReadinessResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest("GET", "/readyz", nil))
	return rec.Code, decodeBody[model.ReadinessResponse](t, rec)
}

func TestReadiness(t *testing.T) {
	down := fakePinger{err: errors.New("connection refused")}
	tests := []struct {
		name       string
		handler    *HealthHandler
		wantStatus int
		wantChecks map[string]string // exact "ok" values; failures are matched by prefix
	}{
		{
			name:       "all ok",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry()},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok"},
		},
		{
			name:       "all ok in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: fakePinger{}, AdminMode: true},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka": "ok"},
		},
		{
			name:       "db down",
			handler:    &HealthHandler{DB: down, Cache: fakePinger{}, Registry: loadedRegistry()},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "failed: connection refused", "redis": "ok", "registry": "ok"},
		},
		{
			name:       "redis down",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: down, Registry: loadedRegistry()},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "failed: connection refused", "registry": "ok"},
		},
		{
			name:       "registry empty",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: registry.NewEmpty()},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "failed: no permissions loaded"},
		},
		{
			name:       "kafka down in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: down, AdminMode: true},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka": "failed: connection refused"},
		},
		{
			name:       "kafka missing in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), AdminMode: true},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka": "failed: producer not configured"},
		},
		{
			name:       "kafka ignored outside admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: down},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, resp := readiness(t, tt.handler)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%+v)", status, tt.wantStatus, resp)
			}
			wantStr := "ok"
			if tt.wantStatus != http.StatusOK {
				wantStr = "unavailable"
			}
			if resp.Status != wantStr {
				t.Errorf("status field = %q, want %q", resp.Status, wantStr)
			}
			if len(resp.Checks) != len(tt.wantChecks) {
				t.Errorf("checks = %v, want keys of %v", resp.Checks, tt.wantChecks)
			}
			for k, v := range tt.wantChecks {
				if got := resp.Checks[k]; !strings.HasPrefix(got, v) {
					t.Errorf("checks[%s] = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestLiveness(t *testing.T) {
	rec := httptest.NewRecorder()
	// Liveness needs no dependencies; the zero value works.
	(&HealthHandler{}).Liveness(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decodeBody[model.HealthResponse](t, rec); got.Status != "ok" {
		t.Fatalf("got %+v", got)
	}
}

func TestHealthRoutesMounted(t *testing.T) {
	ts := newTestServer(t)
	if rec := ts.do(t, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d", rec.Code)
	}
	if rec := ts.do(t, "GET", "/readyz", ""); rec.Code != http.StatusOK {
		t.Errorf("/readyz status = %d (body %s)", rec.Code, rec.Body.String())
	}
}
