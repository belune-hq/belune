// Package app wires all application dependencies and manages their lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/weiliang79/belune/internal/build"
	"github.com/weiliang79/belune/internal/build/buildpacks"
	"github.com/weiliang79/belune/internal/build/dockerfile"
	"github.com/weiliang79/belune/internal/build/image"
	"github.com/weiliang79/belune/internal/build/railpack"
	"github.com/weiliang79/belune/internal/config"
	"github.com/weiliang79/belune/internal/eventwatcher"
	"github.com/weiliang79/belune/internal/logcollector"
	"github.com/weiliang79/belune/internal/logtailer"
	"github.com/weiliang79/belune/internal/migrations"
	"github.com/weiliang79/belune/internal/pkg/metrics"
	"github.com/weiliang79/belune/internal/proxy"
	"github.com/weiliang79/belune/internal/proxy/caddy"
	"github.com/weiliang79/belune/internal/proxy/capability"
	"github.com/weiliang79/belune/internal/quota"
	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/runtime/docker"
	"github.com/weiliang79/belune/internal/server"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/service/backup"
	"github.com/weiliang79/belune/internal/service/email"
	"github.com/weiliang79/belune/internal/store"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/terminal"
	"github.com/weiliang79/belune/internal/tlsstatus"
	"github.com/weiliang79/belune/internal/worker"
	"github.com/weiliang79/belune/internal/ws"
)

// App holds all application dependencies and owns their lifecycle.
type App struct {
	cfg            *config.Config
	db             *pgxpool.Pool
	queries        *generated.Queries
	dockerClient   *docker.Client
	caddyClient    *caddy.Client
	asynqClient    *asynq.Client
	rdb            *redis.Client
	hub            *ws.Hub
	auditSvc       *service.AuditService
	notifySvc      *service.NotificationService
	termMgr        *terminal.Manager
	worker         *worker.Worker
	scheduler      *asynq.Scheduler
	httpServer     *http.Server
	metricsServer  *http.Server // optional separate listener (METRICS_BIND)
	asynqInspector *asynq.Inspector
	reconciler     *proxy.Reconciler
	logTailer      *logtailer.Tailer
	logCollector   *logcollector.Collector
	redisAdapter   *ws.RedisAdapter
	eventWatcher   *eventwatcher.Watcher
}

