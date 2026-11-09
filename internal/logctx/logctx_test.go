package logctx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

func lastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var m map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &m); err != nil {
		t.Fatalf("parse %q: %v", lines[len(lines)-1], err)
	}
	return m
}

func TestFromContext_FallsBackToDefault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(jsonLogger(&buf).With("service", "s"))
	t.Cleanup(func() { slog.SetDefault(prev) })

	FromContext(context.Background()).Info("hello")

	if got := lastLine(t, &buf); got["service"] != "s" || got["msg"] != "hello" {
		t.Fatalf("entry = %v, want the default logger's fields", got)
	}
}

func TestFromContext_IgnoresNilLogger(t *testing.T) {
	ctx := WithContext(context.Background(), nil)
	if FromContext(ctx) != slog.Default() {
		t.Fatal("a nil logger in ctx must fall back to slog.Default()")
	}
}

func TestWithContext_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf).With("request_id", "r1")
	ctx := WithContext(context.Background(), l)
	if FromContext(ctx) != l {
		t.Fatal("FromContext did not return the stored logger")
	}
}

func TestWith_AddsFieldsWithoutChangingParent(t *testing.T) {
	var buf bytes.Buffer
	parent := WithContext(context.Background(), jsonLogger(&buf).With("request_id", "r1"))
	child := With(parent, "user_id", "u1")

	FromContext(child).Info("child")
	got := lastLine(t, &buf)
	if got["request_id"] != "r1" || got["user_id"] != "u1" {
		t.Fatalf("child entry = %v, want request_id and user_id", got)
	}

	FromContext(parent).Info("parent")
	got = lastLine(t, &buf)
	if _, ok := got["user_id"]; ok {
		t.Fatalf("parent entry = %v, must not gain the child's user_id", got)
	}
}
