package serve

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// startServer listens on a free loopback port and closes the server when the
// test ends.
func startServer(t *testing.T, stallAfter time.Duration) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1:0", stallAfter)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// get fetches path from s and returns the response and its body.
func get(t *testing.T, s *Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get("http://" + s.Addr() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return resp, string(body)
}

func TestHealthzOK(t *testing.T) {
	s := startServer(t, time.Hour)
	resp, body := get(t, s, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
}

func TestHealthzStalled(t *testing.T) {
	s := startServer(t, time.Nanosecond)
	time.Sleep(time.Millisecond)
	resp, body := get(t, s, "/healthz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if want := "stalled: no operation or iteration finished for"; !strings.HasPrefix(body, want) {
		t.Errorf("body = %q, want prefix %q", body, want)
	}
}

func TestMetricsEmptyRegistry(t *testing.T) {
	s := startServer(t, time.Hour)
	resp, body := get(t, s, "/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

// constCollector is an unchecked collector, as the OpenTelemetry Prometheus
// exporter's is: it describes nothing and emits one constant gauge.
type constCollector struct{ desc *prometheus.Desc }

func (constCollector) Describe(chan<- *prometheus.Desc) {}

func (c constCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
}

func TestMetricsServesOnGatherError(t *testing.T) {
	s := startServer(t, time.Hour)
	desc := prometheus.NewDesc("dizzy_test_constant", "A constant for the test.", nil, nil)
	// Two collectors emitting the same series make Gather fail with a
	// duplicate-metric error.
	for range 2 {
		if err := s.Registerer().Register(constCollector{desc}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	resp, body := get(t, s, "/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var samples int
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "dizzy_test_constant ") {
			samples++
		}
	}
	if samples != 1 {
		t.Errorf("dizzy_test_constant samples = %d, want 1; body:\n%s", samples, body)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	s := startServer(t, time.Hour)
	for _, path := range []string{"/", "/health"} {
		if resp, _ := get(t, s, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestListenBusyAddress(t *testing.T) {
	s := startServer(t, time.Hour)
	_, err := Listen(s.Addr(), time.Hour)
	if err == nil {
		t.Fatal("second Listen on a held address: expected an error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "listening on ") {
		t.Errorf("error = %q, want prefix %q", err, "listening on ")
	}
	if !errors.As(err, new(*net.OpError)) {
		t.Errorf("error = %v, want it to wrap a *net.OpError", err)
	}
}

func TestListenInvalidAddress(t *testing.T) {
	_, err := Listen("not-an-address", 0)
	if err == nil {
		t.Fatal("Listen on an invalid address: expected an error, got nil")
	}
	if want := "listening on not-an-address: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %q, want prefix %q", err, want)
	}
}