// New initialises all application dependencies in dependency order.
// Returns an error if any required resource (DB, Docker, Redis) is unavailable.
func New(cfg *config.Config) (*App, error) {
	db, err := store.Connect(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	if cfg.SkipMigrations {
		slog.Warn("BELUNE_SKIP_MIGRATIONS=true — auto-migration disabled, schema may be ahead or behind binary expectations")
	} else if err := store.RunMigrations(cfg.DatabaseURL, migrations.Files); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	queries := generated.New(db)

	dockerClient, err := docker.New()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	// Everything that acts on containers goes through the resolver rather than
	// this client, so the day a second host exists only the resolver changes.
	// Still wired to the local client directly, each for its own reason: the
	// long-running per-host components below (log collector, event watcher,
	// metrics broadcaster) are a "run one of me per server" supervisor problem
	// rather than a lookup, and the build chain and Caddy network attacher are
	// genuinely single-host today — offloading builds and per-server proxying
	// are separate pieces of work.
	runtimes := runtime.NewLocalRuntimes(dockerClient)

	resolveCaddyContainer(cfg, dockerClient)

	caddyClient := caddy.New(cfg.CaddyAdminURL)

	// Route go-redis's own logs through slog before any client is created (the
	// logger is process-global and covers asynq's internal go-redis too). By
	// default go-redis writes pool/pubsub teardown noise straight to stderr with
	// its own timestamp and no level; on a redis restart that surfaces as
	// unfiltered "discarding bad PubSub connection: EOF" lines. redisSlogLogger
	// drops that transient churn to Debug and routes anything else to Warn.
	redis.SetLogger(redisSlogLogger{})

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		dockerClient.Close()
		db.Close()
		return nil, fmt.Errorf("parse redis URL for asynq: %w", err)
	}
	asynqClient := asynq.NewClient(redisOpt)
	asynqInspector := asynq.NewInspector(redisOpt)

	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		asynqInspector.Close()
		asynqClient.Close()
		dockerClient.Close()
		db.Close()
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}
	rdb := redis.NewClient(redisOptions)

	buildChain := build.NewChain(
		dockerfile.New(dockerClient),
		buildpacks.New(dockerClient),
		railpack.New(),
		image.New(dockerClient),
	)

	metricsSvc := service.NewMetricsService(queries, rdb)
	auditSvc := service.NewAuditService(queries)
	notifySvc := service.NewNotificationService(queries, rdb)
	termMgr := terminal.NewManager(cfg.MaxTerminalSessionsPerUser)
	hub := ws.NewHub(cfg.MaxWebSocketConnsPerUser)

	// backupDestSvc first: appSvc needs it to erase volume-backup objects when an
	// application is deleted, rather than leaving them billed with no row
	// recording their keys.
	backupDestSvc := service.NewBackupDestinationService(queries, cfg.Keyring)
	appSvc := service.NewApplicationService(db, queries, runtimes, cfg.Keyring, cfg.FileMountsDir, backupDestSvc)
	gitProviderSvc := service.NewGitProviderConfigService(queries, cfg.Keyring)
	gitIntegrationSvc := service.NewGitIntegrationService(queries, cfg.Keyring, gitProviderSvc)
	quotaSvc := quota.NewService(queries)
	emailSvc, err := email.New(cfg)
	if err != nil {
		rdb.Close()
		asynqInspector.Close()
		asynqClient.Close()
		dockerClient.Close()
		db.Close()
		return nil, fmt.Errorf("load email templates: %w", err)
	}
	// DB-backed SMTP settings override the env defaults and take effect without a
	// restart: the resolver is read per-send by the shared email service.
	emailSvc.SetResolver(service.NewSMTPSettingsService(queries, cfg.Keyring, cfg))

	backupSvc := backup.New(cfg)
	notifyRegistry := service.NewNotifyRegistry(emailSvc)
	notifyChannelSvc := service.NewNotificationChannelService(queries, cfg.Keyring, notifyRegistry, cfg.PublicBaseURL)
	taskHandler := &worker.TaskHandler{
		Runtimes:              runtimes,
		Proxy:                 caddyClient,
		DB:                    db,
		Queries:               queries,
		Chain:                 buildChain,
		Keyring:               cfg.Keyring,
		RedisClient:           rdb,
		Config:                cfg,
		MetricsService:        metricsSvc,
		AppService:            appSvc,
		GitIntegrationService: gitIntegrationSvc,
		QuotaService:          quotaSvc,
		EmailService:          emailSvc,
		BackupService:         backupSvc,
		BackupDestinations:    backupDestSvc,
		NotifyChannels:        notifyChannelSvc,
		AuditLog:              auditSvc,
		Notifier:              notifySvc,
		Enqueuer:              asynqClient,
	}

	w := worker.New(redisOpt, taskHandler)

	scheduler, err := w.StartScheduler()
	if err != nil {
		slog.Warn("failed to start cleanup scheduler", "error", err)
	}

	caddyClient.SetDashboardUpstream(cfg.DashboardUpstream)
	caddyClient.InitCatchAll(context.Background())
	// The reconciler also keeps Caddy joined to each project's network. Without
	// it, a recreated Caddy container (any compose up, any upgrade) comes back
	// having lost every network the deploy worker put it on, and every app domain
	// answers 502 until the app is redeployed.
	reconciler := proxy.NewReconciler(queries, caddyClient, cfg.Keyring, 30*time.Second).
		WithNetworkAttacher(dockerClient, cfg.CaddyContainerName).
		// After resolveCaddyContainer, so the probe execs into the real name.
		WithCapabilities(capability.NewProber(dockerClient, cfg.CaddyContainerName))

	// One-time move of plaintext webhook secrets into the keyring-encrypted
	// column (migration 000051). Safe to run on every boot: it only touches
	// rows that still hold plaintext.
	service.BackfillWebhookSecrets(context.Background(), queries, cfg.Keyring)

	if err := caddyClient.ConfigureAccessLogs(context.Background()); err != nil {
		slog.Warn("failed to configure Caddy access logs", "error", err)
	}

	broadcaster := ws.NewContainerStatusBroadcaster(hub)
	httpSrv := server.New(cfg, db, queries, asynqClient, asynqInspector, runtimes, caddyClient, reconciler, rdb, hub, auditSvc, notifySvc, termMgr, emailSvc)

	// Optional Prometheus-friendly bind. Serves /metrics without auth on the
	// configured address (typically loopback). Keeps the main /metrics route
	// gated behind admin auth for UI browsers.
	var metricsServer *http.Server
	if cfg.MetricsBind != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		metricsServer = &http.Server{
			Addr:              cfg.MetricsBind,
			Handler:           mux,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      10 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	app := &App{
		cfg:          cfg,
		db:           db,
		queries:      queries,
		dockerClient: dockerClient,
		caddyClient:  caddyClient,
		asynqClient:  asynqClient,
		rdb:          rdb,
		hub:          hub,
		auditSvc:     auditSvc,
		notifySvc:    notifySvc,
		termMgr:      termMgr,
		worker:       w,
		scheduler:    scheduler,
		httpServer: &http.Server{
			Addr:         fmt.Sprintf(":%d", cfg.Port),
			Handler:      httpSrv.Router(),
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 0, // Disabled to support SSE streaming endpoints
			IdleTimeout:  60 * time.Second,
		},
		metricsServer:  metricsServer,
		asynqInspector: asynqInspector,
		reconciler:     reconciler,
		logTailer:      logtailer.New(cfg.AccessLogPath, queries, rdb),
		logCollector:   logcollector.New(dockerClient, queries, rdb),
		redisAdapter:   ws.NewRedisAdapter(rdb, hub),
		eventWatcher:   eventwatcher.New(dockerClient, queries, broadcaster),
	}

	// Caddy's own logs carry the reason a certificate failed to issue. Hook the
	// collector so those reasons land on the domain they belong to, instead of
	// leaving the user with a "pending" badge and no explanation.
	tlsRecorder := tlsstatus.NewRecorder(queries)
	app.logCollector.SetLineHook(func(hookCtx context.Context, srcType, name, message string) {
		if name == logcollector.SystemCaddy {
			tlsRecorder.HandleCaddyLine(hookCtx, message)
		}
	})
	// A failed SetupTLS used to be a log line and nothing more. Give it the same
	// destination as an ACME error, so a broken custom certificate is visible.
	caddyClient.SetTLSErrorSink(tlsRecorder.Record)

	return app, nil
}

