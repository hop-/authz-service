package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"authz-service/internal/config"
)

func TestNewLogger_AddsService(t *testing.T) {
	for _, service := range []string{ServiceAPI, ServiceWorker} {
		var buf bytes.Buffer
		l := NewLogger(&buf, service)
		l.Info("a")
		l.With("event_id", "e1").Warn("b")

		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("not JSON: %q", line)
			}
			if m["service"] != service {
				t.Errorf("entry %v: service = %v, want %s", m, m["service"], service)
			}
		}
	}
}

// AC-4: the API and both metrics servers use these timeouts.
func TestNewHTTPServer_Timeouts(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 10*time.Second ||
		srv.WriteTimeout != 10*time.Second || srv.IdleTimeout != 60*time.Second {
		t.Fatalf("timeouts = header %v read %v write %v idle %v; want 5s 10s 10s 60s",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}

func TestListen_UsesServerTimeouts(t *testing.T) {
	s, err := listen("test", 0, http.NotFoundHandler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = s.ln.Close() }) // never served; the close error is irrelevant
	if s.srv.ReadHeaderTimeout != readHeaderTimeout || s.srv.WriteTimeout != writeTimeout {
		t.Fatal("listen must build the server with newHTTPServer's timeouts")
	}
}

func TestListen_PortInUseFailsAtStartup(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // test cleanup
	port := ln.Addr().(*net.TCPAddr).Port

	if _, err := listen("metrics", port, http.NotFoundHandler()); err == nil {
		t.Fatal("listen on a taken port must fail synchronously")
	}
}

func TestServer_ServeAndShutdown(t *testing.T) {
	s, err := listen("test", 0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	errCh := make(chan error, 1)
	s.serve(errCh)

	resp, err := http.Get("http://" + s.ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close() // body unused
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.shutdown(ctx)
	select {
	case err := <-errCh:
		t.Fatalf("a clean shutdown must not report a serve error, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMetricsHandler_ServesMetrics(t *testing.T) {
	s, err := listen("metrics", 0, newMetricsHandler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.serve(make(chan error, 1))
	t.Cleanup(func() { s.shutdown(context.Background()) })

	resp, err := http.Get("http://" + s.ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body.String(), "authz_event_log_deleted_total") {
		t.Fatalf("status %d; /metrics must expose the authz metrics", resp.StatusCode)
	}
}

func TestValidateAPIConfig(t *testing.T) {
	valid := config.Config{KafkaBrokers: "k:9092", RegistryReloadSecs: 300}
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"valid", func(*config.Config) {}, ""},
		{"admin without token", func(c *config.Config) { c.AdminMode = true }, "ADMIN_TOKEN"},
		{"admin with token", func(c *config.Config) { c.AdminMode, c.AdminToken = true, "t" }, ""},
		{"no brokers", func(c *config.Config) { c.KafkaBrokers = "" }, "KAFKA_BROKERS"},
		{"zero reload", func(c *config.Config) { c.RegistryReloadSecs = 0 }, "REGISTRY_RELOAD_SECONDS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			err := validateAPIConfig(&cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one naming %s", err, tt.want)
			}
		})
	}
}

// Invalid config is rejected before anything is dialed, so these need no infra.
func TestRunAPI_InvalidConfigReturnsError(t *testing.T) {
	quiet(t)
	err := RunAPI(context.Background(), &config.Config{AdminMode: true, KafkaBrokers: "k:9092", RegistryReloadSecs: 1})
	if err == nil || !strings.Contains(err.Error(), "ADMIN_TOKEN") {
		t.Fatalf("RunAPI = %v, want the ADMIN_TOKEN error", err)
	}
}

func TestRunWorker_NegativeRetentionReturnsError(t *testing.T) {
	quiet(t)
	err := RunWorker(context.Background(), &config.Config{EventLogRetentionHours: -1})
	if err == nil || !strings.Contains(err.Error(), "EVENT_LOG_RETENTION_HOURS") {
		t.Fatalf("RunWorker = %v, want the EVENT_LOG_RETENTION_HOURS error", err)
	}
}

func TestRunWorker_BadDatabaseURLReturnsError(t *testing.T) {
	quiet(t)
	err := RunWorker(context.Background(), &config.Config{DatabaseURL: "::not a url::"})
	if err == nil || !strings.Contains(err.Error(), "db pool") {
		t.Fatalf("RunWorker = %v, want a db pool error", err)
	}
	var target *net.OpError
	if errors.As(err, &target) {
		t.Fatalf("RunWorker dialed something: %v", err)
	}
}

func TestAcquireTimeout(t *testing.T) {
	if got := acquireTimeout(&config.Config{DBAcquireTimeoutSecs: 2}); got != 2*time.Second {
		t.Fatalf("acquireTimeout = %v, want 2s", got)
	}
}

// quiet discards default-logger output for the rest of the test.
func quiet(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })
}
