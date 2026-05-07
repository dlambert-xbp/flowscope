// Command api is the FlowScope read-side service.
//
// It serves the JSON REST contract documented in api/openapi.yaml and
// streams live updates over WebSocket. All endpoints are stateless;
// data flows in from the ClickHouse cluster (warm + cold tiers) plus,
// in a later slice, the WebSocket pump from cmd/ingest.
//
// In v1 the api binary also serves a minimal live HTML dashboard at /
// so operators can see the system working before the React app lands.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("FLOWSCOPE_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	httpAddr := envOr("FLOWSCOPE_HTTP_ADDR", ":8080")
	chDSN := envOr("FLOWSCOPE_CLICKHOUSE_DSN", "")
	if chDSN == "" {
		return errors.New("FLOWSCOPE_CLICKHOUSE_DSN is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	conn, err := store.Open(ctx, chDSN)
	if err != nil {
		return fmt.Errorf("clickhouse open: %w", err)
	}
	defer conn.Close()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	h := &handlers{conn: conn}
	r.Get("/healthz", h.health)
	r.Get("/api/summary", h.summary)
	r.Get("/api/flows/recent", h.recentFlows)
	r.Get("/api/interfaces", h.interfaces)
	r.Get("/api/interfaces/{exporter}/{ifindex}/timeseries", h.interfaceTimeseries)
	r.Method("GET", "/metrics", obs.Handler())

	// Live HTML dashboard at /. Served from embedded assets so the
	// binary is self-contained.
	r.Mount("/", staticHandler())

	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("flowscope api started", "addr", httpAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http: %w", err)
	}
	return nil
}

// requestLogger emits one structured log entry per request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds(),
			"req_id", middleware.GetReqID(r.Context()),
		)
	})
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
