package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/mcpserver"
	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/caddyinfo"
	"github.com/belune-hq/belune/internal/proxy/caddymetrics"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/quota"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/service/email"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/terminal"
	"github.com/belune-hq/belune/internal/ws"
)

// TaskEnqueuer abstracts task queue operations for testability.
type TaskEnqueuer interface {
	Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// QueueInspector is the narrow slice of *asynq.Inspector the handler uses for
// queue maintenance: reporting depth and clearing dead-letter/retry tasks.
// Concrete impl is *asynq.Inspector; mocked in tests.
type QueueInspector interface {
	GetQueueInfo(queue string) (*asynq.QueueInfo, error)
	DeleteAllArchivedTasks(queue string) (int, error)
	DeleteAllRetryTasks(queue string) (int, error)
	// DeleteAllPendingTasks removes queued-but-not-started tasks. It never
	// touches active (running) tasks, so an in-flight deploy survives.
	DeleteAllPendingTasks(queue string) (int, error)
	// DeleteTask removes a single task by id. It fails for an active (running)
	// task, which lets callers distinguish a genuinely in-progress task from a
	// stale one (pending/retry/archived) holding a unique TaskID.
	DeleteTask(queue, id string) error
}

// ReconcilerStatusProvider is the narrow interface the handler consumes to
// surface proxy reconciler state and trigger an on-demand reconcile from the
// admin API. Concrete type lives in internal/proxy.
type ReconcilerStatusProvider interface {
	Status() proxy.ReconcilerStatus
	ReconcileNow(ctx context.Context) error
	// WAF is what the last pass did about the WAF route.
	WAF() proxy.WAFState
	// CrowdSec is what the last pass did about the bouncer.
	CrowdSec() proxy.CrowdSecState
}

type Handler struct {
	// updateStarting is set from TriggerSelfUpdate's 202 until its background
	// goroutine has started the helper or given up — the stretch in which no
	// helper container exists yet for the 409 check to see.
	updateStarting atomic.Bool
	// updateBeganAt (unix nanos) is when updateStarting was last taken, for the
	// public flag's elapsed time during the pull window.
	updateBeganAt atomic.Int64
	updateProbe   updateProbe
	cfg           *config.Config
	db            *pgxpool.Pool
	queries       *generated.Queries
	asynq         TaskEnqueuer
	inspector     QueueInspector
	runtimes      runtime.Runtimes
	proxy         proxy.ProxyManager
	reconciler    ReconcilerStatusProvider
	// capabilities reports which optional Caddy modules exist. Nil until wired
	// (tests, a Caddy we do not manage) and then treated as "not known", never as
	// "none present": see capabilitySet.
	capabilities capability.Provider
	// proxyAdmin reads Caddy's metrics and live config; nil when not wired.
	proxyAdmin   ProxyAdminReader
	proxyMetrics *caddymetrics.Observer
	// accessLog reports the request-log tailer's health; nil until wired.
	accessLog AccessLogStatusProvider
	// wafExclusions owns the operator's carve-outs from the WAF rule set.
	wafExclusions *service.WAFExclusionService
	// waf reports the WAF recorder's counters; nil until wired.
	waf WAFStatsProvider
	// crowdsec drives the CrowdSec local API; nil until wired.
	crowdsec *crowdsec.Service
	// caddyInspector is built on first use and kept: it holds the version cache.
	caddyInspector     *caddyinfo.Inspector
	caddyInspectorOnce sync.Once
	auth               *service.AuthService
	rdb                *redis.Client
	appService         *service.ApplicationService
	projService        *service.ProjectService
	dbService          *service.DatabaseService
	gitProviderSvc     *service.GitProviderConfigService
	gitIntegrationSvc  *service.GitIntegrationService
	backupDestSvc      *service.BackupDestinationService
	hub                *ws.Hub
	auditSvc           *service.AuditService
	deployQueue        *service.DeployQueue
	notifySvc          *service.NotificationService
	termManager        *terminal.Manager
	// docker system df is far too slow to run inside a request (33s on a small
	// VPS), so the Docker overview reads it from here.
	diskUsage diskUsageCache
	quotaSvc  *quota.Service
	emailSvc  *email.Service
	// certSvc is built here rather than injected: it needs only queries and the
	// keyring, both already held above, and nothing outside the handler uses it.
	certSvc *service.CertificateService
	// dnsProviderSvc is built here like certSvc: queries and the keyring are all it needs.
	dnsProviderSvc *service.DNSProviderService
	// notifyChannelSvc is built here (like certSvc) from queries, keyring and the
	// email service. The worker holds its own equivalent instance for delivery.
	notifyChannelSvc *service.NotificationChannelService
	// smtpSettingsSvc reads/writes DB-backed SMTP config; the same logic is wired
	// as the email service's resolver in app.go.
	smtpSettingsSvc *service.SMTPSettingsService
	// serverSvc is built here (like certSvc) from queries alone. It resolves the
	// local server row that new projects are placed on.
	serverSvc *service.ServerService
	// totpSvc owns the second factor. It is also handed to the auth service as
	// its verifier, so the login challenge and these endpoints agree on what
	// counts as a valid factor.
	totpSvc *service.TOTPService
	// tokenSvc mints personal access tokens for the CRUD endpoints below. The
	// same instance is also wired into middleware.Auth (in server.go) for the
	// Bearer PAT branch — one TokenService, two callers.
	tokenSvc *service.TokenService
	// mcpHandler serves the read-only MCP server at POST /mcp. Built once
	// here (like certSvc) from queries alone — its tools resolve identity
	// per-call from the request context, not from anything held on Handler.
	mcpHandler http.Handler
}

func New(
	cfg *config.Config,
	db *pgxpool.Pool,
	queries *generated.Queries,
	asynqClient TaskEnqueuer,
	inspector QueueInspector,
	rts runtime.Runtimes,
	pm proxy.ProxyManager,
	reconciler ReconcilerStatusProvider,
	auth *service.AuthService,
	rdb *redis.Client,
	appSvc *service.ApplicationService,
	projSvc *service.ProjectService,
	dbSvc *service.DatabaseService,
	gitProviderSvc *service.GitProviderConfigService,
	gitIntegrationSvc *service.GitIntegrationService,
	backupDestSvc *service.BackupDestinationService,
	hub *ws.Hub,
	auditSvc *service.AuditService,
	notifySvc *service.NotificationService,
	termMgr *terminal.Manager,
	quotaSvc *quota.Service,
	emailSvc *email.Service,
	tokenSvc *service.TokenService,
) *Handler {
	// Shared with the MCP tools so a deploy started by either takes the same
	// TaskID guard, retry policy and stale-task reclaim.
	deployQueue := service.NewDeployQueue(queries, asynqClient, inspector, cfg.TaskTimeoutMinutes)
	h := &Handler{
		cfg:               cfg,
		db:                db,
		queries:           queries,
		asynq:             asynqClient,
		inspector:         inspector,
		runtimes:          rts,
		wafExclusions:     service.NewWAFExclusionService(queries),
		proxy:             pm,
		reconciler:        reconciler,
		auth:              auth,
		rdb:               rdb,
		appService:        appSvc,
		projService:       projSvc,
		dbService:         dbSvc,
		gitProviderSvc:    gitProviderSvc,
		gitIntegrationSvc: gitIntegrationSvc,
		backupDestSvc:     backupDestSvc,
		hub:               hub,
		auditSvc:          auditSvc,
		deployQueue:       deployQueue,
		notifySvc:         notifySvc,
		termManager:       termMgr,
		quotaSvc:          quotaSvc,
		emailSvc:          emailSvc,
		certSvc:           service.NewCertificateService(queries, cfg.Keyring),
		dnsProviderSvc:    service.NewDNSProviderService(queries, cfg.Keyring),
		notifyChannelSvc:  service.NewNotificationChannelService(queries, cfg.Keyring, service.NewNotifyRegistry(emailSvc), cfg.PublicBaseURL),
		smtpSettingsSvc:   service.NewSMTPSettingsService(queries, cfg.Keyring, cfg),
		serverSvc:         service.NewServerService(queries),
		totpSvc:           service.NewTOTPService(db, queries, cfg.Keyring),
		tokenSvc:          tokenSvc,
	}

	// ⚠️ Built after h, not inside the literal above: create_database needs
	// h itself as its DatabaseProvisioner, so the Deps cannot be assembled
	// until h exists.
	h.mcpHandler = mcpserver.New(mcpserver.Deps{
		Queries:   queries,
		Runtimes:  rts,
		Audit:     mcpAuditor(auditSvc),
		Deploys:   deployQueue,
		Apps:      appSvc,
		Env:       service.NewEnvVarService(db, queries, cfg.Keyring, appSvc),
		Quotas:    quotaSvc,
		DBs:       dbSvc,
		Databases: h,
		Alerts:    h,
	})
	return h
}

// runtimeForApplication and runtimeForDatabase resolve the host a resource is
// placed on. Handlers that already hold a row carrying server_id should call
// h.runtimes.For with it instead — these exist for the paths that do not.
func (h *Handler) runtimeForApplication(ctx context.Context, appID pgtype.UUID) (runtime.ContainerRuntime, error) {
	return service.RuntimeForApplication(ctx, h.queries, h.runtimes, appID)
}

func (h *Handler) runtimeForDatabase(ctx context.Context, dbID pgtype.UUID) (runtime.ContainerRuntime, error) {
	return service.RuntimeForDatabase(ctx, h.queries, h.runtimes, dbID)
}

func (h *Handler) runtimeForProject(ctx context.Context, projectID pgtype.UUID) (runtime.ContainerRuntime, error) {
	return service.RuntimeForProject(ctx, h.queries, h.runtimes, projectID)
}

// mcpAuditor avoids handing mcpserver a non-nil interface wrapping a nil
// *AuditService, which would panic on first use rather than skip like
// Handler.audit does.
func mcpAuditor(s *service.AuditService) mcpserver.Auditor {
	if s == nil {
		return nil
	}
	return s
}

// audit is a nil-safe wrapper for audit logging. Extracts user ID, the
// authenticating PAT's id (empty for a session), and real client IP from the
// request.
func (h *Handler) audit(r *http.Request, action, resourceType, resourceID string, details map[string]any) {
	if h.auditSvc != nil {
		userID := middleware.UserIDFromContext(r.Context())
		tokenID := middleware.TokenIDFromContext(r.Context())
		h.auditSvc.Log(userID, tokenID, middleware.ClientIP(r), action, resourceType, resourceID, details)
	}
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Debug("writeJSON: encode error", "error", err)
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeServerError answers a 500 AND logs the cause, which writeError cannot
// do because it never receives one.
//
// Most 500s here used to discard their reason at the point it was known: the
// operator saw "failed to list scheduled tasks" and the server log held
// nothing at all, so a dropped database column was indistinguishable from a
// dead connection. Diagnosing one such 500 took a schema diff against a
// freshly migrated database, because the cause had simply been thrown away.
//
// The caller's message stays the response body — it is all a client should
// see — and the cause goes to the log with the request id, so it can be tied
// to the access-log line for the same request.
//
// ⛔ This is for a site that logs NOTHING. A handler that already logs its own
// error with a more specific message than the one the client gets should keep
// doing that and call writeError: one log line per 500, and the better one
// wins.
func writeServerError(w http.ResponseWriter, r *http.Request, message string, err error) {
	attrs := []any{
		"method", r.Method,
		"path", redact.Path(r.URL.Path),
		"request_id", chiMiddleware.GetReqID(r.Context()),
	}
	// Not every 500 has an error to carry; a handler may refuse for a reason it
	// computed itself. Log it either way — which handler answered 500 was not
	// knowable from the log before.
	if err != nil {
		// Belt and braces: logger.RedactHandler scrubs records in production,
		// but this package's own convention is to redact before logging, and a
		// differently-configured handler must not change what reaches disk.
		attrs = append(attrs, "error", redact.Error(err.Error()))
	}
	slog.ErrorContext(r.Context(), message, attrs...)
	// ⛔ writeError, never writeServerError — this IS writeServerError. An
	// automated rewrite of the call sites once turned this line into a call to
	// its own function, which recursed until the stack ran out.
	writeError(w, http.StatusInternalServerError, message)
}

// WriteRateLimited answers a rate-limited request in the API's own shape.
//
// Exported for internal/server's limiter, which produces the only response in
// the API that no handler writes. httprate's default is http.Error — plain text
// "Too Many Requests" — and that is the one body the SPA cannot read as
// {"error": …}, so a rate-limited dashboard surfaced a bare string with nothing
// in it to act on. Deliberately the same shape as writeLockedResponse, the
// other 429 this app can produce, so the client needs no second case.
func WriteRateLimited(w http.ResponseWriter, message string, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       message,
		"retry_after": secs,
	})
}

// pinnedProjectUUIDs resolves the request's token pin set, if any, to
// []pgtype.UUID for a query's project_ids array param. nil means unpinned
// (don't filter, passes through as SQL NULL); a non-nil (even empty) slice
// means "match exactly these ids" via ANY(), which correctly matches nothing
// when every project the token was pinned to is no longer reachable — see
// middleware.TokenProjectsFromContext.
func pinnedProjectUUIDs(ctx context.Context) ([]pgtype.UUID, error) {
	ids := middleware.TokenProjectsFromContext(ctx)
	if ids == nil {
		return nil, nil
	}
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		if err := out[i].Scan(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// notImplemented returns a 501 stub response.
func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not implemented")
}
