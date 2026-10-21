package testutil

import (
	"net/http/httptest"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/terminal"
)

// TestEncryptionKey is a valid 32-byte hex-encoded key for AES-256 in tests.
const TestEncryptionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestJWTSecret is the JWT secret used in tests.
const TestJWTSecret = "test-jwt-secret-for-integration-tests"

// TestEnv holds all test dependencies.
type TestEnv struct {
	Server     *httptest.Server
	Pool       *pgxpool.Pool
	Queries    *generated.Queries
	Runtime    *MockContainerRuntime
	Proxy      *MockProxyManager
	Asynq      *MockTaskEnqueuer
	Inspector  *MockQueueInspector
	Reconciler *MockReconciler
	// Capabilities is what the Caddy probe reports; the zero value is "unknown".
	Capabilities *MockCapabilities
	AccessLog    *MockAccessLog
	Config       *config.Config
	Redis        *redis.Client
	RedisSrv     *miniredis.Miniredis
}

// SetupTestServer creates a full HTTP test server with real DB and mock external deps.
// Called from TestMain so does not take *testing.T.
func SetupTestServer(pool *pgxpool.Pool, queries *generated.Queries) *TestEnv {
	keyring, err := crypto.ParseKeyringEnv("", TestEncryptionKey, "")
	if err != nil {
		panic("testutil: build test keyring: " + err.Error())
	}
	cfg := &config.Config{
		Port:                8080,
		JWTSecret:           TestJWTSecret,
		Keyring:             keyring,
		CaddyAdminURL:       "http://localhost:2019",
		DisableRateLimiting: true,
	}

	mockRuntime := &MockContainerRuntime{}
	mockProxy := &MockProxyManager{}
	mockAsynq := &MockTaskEnqueuer{}
	mockInspector := &MockQueueInspector{}
	mockReconciler := &MockReconciler{}
	mockCaps := &MockCapabilities{}
	mockAccessLog := &MockAccessLog{}

	// In-memory Redis so the auth service's revocation paths (per-JTI
	// blacklist + per-user invalidated-after flag) actually exercise their
	// Redis branches under test.
	mr, err := miniredis.Run()
	if err != nil {
		panic("testutil: start miniredis: " + err.Error())
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	// A real terminal manager (over the mock runtime) so the host-shell route
	// reaches its auth gates instead of short-circuiting on a nil manager.
	termMgr := terminal.NewManager(2)

	srv := server.New(cfg, pool, queries, mockAsynq, mockInspector, runtime.NewLocalRuntimes(mockRuntime), mockProxy, mockReconciler, rdb, nil, nil, nil, termMgr, nil)
	srv.SetCapabilities(mockCaps)
	srv.SetAccessLog(mockAccessLog)
	ts := httptest.NewServer(srv.Router())

	return &TestEnv{
		Server:       ts,
		Pool:         pool,
		Queries:      queries,
		Runtime:      mockRuntime,
		Proxy:        mockProxy,
		Asynq:        mockAsynq,
		Inspector:    mockInspector,
		Reconciler:   mockReconciler,
		Capabilities: mockCaps,
		AccessLog:    mockAccessLog,
		Config:       cfg,
		Redis:        rdb,
		RedisSrv:     mr,
	}
}
