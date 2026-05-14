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
	"github.com/dlambert-xbp/flowscope/internal/notifier"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/rdns"
	"github.com/dlambert-xbp/flowscope/internal/secrets"
	"github.com/dlambert-xbp/flowscope/internal/services"
	"github.com/dlambert-xbp/flowscope/internal/sessionsign"
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
	// endpoints. Disabled when no master key is resolvable; the api
	// then surfaces the management endpoints as 503 Service
	// Unavailable so the operator can see why. The same crypter seals
	// the broader Settings secrets (webhook, OIDC client) so we reuse
	// it instead of growing a second secret root. Master key
	// resolution is delegated to internal/secrets — FLOWSCOPE_SNMP_KEY_REF
	// preferred, legacy FLOWSCOPE_SNMP_KEY as a deprecation-warned
	// fallback.
	var (
		creds   snmpx.CredentialStore
		crypter *snmpx.Crypter
	)
	mk, err := secrets.ResolveSNMPMaster(ctx)
	if err != nil {
		return fmt.Errorf("snmp master key: %w", err)
	}
	if mk != "" {
		c, err := snmpx.NewCrypter(mk)
		if err != nil {
			return fmt.Errorf("snmp crypter: %w", err)
		}
		crypter = c
		creds = snmpx.NewClickHouseCredentialStore(conn, crypter)
		slog.Info("snmp credential management enabled",
			"master_fp", secrets.Fingerprint(mk),
		)
	} else {
		slog.Warn("FLOWSCOPE_SNMP_KEY_REF / FLOWSCOPE_SNMP_KEY not set — snmp credential management endpoints will return 503")
	}

	settingsStore := settings.New(conn, crypter)
	auditWriter := audit.NewClickHouseWriter(conn)
	auditReader := audit.NewClickHouseReader(conn)
	resolver := services.NewResolver()

	// Phase 2 OIDC login: optional. Wired when FLOWSCOPE_SESSION_KEY_REF
	// (or legacy FLOWSCOPE_SESSION_KEY) is set. Independent root from
	// the SNMP master key — rotating one does not disturb the other.
	var (
		signer  *sessionsign.Signer
		sessAdp *sessionAdapter
	)
	sessKey, err := secrets.ResolveSessionKey(ctx)
	if err != nil {
		return fmt.Errorf("session key: %w", err)
	}
	if sessKey != "" {
		s, err := sessionsign.New(sessKey)
		if err != nil {
			return fmt.Errorf("session signer: %w", err)
		}
		signer = s
		sessAdp = &sessionAdapter{signer: signer, cookieName: sessionCookieName}
		slog.Info("oidc login enabled (Phase 2)",
			"session_key_fp", secrets.Fingerprint(sessKey),
		)
	} else {
		slog.Info("oidc login disabled (no FLOWSCOPE_SESSION_KEY_REF / FLOWSCOPE_SESSION_KEY)")
	}

	// Seed the resolver with whatever's in custom_services right now,
	// then refresh on a 30-second tick to pick up edits from peer api
	// replicas. Per-replica writes already prime locally via
	// h.refreshResolver; the tick is the multi-replica safety net.
	go refreshResolverLoop(ctx, settingsStore, resolver)

	authCfg := authz.Config{
		SharedToken: os.Getenv("FLOWSCOPE_AUTH_TOKEN"),
		Tokens:      settingsStore.APITokens,
	}
	// Wire session source only when the signer is built. nil leaves the
	// existing shared/per-token paths unchanged.
	if sessAdp != nil {
		authCfg.Sessions = sessAdp
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)

	// Webhook test endpoint reuses the dispatcher's delivery pipeline.
	// Only available when crypter is configured because endpoint
	// secrets are sealed. The dispatcher itself runs in cmd/alert, not
	// here — this is a lightweight, never-Run() instance used only for
	// SendTest.
	var testDispatcher *notifier.Dispatcher
	if crypter != nil {
		testDispatcher = notifier.New(conn, crypter, auditWriter)
	}

	h := &handlers{
		conn:           conn,
		creds:          creds,
		crypter:        crypter,
		testDispatcher: testDispatcher,
		settings: settingsDeps{
			store:    settingsStore,
			resolver: resolver,
			audit:    auditWriter,
			reader:   auditReader,
		},
		// FLOWSCOPE_INGEST_HEALTH_URL points at the ingest service's
		// /health/ingest endpoint. Default to the well-known docker-
		// compose + Helm hostname so the out-of-box dashboard isn't
		// stuck on the "not configured" placeholder. Operators with
		// non-standard hostnames override via env.
		ingestHealthURL: envOr("FLOWSCOPE_INGEST_HEALTH_URL", "http://flowscope-ingest:9100/health/ingest"),
		ingestHealthHTTP: &http.Client{
			Timeout: 3 * time.Second,
		},
		rdns: rdns.New(rdns.Options{}),
		auth: authDeps{
			signer:  signer,
			store:   settingsStore.OIDC,
			crypter: crypter,
		},
	}
	// /healthz is the k8s liveness probe — never gated. Same for the
	// static dashboard mount and the /metrics scrape endpoint below.
	r.Get("/healthz", h.health)

	// /api/config/effective is the unauthenticated bootstrap call the
	// SPA makes before it has a chance to attach the X-Auth-Token
	// header (the token is loaded from localStorage on first render).
	// It returns brand / theme defaults only, no flow data.
	r.Get("/api/config/effective", h.effectiveConfig)

	// OIDC Phase 2 endpoints — all unauthenticated by design (users
	// hitting /auth/login don't yet have a session). The cookie
	// minted by /auth/callback is what later requests authenticate
	// with via the authz.SessionSource wired above.
	r.Get("/auth/login", h.authLogin)
	r.Get("/auth/callback", h.authCallback)
	r.Post("/auth/logout", h.authLogout)
	r.Get("/auth/me", h.authMe)

	// Phase 1 read gate. Every GET that exposes flow / topology /
	// alert / SNMP-derived data — plus the /api/health/* operator
	// views and /api/dns/lookup, which surface flow-derived counters
	// and reverse-DNS for in-flight addresses — is wrapped in
	// RequireRead so the X-Auth-Token check is enforced when a
	// SharedToken (or a per-token store) is configured. When neither
	// is configured the middleware lets the request through and
	// stamps subject "unauth-bypass" for audit visibility — same
	// behaviour as the write group below.
	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireRead())
		r.Get("/api/summary", h.summary)
		r.Get("/api/health/streams", h.healthStreams)
		r.Get("/api/health/storage", h.healthStorage)
		r.Get("/api/health/exporters", h.healthExporters)
		r.Get("/api/health/ingest", h.healthIngest)
		r.Get("/api/dns/lookup", h.dnsLookup)
		r.Get("/api/flows/recent", h.recentFlows)
		r.Get("/api/flows/list", h.flowsList)
		r.Get("/api/flows/timeseries", h.flowsTimeseries)
		r.Get("/api/flows/flags-timeseries", h.flowsFlagsTimeseries)
		r.Get("/api/devices", h.devices)
		r.Get("/api/devices/{exporter}", h.device)
		r.Get("/api/devices/{exporter}/inventory", h.deviceInventory)
		r.Get("/api/devices/{exporter}/resources", h.deviceResources)
		r.Get("/api/devices/{exporter}/neighbors", h.deviceNeighbors)
		r.Get("/api/devices/{exporter}/bgp", h.deviceBGP)
		r.Get("/api/topology", h.topology)
		r.Get("/api/interfaces", h.interfaces)
		r.Get("/api/interfaces/{exporter}/{ifindex}/timeseries", h.interfaceTimeseries)
		r.Get("/api/top/talkers", h.topTalkers)
		r.Get("/api/top/services", h.topServices)
		r.Get("/api/top/protocols", h.topProtocols)
		r.Get("/api/top/conversations", h.topConversations)
		r.Get("/api/top/asn", h.topASN)
		r.Get("/api/top/interfaces", h.topInterfaces)
		r.Get("/api/alerts", h.alerts)
		r.Get("/api/alerts/summary", h.alertSummary)
		r.Get("/api/alerts/templates", h.listAlertTemplates)
		r.Get("/api/alerts/instances", h.listAlertInstances)
		r.Get("/api/alerts/instances/{id}", h.getAlertInstance)
		r.Get("/api/alerts/{id}", h.alertDetail)
		// Alert ack/close are POSTs but treated as read-tier
		// mutations — they flip alert state for the operator
		// viewing the dashboard, they don't change auth or
		// configuration. A read-scoped token is enough; a write-
		// or admin-scoped token also works (admin > write > read).
		r.Post("/api/alerts/{id}/ack", h.ackAlert)
		r.Post("/api/alerts/{id}/close", h.closeAlert)
		r.Get("/api/snmp/credentials", h.listCredentials)
		r.Get("/api/snmp/credentials/{exporter}", h.getCredential)
		r.Get("/api/snmp/globals/{role}", h.getGlobalCredential)
		r.Get("/api/services/lookup", h.servicesLookup)
		r.Get("/api/services/library", h.servicesLibrary)
		r.Get("/api/services/custom", h.listCustomServices)
	})

	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireWrite())
		r.Put("/api/snmp/credentials/{exporter}", h.putCredential)
		r.Delete("/api/snmp/credentials/{exporter}", h.deleteCredential)
		r.Post("/api/snmp/credentials/{exporter}/test", h.testCredential)
		r.Put("/api/snmp/globals/{role}", h.putGlobalCredential)
		r.Post("/api/devices/{exporter}/snmp/walk", h.requestSnmpWalk)
	})

	// Settings reads stay on the proxy-trust path for now — the
	// Settings UI is operator-only and the writes (below) already
	// require X-Auth-Token. Folding settings GETs under RequireRead
	// is a follow-up so this PR stays scoped to flow / alert / topo
	// reads.
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
		r.Post("/api/alerts/instances", h.createAlertInstance)
		r.Put("/api/alerts/instances/{id}", h.updateAlertInstance)
		r.Delete("/api/alerts/instances/{id}", h.deleteAlertInstance)
		r.Post("/api/alerts/instances/{id}/preview", h.previewAlertInstance)
		r.Post("/api/alerts/instances/preview", h.previewAlertInstanceDryRun)
		r.Put("/api/settings/integrations/webhooks", h.putWebhook)
		r.Put("/api/settings/integrations/webhooks/{id}", h.putWebhook)
		r.Delete("/api/settings/integrations/webhooks/{id}", h.deleteWebhook)
		r.Post("/api/settings/integrations/webhooks/{id}/test", h.testWebhook)
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
