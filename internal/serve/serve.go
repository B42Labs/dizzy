// Package serve is the opt-in HTTP listener of dizzy. It serves the metrics
// that the telemetry package feeds into its registry on /metrics, in the
// Prometheus exposition format, and a liveness check on /healthz that turns
// unhealthy once the process stopped making progress (see Health).
//
// The listener speaks plain HTTP without authentication; the operator limits
// who reaches it through the bind address.
package serve

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is a running listener with its metrics registry and health tracker.
type Server struct {
	addr     string
	registry *prometheus.Registry
	health   *Health
	srv      *http.Server
}

// Listen binds addr, starts serving /metrics and /healthz in the background,
// and returns the running server. The health tracker turns unhealthy after
// stallAfter without progress; 0 disables that check. A bind failure is
// returned as "listening on <addr>: <cause>". The server runs until the
// process exits or Close is called.
func Listen(addr string, stallAfter time.Duration) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}

	s := &Server{
		addr:     ln.Addr().String(),
		registry: prometheus.NewRegistry(),
		health:   NewHealth(stallAfter, time.Now),
	}

	mux := http.NewServeMux()
	// A failing gather must not blank the scrape: ContinueOnError serves
	// what was gathered and logs the error, as an export failure degrades
	// to a warning.
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}))
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics listener stopped", "error", err)
		}
	}()
	return s, nil
}

// handleHealthz answers 200 "ok" while the process makes progress and 503
// with the idle time and the limit once it stalled.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	healthy, idle := s.health.Check()
	if healthy {
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	slog.Warn("health check failing", "idle", idle, "limit", s.health.stallAfter)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, "stalled: no operation or iteration finished for %s (limit %s)\n",
		idle.Round(time.Second), s.health.stallAfter.Round(time.Second))
}

// Addr returns the bound address, e.g. 127.0.0.1:40123 for ":0".
func (s *Server) Addr() string { return s.addr }

// Registerer returns the server's own registry, which /metrics gathers.
func (s *Server) Registerer() prometheus.Registerer { return s.registry }

// Health returns the server's health tracker.
func (s *Server) Health() *Health { return s.health }

// Close stops the listener. dizzy itself never calls it: the listener
// serves until the process exits, so a scrape during teardown still works.
func (s *Server) Close() error { return s.srv.Close() }
