// Package app wires and runs the authz-api and authz-worker processes.
// cmd/api and cmd/worker only load the config, handle signals, and call
// RunAPI or RunWorker.
package app

import (
	"io"
	"log/slog"
)

// Service names, logged as the "service" field of every log entry.
const (
	ServiceAPI    = "authz-api"
	ServiceWorker = "authz-worker"
)

// NewLogger returns the base JSON logger for service. Every entry it writes,
// and every logger derived from it, carries service=<service>.
func NewLogger(w io.Writer, service string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil)).With("service", service)
}
