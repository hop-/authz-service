package kafkatopics

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// checkScript returns its results in order, then reports nothing missing.
type checkScript struct {
	results []checkResult
	calls   int
}

type checkResult struct {
	missing []string
	err     error
}

func (c *checkScript) check(context.Context) ([]string, error) {
	c.calls++
	if len(c.results) == 0 {
		return nil, nil
	}
	r := c.results[0]
	c.results = c.results[1:]
	return r.missing, r.err
}

func recordSleeps(sleeps *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return ctx.Err()
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

var topics = []string{"authz.permission-events", "authz.permission-events.dlq"}

func TestWait_ReturnsWhenTopicsExist(t *testing.T) {
	c := &checkScript{}
	var sleeps []time.Duration

	if err := wait(context.Background(), topics, c.check, recordSleeps(&sleeps)); err != nil {
		t.Fatalf("wait = %v, want nil", err)
	}
	if c.calls != 1 || len(sleeps) != 0 {
		t.Fatalf("calls=%d sleeps=%v, want one check and no wait", c.calls, sleeps)
	}
}

func TestWait_BacksOffAndLogsMissingTopics(t *testing.T) {
	logs := captureLogs(t)
	missingDLQ := checkResult{missing: []string{"authz.permission-events.dlq"}}
	c := &checkScript{results: []checkResult{
		{missing: topics}, missingDLQ, missingDLQ, missingDLQ, missingDLQ, missingDLQ, missingDLQ, missingDLQ,
	}}
	var sleeps []time.Duration

	if err := wait(context.Background(), topics, c.check, recordSleeps(&sleeps)); err != nil {
		t.Fatalf("wait = %v, want nil", err)
	}
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	if !slices.Equal(sleeps, want) {
		t.Fatalf("backoff = %v, want %v", sleeps, want)
	}
	out := logs.String()
	if n := strings.Count(out, "level=ERROR"); n != len(want) {
		t.Errorf("ERROR logs = %d, want one per attempt (%d):\n%s", n, len(want), out)
	}
	if !strings.Contains(out, "authz.permission-events.dlq") {
		t.Errorf("logs do not name the missing topic:\n%s", out)
	}
}

func TestWait_RetriesCheckErrors(t *testing.T) {
	logs := captureLogs(t)
	c := &checkScript{results: []checkResult{{err: errors.New("dial tcp: connection refused")}}}
	var sleeps []time.Duration

	if err := wait(context.Background(), topics, c.check, recordSleeps(&sleeps)); err != nil {
		t.Fatalf("wait = %v, want nil after the broker comes back", err)
	}
	if !slices.Equal(sleeps, []time.Duration{time.Second}) {
		t.Fatalf("backoff = %v, want [1s]", sleeps)
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "connection refused") {
		t.Errorf("the check error was not logged at ERROR:\n%s", out)
	}
}

func TestWait_CancelledReturnsContextError(t *testing.T) {
	t.Run("during the backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &checkScript{results: []checkResult{{missing: topics}, {missing: topics}}}
		sleep := func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}

		if err := wait(ctx, topics, c.check, sleep); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = %v, want context.Canceled", err)
		}
		if c.calls != 1 {
			t.Errorf("checks = %d, want 1", c.calls)
		}
	})

	t.Run("during the check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		check := func(ctx context.Context) ([]string, error) {
			cancel()
			return nil, ctx.Err()
		}
		var sleeps []time.Duration

		if err := wait(ctx, topics, check, recordSleeps(&sleeps)); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = %v, want context.Canceled", err)
		}
		if len(sleeps) != 0 {
			t.Errorf("must not back off after cancellation, slept %v", sleeps)
		}
	})
}

func TestSleepContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext on a cancelled ctx = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("sleepContext did not return promptly on cancellation")
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepContext = %v, want nil", err)
	}
}

// TestMissing_UnreachableBroker checks that a metadata failure is an error,
// not a "missing" answer. Topic existence itself needs a broker and is covered
// by the integration tests.
func TestMissing_UnreachableBroker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	missing, err := Missing(ctx, []string{addr}, topics...)
	if err == nil {
		t.Fatalf("Missing against an unreachable broker = %v, nil; want an error", missing)
	}

	missing, err = Checker{Brokers: []string{addr}}.Missing(ctx, topics...)
	if err == nil {
		t.Fatalf("Checker.Missing against an unreachable broker = %v, nil; want an error", missing)
	}
}
