package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/eventwatcher"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

var (
	testPool    *pgxpool.Pool
	testQueries *generated.Queries
)

func TestMain(m *testing.M) {
	pool, queries, teardown := testutil.SetupTestDB()
	testPool = pool
	testQueries = queries
	code := m.Run()
	teardown()
	os.Exit(code)
}

// failingProxy is a ProxyManager whose AddRoute always fails. Used to drive
// the wire_proxy stage into its compensator path so we can assert that the
// container created in stage 5 gets stopped + removed.
type failingProxy struct {
	mu          sync.Mutex
	addAttempts []string
}

func (p *failingProxy) AddRoute(_ context.Context, cfg proxy.RouteConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addAttempts = append(p.addAttempts, cfg.Hostname)
	return errors.New("proxy unavailable")
}

func (p *failingProxy) RemoveRoute(_ context.Context, hostname, _ string) error { return nil }
func (p *failingProxy) SetupTLS(_ context.Context, _, _, _, _ string) error     { return nil }
func (p *failingProxy) SyncAutoHTTPS(_ context.Context, _, _ []string) error    { return nil }
func (p *failingProxy) SyncCertificates(_ context.Context, _ []proxy.HostCertificate) error {
	return nil
}
func (p *failingProxy) SetDashboardRoute(_ context.Context, _, _ string) error { return nil }

func (p *failingProxy) EnsureRoute(ctx context.Context, cfg proxy.RouteConfig) (bool, error) {
	return true, p.AddRoute(ctx, cfg)
}
func (p *failingProxy) UsesInternalIssuer(_ context.Context) (bool, error)        { return false, nil }
func (p *failingProxy) ListRoutes(_ context.Context) ([]proxy.RouteConfig, error) { return nil, nil }

// createFailRuntime wraps MockContainerRuntime so CreateContainer always fails.
// Used to drive stage 5 into its failure path without modifying the shared mock.
type createFailRuntime struct {
	*testutil.MockContainerRuntime
	createErr error
}

func (r *createFailRuntime) CreateContainer(_ context.Context, _ runtime.ContainerConfig) (string, error) {
	return "", r.createErr
}

// seedApp inserts a user, project, application (image type, no health check),
// and deployment (status=pending). Optional opts override fields before insert.
func seedApp(t *testing.T, opts ...func(app *generated.CreateApplicationParams, dep *generated.CreateDeploymentParams)) (generated.Application, generated.Deployment) {
	t.Helper()
	ctx := context.Background()

	suffix := randomSuffix(t)
	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{
		Email:        "deploy-" + suffix + "@test.com",
		PasswordHash: "x",
		Role:         "admin",
	})
	require.NoError(t, err)

	project, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name:     "Test Project",
		Slug:     "proj-" + suffix,
		UserID:   user.ID,
		ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)

	appParams := generated.CreateApplicationParams{
		ProjectID:              project.ID,
		Name:                   "Test App",
		Slug:                   project.Slug + "-app",
		Type:                   "image",
		SourceImage:            pgtype.Text{String: "nginx:alpine", Valid: true},
		BuildType:              "image",
		WebhookSecretEncrypted: []byte("ciphertext-" + suffix),
	}
	depParams := generated.CreateDeploymentParams{
		Status:      status.DeploymentPending,
		TriggeredBy: "manual",
	}
	for _, fn := range opts {
		fn(&appParams, &depParams)
	}

	app, err := testQueries.CreateApplication(ctx, appParams)
	require.NoError(t, err)

	depParams.ApplicationID = app.ID
	dep, err := testQueries.CreateDeployment(ctx, depParams)
	require.NoError(t, err)

	return app, dep
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	tok, err := crypto.GenerateWebhookSecret()
	require.NoError(t, err)
	return tok[:8]
}

