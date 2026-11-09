package api

import (
	"context"
	"net/http"
	"time"

	"authz-service/internal/logctx"

	"github.com/google/uuid"
)

type contextKey string

const (
	requestIDKey  contextKey = "request_id"
	requestLogKey contextKey = "request_log"
)

// RequestIDMiddleware takes the request ID from X-Request-ID, or generates a
// UUID v7, and sets it on the response header and in the context. The
// context's logger (logctx) carries it as request_id.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			// NewV7 fails only when the random source does; fall back to v4.
			if uid, err := uuid.NewV7(); err == nil {
				id = uid.String()
			} else {
				id = uuid.NewString()
			}
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		ctx = logctx.With(ctx, "request_id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestLog collects attributes that handlers learn while serving a request
// (user_id), so the access log line written after the handler returns
// includes them. A request is served on one goroutine, so it needs no lock.
type requestLog struct {
	attrs []any
}

// LoggingMiddleware logs each request through the context's logger, so the
// line carries request_id, plus user_id when a handler recorded it with
// withUserID.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rl := &requestLog{}
		r = r.WithContext(context.WithValue(r.Context(), requestLogKey, rl))
		ww := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		logctx.FromContext(r.Context()).With(rl.attrs...).Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withUserID records the request's user id for the access log line and
// returns r with a context logger that carries user_id, so every later log
// entry for the request has it.
func withUserID(r *http.Request, userID uuid.UUID) *http.Request {
	id := userID.String()
	if rl, ok := r.Context().Value(requestLogKey).(*requestLog); ok {
		rl.attrs = append(rl.attrs, "user_id", id)
	}
	return r.WithContext(logctx.With(r.Context(), "user_id", id))
}

// RecoveryMiddleware recovers from panics and returns 500.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				logctx.FromContext(r.Context()).Error("panic recovered", "error", err, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
