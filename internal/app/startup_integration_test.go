//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"authz-service/internal/config"
	"authz-service/internal/producer"
	"authz-service/internal/testinfra"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One Postgres, Redis and Kafka for the package; each test uses its own
// topics and consumer group (008 F-005: the startup sequence under test).
var (
	itPool    *pgxpool.Pool
	itRedis   string
	itBrokers []string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	// The services log every request and event; keep WARN and above.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	var stops []func() error
	exit := func(code int) {
		for i := len(stops) - 1; i >= 0; i-- {
			if err := stops[i](); err != nil {
				fmt.Fprintln(os.Stderr, "stop container:", err)
			}
		}
		os.Exit(code)
	}
	p, stop, err := testinfra.StartPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		exit(1)
	}
	itPool, stops = p, append(stops, stop)
	addr, stop, err := testinfra.StartRedis(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		exit(1)
	}
	itRedis, stops = addr, append(stops, stop)
	b, stop, err := testinfra.StartKafka(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start kafka:", err)
		exit(1)
	}
	itBrokers, stops = b, append(stops, stop)
	exit(m.Run())
}

const waitFor = 90 * time.Second

// freePort returns a port that was free a moment ago. Run* binds it at
// startup, so a rare conflict fails that test loudly rather than silently.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

func testConfig(t *testing.T, topic string) *config.Config {
	t.Helper()
	return &config.Config{
		DatabaseURL:            itPool.Config().ConnString(),
		DBMaxConns:             4,
		DBMinConns:             0,
		DBMaxConnIdleSecs:      60,
		DBMaxConnLifetimeSecs:  600,
		DBAcquireTimeoutSecs:   2,
		RedisAddr:              itRedis,
		CacheTTLSeconds:        60,
		KafkaBrokers:           strings.Join(itBrokers, ","),
		KafkaTopic:             topic,
		KafkaGroupID:           "group-" + uuid.NewString()[:8],
		HTTPPort:               freePort(t),
		MetricsPort:            freePort(t),
		RegistryReloadSecs:     300,
		EventLogRetentionHours: 24,
	}
}

func uniqTopic(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// run starts fn in a goroutine and returns a channel with its result.
func run(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return done
}

func requireReturns(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("did not return within %v", within)
		return nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// get returns the status and body of a GET, or status 0 when it fails.
func get(url string) (int, string) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }() // read-only
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, ""
	}
	return resp.StatusCode, string(b)
}

func metricsBody(t *testing.T, port int) string {
	t.Helper()
	status, body := get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if status != http.StatusOK {
		t.Fatalf("/metrics status = %d", status)
	}
	return body
}