func newTestHandler(rt runtime.ContainerRuntime, pm proxy.ProxyManager) *worker.TaskHandler {
	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	if err != nil {
		panic("build keyring: " + err.Error())
	}
	return &worker.TaskHandler{
		// One mock runtime behind the resolver: every server resolves to it,
		// which is exactly what the single-server resolver does in production.
		Runtimes: runtime.NewLocalRuntimes(rt),
		Proxy:    pm,
		DB:       testPool,
		Queries:  testQueries,
		Keyring:  keyring,
		Config: &config.Config{
			ImagePullTimeoutMinutes: 5,
			BuildTimeoutMinutes:     10,
		},
		// QuotaService nil → checkDeployQuota becomes a no-op (intentional).
	}
}

func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	idx := 0
	for i, by := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[idx] = '-'
			idx++
		}
		out[idx] = hex[by>>4]
		out[idx+1] = hex[by&0x0f]
		idx += 2
	}
	return string(out)
}

func mustMarshalPayload(t *testing.T, appID, depID pgtype.UUID) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"application_id": uuidString(appID),
		"deployment_id":  uuidString(depID),
	})
	require.NoError(t, err)
	return b
}

func TestHandleDeployTask_SkipsAlreadyTerminal(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app, dep := seedApp(t, func(_ *generated.CreateApplicationParams, dp *generated.CreateDeploymentParams) {
		dp.Status = status.DeploymentSuccess
	})

	rt := &testutil.MockContainerRuntime{}
	pm := &testutil.MockProxyManager{}
	h := newTestHandler(rt, pm)

	task := asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))
	err := h.HandleDeployTask(context.Background(), task)

	assert.NoError(t, err, "terminal deployment should be skipped silently")
	assert.Empty(t, rt.CreateCalls, "no container should be created when skipping")
	assert.Empty(t, rt.StartCalls, "no container should be started when skipping")
	assert.Empty(t, pm.AddedRoutes, "no proxy route should be added when skipping")
}

func TestHandleDeployTask_CompensatesOnProxyFailure(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app, dep := seedApp(t)

	// Wire a domain so wireProxy enters its loop and our failing proxy fires.
	_, err := testQueries.CreateDomain(context.Background(), generated.CreateDomainParams{
		Path:          "/",
		ApplicationID: app.ID,
		Hostname:      "broken.example.com",
		SslEnabled:    false,
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
		ForceHttps:    false,
		SslMode:       "off",
	})
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{}
	pm := &failingProxy{}
	h := newTestHandler(rt, pm)

	task := asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))
	err = h.HandleDeployTask(context.Background(), task)

	// HandleDeployTask returns the proxy error so asynq retries.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wire proxy")

	// Container was created and started in stage 5 …
	assert.Len(t, rt.CreateCalls, 1, "container should be created before proxy stage")
	assert.Len(t, rt.StartCalls, 1, "container should be started before proxy stage")

	// … and the compensator must have stopped + removed it after the proxy
	// failure, otherwise we'd leak a half-broken container behind a missing
	// route.
	assert.Contains(t, rt.StopCalls, "mock-container-id",
		"compensator should stop the container on wire_proxy failure")
	assert.Contains(t, rt.RemoveCalls, "mock-container-id",
		"compensator should remove the container on wire_proxy failure")

	// Deployment row should be marked failed (retried=0, maxRetry=0 →
	// failDeployment writes the failure status).
	final, err := testQueries.GetDeployment(context.Background(), dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentFailed, final.Status)

	// The application must go with it. cleanupExisting removes the previous
	// container before the build, so a failure here leaves nothing serving —
	// the app used to keep its old status and read as healthy.
	failedApp, err := testQueries.GetApplication(context.Background(), app.ID)
	require.NoError(t, err)
	assert.Equal(t, status.ApplicationError, failedApp.Status)
}

