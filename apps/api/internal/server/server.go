package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/handler"
	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/quota"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/service/backup"
	"github.com/belune-hq/belune/internal/service/email"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/terminal"
	"github.com/belune-hq/belune/internal/ws"
	"github.com/belune-hq/belune/web"
)

type Server struct {
	cfg     *config.Config
	db      *pgxpool.Pool
	router  chi.Router
	handler *handler.Handler
	auth    *service.AuthService
	tokens  *service.TokenService
}

func New(cfg *config.Config, db *pgxpool.Pool, queries *generated.Queries, asynqClient handler.TaskEnqueuer, inspector handler.QueueInspector, rts runtime.Runtimes, pm proxy.ProxyManager, reconciler handler.ReconcilerStatusProvider, rdb *redis.Client, hub *ws.Hub, auditSvc *service.AuditService, notifySvc *service.NotificationService, termMgr *terminal.Manager, emailSvc *email.Service) *Server {
	auth := service.NewAuthService(queries, cfg.JWTSecret, cfg.JWTExpiryHours, cfg.JWTRefreshHours, rdb)
	tokens := service.NewTokenService(db, queries)
	backupDestSvc := service.NewBackupDestinationService(queries, cfg.Keyring)
	// appSvc needs backupDestSvc to erase volume-backup objects on delete, so it
	// is built after it.
	appSvc := service.NewApplicationService(db, queries, rts, cfg.Keyring, cfg.FileMountsDir, backupDestSvc)
	dbSvc := service.NewDatabaseService(db, queries, rts, backup.New(cfg), backupDestSvc)
	// projSvc delegates project deletion to appSvc/dbSvc, so it is built after them.
	projSvc := service.NewProjectService(queries, rts, appSvc, dbSvc)
	gitProviderSvc := service.NewGitProviderConfigService(queries, cfg.Keyring)
	gitIntegrationSvc := service.NewGitIntegrationService(queries, cfg.Keyring, gitProviderSvc)
	quotaSvc := quota.NewService(queries)

	// The login challenge verifies factors through this, so it must be wired
	// before any request is served: a user with TOTP enabled cannot log in
	// while it is missing, which is the correct way for this to fail.
	auth.SetSecondFactorVerifier(service.NewTOTPService(db, queries, cfg.Keyring))

	s := &Server{
		cfg:     cfg,
		db:      db,
		auth:    auth,
		tokens:  tokens,
		handler: handler.New(cfg, db, queries, asynqClient, inspector, rts, pm, reconciler, auth, rdb, appSvc, projSvc, dbSvc, gitProviderSvc, gitIntegrationSvc, backupDestSvc, hub, auditSvc, notifySvc, termMgr, quotaSvc, emailSvc, tokens),
	}

	s.router = s.setupRouter()
	return s
}

// SetCapabilities wires the Caddy capability probe into the handlers. Called
// once at startup, before the server accepts requests.
func (s *Server) SetCapabilities(p capability.Provider) {
	s.handler.SetCapabilities(p)
}

// SetAccessLog wires the request-log tailer's status into the handlers. Called
// once at startup, before the server accepts requests.
func (s *Server) SetAccessLog(p handler.AccessLogStatusProvider) {
	s.handler.SetAccessLog(p)
}

// SetWAF wires the WAF recorder's counters into the handlers. Called once at
// startup, before the server accepts requests.
func (s *Server) SetWAF(p handler.WAFStatsProvider) {
	s.handler.SetWAF(p)
}

// SetCrowdSec wires the CrowdSec service into the handlers. Called once at startup,
// before the server accepts requests.
func (s *Server) SetCrowdSec(svc *crowdsec.Service) {
	s.handler.SetCrowdSec(svc)
}

// SetProxyAdmin wires the read-only Caddy admin reader into the handlers.
func (s *Server) SetProxyAdmin(a handler.ProxyAdminReader) {
	s.handler.SetProxyAdmin(a)
}

func (s *Server) Router() chi.Router {
	return s.router
}

func (s *Server) setupRouter() chi.Router {
	r := chi.NewRouter()

	// Global middleware
	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.RealIP)
	// OTel HTTP server middleware: one span per request plus W3C traceparent
	// extraction. No-ops when the tracer provider is the default no-op.
	r.Use(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "http.request")
	})
	// Prometheus HTTP counters/histograms. Mounted before the request logger so
	// latency observations include logger overhead (negligible, simpler to
	// reason about).
	r.Use(metrics.HTTPMiddleware)
	r.Use(middleware.Logger)
	r.Use(chiMiddleware.Recoverer)
	if s.cfg.TLS {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
				next.ServeHTTP(w, r)
			})
		})
	}
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data: https:; "+
					"connect-src 'self' ws: wss:; "+
					"font-src 'self'; "+
					"object-src 'none'; "+
					"frame-ancestors 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			next.ServeHTTP(w, r)
		})
	})
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   s.cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Register API + health routes
	registerRoutes(r, s.handler, s.auth, s.tokens, s.cfg.DisableRateLimiting)

	// Catch-all: serve the embedded SPA for any unmatched path — except under
	// /api/, where an unmatched path is a client error, not a page.
	//
	// Registered unconditionally, including when the SPA is absent. Gating the
	// whole catch-all on web.Handler() != nil made the API's 404 contract
	// depend on whether the frontend happened to be built: in a binary without
	// it — every test binary, since web/dist holds only .gitkeep — chi's default
	// answered "404 page not found" as text/plain instead. Production always
	// embeds the SPA, so this was invisible there, which is exactly what made it
	// worth removing.
	r.Handle("/*", apiAwareCatchAll(web.Handler()))

	return r
}

// apiAwareCatchAll serves the SPA for unmatched paths, but answers an unmatched
// /api/ path with the same JSON 404 the rest of the API returns.
//
// The SPA handler is deliberately a catch-all: any path that does not resolve to
// a built asset gets index.html, which is what makes client-side routing work.
// The side effect was that it also swallowed every unmatched /api/ path, so a
// caller of a typo'd or renamed endpoint got 200 text/html — a SUCCESS status
// and an HTML page, which most clients fail to parse in some confusing way
// rather than reporting "not found". It also quietly undercut the API
// reference: the spec says which paths exist, and probing a wrong one did not
// disagree.
//
// Wrapping the existing catch-all rather than registering an /api/* route: by
// the time a request reaches here chi has already failed to match every real
// route, so no registered API path can be shadowed by this. A second wildcard
// pattern could shadow one, and would fail in the worst possible direction.
//
// spa may be nil when the frontend was not built into the binary. The /api/
// branch still applies — that answer is the API's own contract and does not
// depend on the frontend existing — and everything else gets the plain 404 chi
// would have produced anyway.
func apiAwareCatchAll(spa http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bare "/api" as well as "/api/...": both are unambiguously an attempt
		// to reach the API, and neither is a page.
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			// Same shape as handler.writeError, which is package-private —
			// duplicated rather than exported, since exporting it would widen
			// that package's surface for one caller.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			if err := json.NewEncoder(w).Encode(map[string]string{
				"error": "no such endpoint",
			}); err != nil {
				slog.Debug("api 404: encode error", "error", err)
			}
			return
		}
		if spa == nil {
			http.NotFound(w, r)
			return
		}
		spa.ServeHTTP(w, r)
	})
}