// Run starts all background goroutines and the HTTP server. It blocks until ctx
// is cancelled or a goroutine fails fatally, then coordinates an ordered shutdown
// before returning. The caller should invoke Shutdown to release remaining resources.
func (a *App) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	// HTTP server
	g.Go(func() error {
		slog.Info("server starting", "port", a.cfg.Port)
		if err := a.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	// Worker — blocks until stopped
	g.Go(func() error {
		if err := a.worker.Start(); err != nil {
			return fmt.Errorf("worker: %w", err)
		}
		return nil
	})

	// Context-driven goroutines — all return when gctx is cancelled
	g.Go(func() error { a.worker.StartMetricsTicker(gctx); return nil })
	g.Go(func() error { a.reconciler.Run(gctx); return nil })
	g.Go(func() error { a.logTailer.Run(gctx); return nil })
	g.Go(func() error { a.logCollector.Run(gctx); return nil })
	g.Go(func() error { a.hub.Run(gctx); return nil })
	g.Go(func() error { a.redisAdapter.RunBuildLogAdapter(gctx); return nil })
	g.Go(func() error { a.redisAdapter.RunHostMetricsAdapter(gctx); return nil })
	g.Go(func() error { a.redisAdapter.RunRequestLogAdapter(gctx); return nil })
	g.Go(func() error { a.redisAdapter.RunContainerLogAdapter(gctx); return nil })
	g.Go(func() error { ws.RunAppMetricsBroadcaster(gctx, a.hub, a.dockerClient, a.queries); return nil })
	g.Go(func() error { a.eventWatcher.Run(gctx); return nil })
	g.Go(func() error { a.auditSvc.Run(gctx); return nil })
	g.Go(func() error { a.notifySvc.Run(gctx); return nil })
	g.Go(func() error { metrics.AsynqQueuePoller(gctx, a.asynqInspector, 15*time.Second); return nil })

	if a.metricsServer != nil {
		g.Go(func() error {
			slog.Info("metrics server starting", "addr", a.metricsServer.Addr)
			if err := a.metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("metrics server: %w", err)
			}
			return nil
		})
	}

	// Shutdown coordinator — triggered when gctx is cancelled (signal or fatal error)
	g.Go(func() error {
		<-gctx.Done()
		slog.Info("shutting down server...")

		// Stop HTTP server first (drain in-flight requests)
		httpCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.httpServer.Shutdown(httpCtx); err != nil {
			slog.Error("http server forced to shutdown", "error", err)
		}
		if a.metricsServer != nil {
			if err := a.metricsServer.Shutdown(httpCtx); err != nil {
				slog.Error("metrics server forced to shutdown", "error", err)
			}
		}

		// Stop the worker (no new tasks), then drain in-flight tasks.
		// Note: the asynq Scheduler is shut down in Shutdown() after g.Wait()
		// returns. A scheduler tick that fires in this window may enqueue a
		// task that the stopped processor won't pick up until next start — this
		// is acceptable (tasks are idempotent and will run on next restart).
		a.worker.Stop()
		a.worker.Shutdown()

		return nil
	})

	return g.Wait()
}