// A crashed container (non-zero exit) must be distinguishable from one the user
// stopped, matching how database containers are already handled.
func TestApplicationStatusForEvent(t *testing.T) {
	cases := []struct {
		name   string
		event  runtime.ContainerEvent
		want   string
		wantOK bool
	}{
		{"start", runtime.ContainerEvent{Status: "start"}, status.ApplicationRunning, true},
		{"restart", runtime.ContainerEvent{Status: "restart"}, status.ApplicationRunning, true},
		{"explicit stop", runtime.ContainerEvent{Status: "stop"}, status.ApplicationStopped, true},
		{
			"clean exit",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "0"}},
			status.ApplicationStopped, true,
		},
		{
			"crash",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "1"}},
			status.ApplicationError, true,
		},
		// The regression: `docker stop` sends SIGTERM, and an image that does
		// not trap it exits 128+15. Reading that as a crash marked every
		// deliberate stop as an error.
		{
			"terminated by SIGTERM on a deliberate stop",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "143"}},
			status.ApplicationStopped, true,
		},
		{
			"killed by SIGKILL after the stop grace period",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "137"}},
			status.ApplicationStopped, true,
		},
		// Codes the application chose itself still mean a crash.
		{
			"fatal config error",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "2"}},
			status.ApplicationError, true,
		},
		{
			"command not found",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "127"}},
			status.ApplicationError, true,
		},
		// Docker always sets exitCode on die, so these are defensive only —
		// and when we cannot tell, the non-alarming reading wins.
		{
			"missing exit code",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{}},
			status.ApplicationStopped, true,
		},
		{
			"unparseable exit code",
			runtime.ContainerEvent{Status: "die", Labels: map[string]string{"exitCode": "nope"}},
			status.ApplicationStopped, true,
		},
		// An OOM kill also exits 137, but Docker emits `oom` first and the
		// watcher refuses to downgrade an errored app back to stopped.
		{"oom", runtime.ContainerEvent{Status: "oom"}, status.ApplicationError, true},
		{"unrelated", runtime.ContainerEvent{Status: "pause"}, "", false},
	}
	for _, tc := range cases {
		got, ok := eventwatcher.ApplicationStatusForEvent(tc.event)
		assert.Equal(t, tc.wantOK, ok, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

func TestHandleDeployTask_HappyPathSkipsHealthCheck(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app, dep := seedApp(t) // image type, no domains, no health_check_path

	rt := &testutil.MockContainerRuntime{}
	pm := &testutil.MockProxyManager{}
	h := newTestHandler(rt, pm)

	task := asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))
	err := h.HandleDeployTask(context.Background(), task)
	require.NoError(t, err)

	// Container created + started, no proxy routes (no domains).
	assert.Len(t, rt.CreateCalls, 1)
	assert.Len(t, rt.StartCalls, 1)
	assert.Empty(t, pm.AddedRoutes)

	// Deployment finalized → success; health probe recorded as skipped.
	final, err := testQueries.GetDeployment(context.Background(), dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentSuccess, final.Status)
	assert.True(t, final.HealthStatus.Valid)
	assert.Equal(t, "skipped", final.HealthStatus.String,
		"apps without health_check_path should record skipped, not passing")

	// App row flipped to running.
	appRow, err := testQueries.GetApplication(context.Background(), app.ID)
	require.NoError(t, err)
	assert.Equal(t, status.ApplicationRunning, appRow.Status)
}

func TestHandleDeployTask_RollbackUsesProvidedImageTag(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app, dep := seedApp(t)

	rt := &testutil.MockContainerRuntime{}
	pm := &testutil.MockProxyManager{}
	h := newTestHandler(rt, pm)

	// Build a payload with a rollback image tag so prepareImage takes the
	// rollback path and skips the PullImage call entirely.
	const rollbackTag = "nginx:1.20"
	payload, err := json.Marshal(map[string]string{
		"application_id":     uuidString(app.ID),
		"deployment_id":      uuidString(dep.ID),
		"rollback_image_tag": rollbackTag,
	})
	require.NoError(t, err)

	task := asynq.NewTask("deploy", payload)
	require.NoError(t, h.HandleDeployTask(context.Background(), task))

	// Container must be created with the rollback image tag — not the app's
	// configured source image and not a build tag.
	require.Len(t, rt.CreateCalls, 1, "expected exactly one container creation")
	assert.Equal(t, rollbackTag, rt.CreateCalls[0].Image,
		"rollback deploy should use the provided image tag, not the app source image")
	// PullImage must not be called on the rollback path — the image was already
	// on the host when it was originally deployed.
	assert.Empty(t, rt.PullCalls, "rollback should not pull an image from the registry")

	final, err := testQueries.GetDeployment(context.Background(), dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentSuccess, final.Status)
}

