package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTP server timeouts, applied to the API server and both metrics servers.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// shutdownTimeout bounds the graceful shutdown of the HTTP servers and the
// background loops after the run context is done.
const shutdownTimeout = 15 * time.Second

// newHTTPServer returns an http.Server for h with the service's timeouts.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func newMetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

// server is an http.Server bound to its listener.
type server struct {
	name string
	srv  *http.Server
	ln   net.Listener
}

// listen binds port (0 picks a free one) for an HTTP server named name. The
// bind happens now, so a port conflict fails startup instead of a goroutine.
func listen(name string, port int, h http.Handler) (*server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("%s server: listen on port %d: %w", name, port, err)
	}
	return &server{name: name, srv: newHTTPServer(h), ln: ln}, nil
}

// serve runs the server in a goroutine until shutdown. A serve error other
// than http.ErrServerClosed is sent on errCh, which must have room for one
// error per server so the goroutine never blocks.
func (s *server) serve(errCh chan<- error) {
	go func() {
		slog.Info(s.name+" server starting", "addr", s.ln.Addr().String())
		if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s server: %w", s.name, err)
		}
	}()
}

// shutdown gracefully stops the server, logging a failure.
func (s *server) shutdown(ctx context.Context) {
	if err := s.srv.Shutdown(ctx); err != nil {
		slog.Error(s.name+" server shutdown error", "error", err)
	}
}
