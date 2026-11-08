package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	topicsOK := func() *fakeTopics { return &fakeTopics{} }
	tests := []struct {
		name       string
		handler    *HealthHandler
		wantStatus int
		wantChecks map[string]string // exact "ok" values; failures are matched by prefix
	}{
		{
			name:       "all ok",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Topics: topicsOK(), KafkaTopic: testTopic},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "ok"},
		},
		{
			name:       "all ok in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: fakePinger{}, Topics: topicsOK(), KafkaTopic: testTopic, AdminMode: true},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "ok", "kafka": "ok"},
		},
		{
			name:       "db down",
			handler:    &HealthHandler{DB: down, Cache: fakePinger{}, Registry: loadedRegistry(), Topics: topicsOK(), KafkaTopic: testTopic},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "failed: connection refused", "redis": "ok", "registry": "ok", "kafka_topics": "ok"},
		},
		{
			name:       "redis down",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: down, Registry: loadedRegistry(), Topics: topicsOK(), KafkaTopic: testTopic},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "failed: connection refused", "registry": "ok", "kafka_topics": "ok"},
		},
		{
			name:       "registry empty",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: registry.NewEmpty(), Topics: topicsOK(), KafkaTopic: testTopic},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "failed: no permissions loaded", "kafka_topics": "ok"},
		},
		{
			name:       "kafka down in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: down, Topics: topicsOK(), KafkaTopic: testTopic, AdminMode: true},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "ok", "kafka": "failed: connection refused"},
		},
		{
			name:       "kafka missing in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Topics: topicsOK(), KafkaTopic: testTopic, AdminMode: true},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "ok", "kafka": "failed: producer not configured"},
		},
		{
			name:       "kafka ignored outside admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: down, Topics: topicsOK(), KafkaTopic: testTopic},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "ok"},
		},
		{
			name:       "topic missing",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Topics: &fakeTopics{missing: []string{testTopic}}, KafkaTopic: testTopic},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "failed: missing " + testTopic},
		},
		{
			name:       "topic missing in admin mode",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Kafka: fakePinger{}, Topics: &fakeTopics{missing: []string{testTopic}}, KafkaTopic: testTopic, AdminMode: true},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "failed: missing " + testTopic, "kafka": "ok"},
		},
		{
			name:       "topic check error",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Topics: &fakeTopics{err: errors.New("kafka metadata: dial tcp: refused")}, KafkaTopic: testTopic},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "failed: kafka metadata: dial tcp: refused"},
		},
		{
			name:       "topic check not configured",
			handler:    &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), KafkaTopic: testTopic},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"db": "ok", "redis": "ok", "registry": "ok", "kafka_topics": "failed: topic check not configured"},
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
			if ft, ok := tt.handler.Topics.(*fakeTopics); ok {
				if len(ft.asked) != 1 || ft.asked[0] != testTopic {
					t.Errorf("topic check asked for %v, want [%s]", ft.asked, testTopic)
				}
			}
		})
	}
}

func TestReadiness_TopicCheckHasTimeout(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	topics := topicCheckerFunc(func(ctx context.Context, _ ...string) ([]string, error) {
		deadline, hasDeadline = ctx.Deadline()
		return nil, nil
	})
	h := &HealthHandler{DB: fakePinger{}, Cache: fakePinger{}, Registry: loadedRegistry(), Topics: topics, KafkaTopic: testTopic}

	start := time.Now()
	if status, resp := readiness(t, h); status != http.StatusOK {
		t.Fatalf("status = %d (%+v)", status, resp)
	}
	if !hasDeadline {
		t.Fatal("topic check ran without a deadline")
	}
	if d := deadline.Sub(start); d <= 0 || d > 1100*time.Millisecond {
		t.Fatalf("topic check deadline %v after start, want about 1s", d)
	}
}

type topicCheckerFunc func(ctx context.Context, topics ...string) ([]string, error)

func (f topicCheckerFunc) Missing(ctx context.Context, topics ...string) ([]string, error) {
	return f(ctx, topics...)
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