// resolveCaddyContainer fills in the Caddy container's real name by looking for
// the belune-system=caddy label, unless the operator named it explicitly.
//
// Compose derives container names from the project, and the project defaults to
// the directory holding the compose file: infra/ in this repo, but the install
// directory (belune-caddy-1 for /opt/belune) on a real install. The old baked-in
// default was the repo's name, so every real install missed — and the miss is
// silent, because attaching Caddy to a project network only warns. The visible
// symptom is every app domain answering 502 with a healthy-looking stack.
func resolveCaddyContainer(cfg *config.Config, dc *docker.Client) {
	if os.Getenv("CADDY_CONTAINER_NAME") != "" {
		return
	}

	// Retried briefly: on a cold `compose up` the proxy is created alongside this
	// container rather than before it, so the first look can legitimately come up
	// empty. Bounded, because a stack that runs Caddy elsewhere must still boot.
	for attempt := range 5 {
		if attempt > 0 {
			time.Sleep(time.Second)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		containers, err := dc.ListSystemContainers(ctx)
		cancel()
		if err != nil {
			slog.Warn("could not list system containers to find Caddy", "error", err, "assuming", cfg.CaddyContainerName)
			return
		}

		for _, c := range containers {
			if c.Labels["belune-system"] == logcollector.SystemCaddy && c.Name != "" {
				slog.Info("discovered Caddy container", "name", c.Name)
				cfg.CaddyContainerName = c.Name
				return
			}
		}
	}
	slog.Warn("no container carries belune-system=caddy — app domains will not be routable until Caddy joins their networks",
		"assuming", cfg.CaddyContainerName)
}

// Shutdown releases remaining resources after Run has returned. It should be
// called with a timeout context.
func (a *App) Shutdown(ctx context.Context) error {
	a.termMgr.CloseAll()

	if a.scheduler != nil {
		a.scheduler.Shutdown()
	}

	a.asynqClient.Close()
	if a.asynqInspector != nil {
		a.asynqInspector.Close()
	}
	a.rdb.Close()
	a.dockerClient.Close()
	a.db.Close()

	slog.Info("server stopped")
	return nil
}