func TestHandleDeployTask_CreateContainerFailureMarksDeployFailed(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })

	app, dep := seedApp(t)

	rt := &createFailRuntime{
		MockContainerRuntime: &testutil.MockContainerRuntime{},
		createErr:            errors.New("disk full"),
	}
	pm := &testutil.MockProxyManager{}
	h := newTestHandler(rt, pm)

	task := asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))
	err := h.HandleDeployTask(context.Background(), task)

	// Handler must propagate the error so asynq can retry or dead-letter.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create container")

	// No start or proxy calls — the pipeline halted at stage 5.
	assert.Empty(t, rt.StartCalls, "container should not be started when creation fails")
	assert.Empty(t, pm.AddedRoutes, "no proxy route should be wired when creation fails")

	// Deployment row must be marked failed (retried=0, maxRetry=0 → failDeployment
	// writes the failure row immediately).
	final, err := testQueries.GetDeployment(context.Background(), dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentFailed, final.Status)

	// The application must go with it. cleanupExisting removes the previous
	// container before the build, so a failure here leaves nothing serving —
	// the app used to keep its old status and read as healthy.
	failedApp, err := testQueries.GetApplication(context.Background(), app.ID)
	require.NoError(t, err)
	assert.Equal(t, status.ApplicationError, failedApp.Status)
}

// Regression: loadApplication builds dc.app by copying a subset of columns by
// hand, and the command health-check fields were left out of that copy — so the
// HEALTHCHECK never reached the container even though the row had it. This
// deploys a command-health app and asserts the config actually lands on
// CreateContainer, which only passes if loadApplication propagates the fields.
func TestHandleDeployTask_AppliesCommandHealthCheck(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()

	app, dep := seedApp(t)
	_, err := testQueries.SetApplicationHealthCheck(ctx, generated.SetApplicationHealthCheckParams{
		ID:                         app.ID,
		HealthCheckType:            "command",
		HealthCheckCommand:         pgtype.Text{String: "curl -f localhost/health", Valid: true},
		HealthCheckIntervalSeconds: pgtype.Int4{Int32: 15, Valid: true},
	})
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{} // ContainerHealth defaults to "healthy"
	pm := &testutil.MockProxyManager{}
	h := newTestHandler(rt, pm)

	// Use a rollback payload — this is the reload path, exactly where the bug
	// showed up live, and it also skips the image-pull stage (nil Redis in
	// tests). createAndStart still runs, which is what reads the health fields.
	payload, err := json.Marshal(map[string]string{
		"application_id":     uuidString(app.ID),
		"deployment_id":      uuidString(dep.ID),
		"rollback_image_tag": "nginx:alpine",
	})
	require.NoError(t, err)
	require.NoError(t, h.HandleDeployTask(ctx, asynq.NewTask("deploy", payload)))

	require.Len(t, rt.CreateCalls, 1)
	assert.Equal(t, "curl -f localhost/health", rt.CreateCalls[0].HealthCheckCommand,
		"the command health check must reach the container")
	assert.Equal(t, 15*time.Second, rt.CreateCalls[0].HealthCheckInterval)

	// The command gate passed (mock reports healthy), so the deploy succeeds
	// and the probe is recorded as passing, not skipped.
	final, err := testQueries.GetDeployment(ctx, dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentSuccess, final.Status)
	assert.Equal(t, "passing", final.HealthStatus.String)
}
