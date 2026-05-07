package obs

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeMetrics_ExposesPrometheusEndpoint(t *testing.T) {
	// Bind to an ephemeral port, run ServeMetrics, hit /metrics, expect
	// Prometheus exposition format including at least one of our metrics.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- ServeMetrics(ctx, addr) }()

	// Give the server a moment to bind; retry for up to 1s.
	deadline := time.Now().Add(time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Touch a metric so it is exposed.
	UDPPacketsReceived.WithLabelValues("test").Inc()

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "# HELP") {
		t.Errorf("missing prometheus exposition headers: %.200s", s)
	}

	// /healthz responds.
	hr, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	if hr.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d", hr.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}
