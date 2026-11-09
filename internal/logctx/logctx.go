// Package logctx carries a request- or event-scoped *slog.Logger in a
// context.Context, so every log entry about one request or event shares its
// correlation fields (request_id, event_id, ...). Later trace ids can be
// attached the same way.
package logctx

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// WithContext returns a copy of ctx that carries l.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored in ctx, or slog.Default() when there
// is none.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// With returns a copy of ctx whose logger is ctx's logger with args added.
func With(ctx context.Context, args ...any) context.Context {
	return WithContext(ctx, FromContext(ctx).With(args...))
}
