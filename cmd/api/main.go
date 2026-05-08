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

	"github.com/dlambert-xbp/flowscope/internal/audit"
	"github.com/dlambert-xbp/flowscope/internal/authz"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/services"
	"github.com/dlambert-xbp/flowscope/internal/settings"
	"github.com/dlambert-xbp/flowscope/internal/snmpx"
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

	// Optional SNMP credential store for the Settings → SNMP admin
	// endpoints. Disabled when FLOWSCOPE_SNMP_KEY is unset; the api
	// then surfaces the management endpoints as 503 Service
	// Unavailable so the operator can see why. The same crypter seals
	// the broader Settings secrets (webhook, OIDC client) so we reuse
	// it instead of growing a second secret root.
	var (
		creds   snmpx.CredentialStore
		crypter *snmpx.Crypter
	)
	if mk := os.Getenv("FLOWSCOPE_SNMP_KEY"); mk != "" {
		c, err := snmpx.NewCrypter(mk)
		if err != nil {
			return fmt.Errorf("snmp crypter: %w", err)
		}
		crypter = c
		creds = snmpx.NewClickHouseCredentialStore(conn, crypter)
		slog.Info("snmp credential management enabled")
	} else {
		slog.Warn("FLOWSCOPE_SNMP_KEY not set — snmp credential management endpoints will return 503")
	}

	settingsStore := settings.New(conn, crypter)
	auditWriter := audit.NewClickHouseWriter(conn)
	auditReader := audit.NewClickHouseReader(conn)
	resolver := services.NewResolver()

	// Seed the resolver with whatever's in custom_services right now,
	// then refresh on a 30-second tick to pick up edits from peer api
	// replicas. Per-replica writes already prime locally via
	// h.refreshResolver; the tick is the multi-replica safety net.
	go refreshResolverLoop(ctx, settingsStore, resolver)

	authCfg := authz.Config{
		SharedToken: os.Getenv("FLOWSCOPE_AUTH_TOKEN"),
		Tokens:      settingsStore.APITokens,
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	h := &handlers{
		conn:  conn,
		creds: creds,
		settings: settingsDeps{
			store:    settingsStore,
			resolver: resolver,
			audit:    auditWriter,
			reader:   auditReader,
		},
	}
	r.Get("/healthz", h.health)
	r.Get("/api/summary", h.summary)
	r.Get("/api/flows/recent", h.recentFlows)
	r.Get("/api/devices", h.devices)
	r.Get("/api/devices/{exporter}", h.device)
	r.Get("/api/devices/{exporter}/inventory", h.deviceInventory)
	r.Get("/api/interfaces", h.interfaces)
	r.Get("/api/interfaces/{exporter}/{ifindex}/timeseries", h.interfaceTimeseries)
	r.Get("/api/top/talkers", h.topTalkers)
	r.Get("/api/top/services", h.topServices)
	r.Get("/api/top/protocols", h.topProtocols)
	r.Get("/api/top/conversations", h.topConversations)
	r.Get("/api/alerts", h.alerts)
	r.Get("/api/alerts/summary", h.alertSummary)
	r.Post("/api/alerts/{id}/ack", h.ackAlert)
	r.Post("/api/alerts/{id}/close", h.closeAlert)
	r.Get("/api/snmp/credentials", h.listCredentials)
	r.Get("/api/snmp/credentials/{exporter}", h.getCredential)
	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireWrite())
		r.Put("/api/snmp/credentials/{exporter}", h.putCredential)
		r.Delete("/api/snmp/credentials/{exporter}", h.deleteCredential)
		r.Post("/api/snmp/credentials/{exporter}/test", h.testCredential)
	})

	// Settings & Services. Reads are open (proxy-trust, Phase 1);
	// writes go through the X-Auth-Token middleware. Token CRUD is
	// admin-only because creating a token grants new auth state.
	r.Get("/api/services/lookup", h.servicesLookup)
	r.Get("/api/services/library", h.servicesLibrary)
	r.Get("/api/services/custom", h.listCustomServices)
	r.Get("/api/settings/general", h.listGeneralSettings)
	r.Get("/api/settings/exporters/allowlist", h.listAllowlist)
	r.Get("/api/settings/tokens", h.listTokens)
	r.Get("/api/settings/audit", h.listAudit)
	r.Get("/api/settings/alert-rules", h.listAlertRules)
	r.Get("/api/settings/integrations/webhooks", h.listWebhooks)
	r.Get("/api/settings/oidc", h.getOIDC)
	r.Get("/api/settings/advanced", h.listAdvanced)
	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireWrite())
		r.Put("/api/services/custom", h.putCustomService)
		r.Put("/api/services/custom/{id}", h.putCustomService)
		r.Delete("/api/services/custom/{id}", h.deleteCustomService)
		r.Put("/api/settings/general/{name}", h.putGeneralSetting)
		r.Put("/api/settings/exporters/allowlist/{exporter}", h.putAllowlist)
		r.Delete("/api/settings/exporters/allowlist/{exporter}", h.deleteAllowlist)
		r.Put("/api/settings/alert-rules/{id}", h.putAlertRule)
		r.Put("/api/settings/integrations/webhooks", h.putWebhook)
		r.Put("/api/settings/integrations/webhooks/{id}", h.putWebhook)
		r.Delete("/api/settings/integrations/webhooks/{id}", h.deleteWebhook)
		r.Put("/api/settings/oidc", h.putOIDC)
	})
	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireAdmin())
		r.Post("/api/settings/tokens", h.createToken)
		r.Delete("/api/settings/tokens/{id}", h.revokeToken)
	})

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

// refreshResolverLoop reloads the in-process service-name resolver
// from the custom_services table every 30 seconds. Local writes
// already prime the resolver synchronously via h.refreshResolver, so
// this loop is the multi-replica safety net: if api-A creates a
// custom row, api-B picks it up on the next tick. Cheap query
// (SELECT FINAL on a small table), bounded cost.
func refreshResolverLoop(ctx context.Context, store *settings.Store, resolver *services.Resolver) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	refresh := func() {
		rows, err := store.CustomServices.List(ctx)
		if err != nil {
			return
		}
		entries := make([]services.CustomEntry, 0, len(rows))
		for _, r := range rows {
			entries = append(entries, services.CustomEntry{
				Proto:       r.Proto,
				PortLo:      r.PortLo,
				PortHi:      r.PortHi,
				Name:        r.Name,
				Description: r.Description,
				Group:       r.Group,
				Owner:       r.Owner,
				UpdatedAt:   r.UpdatedAt,
			})
		}
		resolver.SetCustoms(entries)
	}
	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			refresh()
		}
	}
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
