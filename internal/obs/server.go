package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServeMetrics runs an HTTP server on addr that exposes /metrics for
// Prometheus scraping plus a /healthz liveness probe. It blocks until
// ctx is cancelled, then performs a graceful shutdown.
//
// Use this in services that don't already host an HTTP server (e.g.
// cmd/ingest). cmd/api adds the same handler to its existing chi
// router via Handler().
func ServeMetrics(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("metrics server started", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics http: %w", err)
	}
	return nil
}

// Handler returns the Prometheus /metrics http.Handler. cmd/api mounts
// this on its chi router; cmd/ingest uses it via ServeMetrics.
func Handler() http.Handler {
	return promhttp.Handler()
}