// AC-4 / 008 F-005: the worker waits for its topics, then consumes once they
// exist; a cancel shuts it down cleanly.
func TestRunWorker_WaitsForTopicsThenProcesses(t *testing.T) {
	topic := uniqTopic("startup.worker")
	cfg := testConfig(t, topic)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := run(func() error { return RunWorker(ctx, cfg) })

	// The metrics server is up while the worker waits for the topics.
	eventually(t, "worker metrics server", func() bool {
		status, _ := get(fmt.Sprintf("http://127.0.0.1:%d/metrics", cfg.MetricsPort))
		return status == http.StatusOK
	})
	select {
	case err := <-done:
		t.Fatalf("RunWorker returned %v while the topics are missing; it must wait", err)
	case <-time.After(2 * time.Second):
	}

	testinfra.CreateTopics(t, itBrokers, topic, topic+".dlq")
	user := uuid.New()
	evt := producer.NewEventEnvelope("role.assigned")
	evt["user_id"], evt["role_name"] = user.String(), "viewer"
	evt["resource_type"], evt["resource_id"] = "document", "d1"
	evt["scope"], evt["granted_by"] = map[string]string{}, uuid.NewString()
	pub := producer.NewProducer(cfg.KafkaBrokers, topic)
	t.Cleanup(func() { _ = pub.Close() }) // test cleanup
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pubCancel()
	if err := pub.Publish(pubCtx, user.String(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}
	eventually(t, "event applied after the topics appeared", func() bool {
		var n int
		err := itPool.QueryRow(context.Background(), `SELECT count(*) FROM user_roles WHERE user_id = $1`, user).Scan(&n)
		return err == nil && n == 1
	})

	// AC-1: the worker exposes its metrics, pool gauges included.
	eventually(t, "worker metrics", func() bool {
		body := metricsBody(t, cfg.MetricsPort)
		return strings.Contains(body, `authz_worker_events_processed_total{event_type="role.assigned",status="ok"}`) &&
			strings.Contains(body, "authz_worker_processing_duration_seconds_count") &&
			strings.Contains(body, "authz_db_pool_total_conns")
	})

	cancel()
	if err := requireReturns(t, done, 45*time.Second); err != nil {
		t.Fatalf("RunWorker = %v after cancel, want nil", err)
	}
	if status, _ := get(fmt.Sprintf("http://127.0.0.1:%d/metrics", cfg.MetricsPort)); status != 0 {
		t.Fatal("the metrics server still answers after RunWorker returned")
	}
}

// AC-4 / 008 F-005: a cancel during the topic wait returns nil promptly.
func TestRunWorker_CancelDuringTopicWait(t *testing.T) {
	cfg := testConfig(t, uniqTopic("startup.never"))
	ctx, cancel := context.WithCancel(context.Background())
	done := run(func() error { return RunWorker(ctx, cfg) })
	eventually(t, "worker metrics server", func() bool {
		status, _ := get(fmt.Sprintf("http://127.0.0.1:%d/metrics", cfg.MetricsPort))
		return status == http.StatusOK
	})

	cancel()
	if err := requireReturns(t, done, 15*time.Second); err != nil {
		t.Fatalf("RunWorker = %v, want nil on cancel during the topic wait", err)
	}
}

// AC-4 / 008 F-005: /readyz goes from 503 (topic missing) to 200, and the API
// shuts down cleanly.
func TestRunAPI_ReadinessTransitionAndShutdown(t *testing.T) {
	topic := uniqTopic("startup.api")
	cfg := testConfig(t, topic)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := run(func() error { return RunAPI(ctx, cfg) })
	readyz := fmt.Sprintf("http://127.0.0.1:%d/readyz", cfg.HTTPPort)

	var body string
	eventually(t, "API serving /readyz", func() bool {
		var status int
		status, body = get(readyz)
		return status != 0
	})
	var ready struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &ready); err != nil {
		t.Fatalf("readyz body %q: %v", body, err)
	}
	if status, _ := get(readyz); status != http.StatusServiceUnavailable || !strings.HasPrefix(ready.Checks["kafka_topics"], "failed: missing") {
		t.Fatalf("readyz before the topic exists = %d %v, want 503 with kafka_topics missing", status, ready.Checks)
	}

	testinfra.CreateTopics(t, itBrokers, topic)
	eventually(t, "readyz 200 after the topic exists", func() bool {
		status, _ := get(readyz)
		return status == http.StatusOK
	})

	// A query works and the API's metrics include the registry and pool gauges.
	checkURL := fmt.Sprintf("http://127.0.0.1:%d/query/v1/check?user_id=%s&action=read&resource_type=document&resource_id=d1",
		cfg.HTTPPort, uuid.NewString())
	if status, b := get(checkURL); status != http.StatusOK {
		t.Fatalf("check = %d %s", status, b)
	}
	metrics := metricsBody(t, cfg.MetricsPort)
	for _, want := range []string{
		`authz_registry_size{type="permissions"} 3`, `authz_registry_size{type="roles"} 3`,
		"authz_db_pool_total_conns", `authz_check_total{result="denied",source="default_deny"}`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("API /metrics lacks %s", want)
		}
	}

	cancel()
	if err := requireReturns(t, done, 30*time.Second); err != nil {
		t.Fatalf("RunAPI = %v after cancel, want nil", err)
	}
	if status, _ := get(readyz); status != 0 {
		t.Fatal("the HTTP server still answers after RunAPI returned")
	}
}

// AC-4: with ADMIN_MODE=true, RunAPI fails when Kafka is unreachable at startup.
func TestRunAPI_AdminModeKafkaUnreachable(t *testing.T) {
	cfg := testConfig(t, uniqTopic("startup.admin"))
	cfg.AdminMode, cfg.AdminToken = true, "t"
	cfg.KafkaBrokers = "127.0.0.1:0" // refused at once: nothing listens on port 0

	done := run(func() error { return RunAPI(context.Background(), cfg) })

	err := requireReturns(t, done, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "kafka unreachable") {
		t.Fatalf("RunAPI = %v, want a kafka unreachable error", err)
	}
	if status, _ := get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.HTTPPort)); status != 0 {
		t.Fatal("RunAPI served HTTP although the admin startup check failed")
	}
}

// AC-4: with ADMIN_MODE=true and Kafka reachable, RunAPI starts.
func TestRunAPI_AdminModeKafkaReachable(t *testing.T) {
	topic := uniqTopic("startup.admin.ok")
	testinfra.CreateTopics(t, itBrokers, topic)
	cfg := testConfig(t, topic)
	cfg.AdminMode, cfg.AdminToken = true, "t"
	ctx, cancel := context.WithCancel(context.Background())
	done := run(func() error { return RunAPI(ctx, cfg) })

	eventually(t, "admin API ready", func() bool {
		status, _ := get(fmt.Sprintf("http://127.0.0.1:%d/readyz", cfg.HTTPPort))
		return status == http.StatusOK
	})
	cancel()
	if err := requireReturns(t, done, 30*time.Second); err != nil {
		t.Fatalf("RunAPI = %v after cancel, want nil", err)
	}
}
