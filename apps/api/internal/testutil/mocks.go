package testutil

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"

	"github.com/weiliang79/belune/internal/proxy"
	"github.com/weiliang79/belune/internal/runtime"
)

// MockContainerRuntime implements runtime.ContainerRuntime for testing.
type MockContainerRuntime struct {
	mu                sync.Mutex
	StopCalls         []string
	RemoveCalls       []string
	RestartCalls      []string
	RestartErr        error
	StartCalls        []string
	StartErr          error // when set, StartContainer returns it
	StopErr           error // when set, StopContainer returns it
	CreateCalls       []runtime.ContainerConfig
	PullCalls         []string // image tags passed to PullImage
	HealthStatus_     string   // returned by ContainerHealth (empty → "healthy")
	ImageMissing_     bool     // when true, ImageExists reports false
	ContainerMissing_ bool     // when true, ContainerExists reports false
	ListContainers_   []runtime.ContainerInfo

	// Read-only admin Docker inspect fixtures (nil → empty result).
	ListAllContainers_    []runtime.ContainerInfo
	ContainerLogsTail_    string
	ListSystemContainers_ []runtime.ContainerInfo
	ListImages_           []runtime.ImageInfo
	ListVolumes_          []runtime.VolumeInfo
	ListNetworks_         []runtime.NetworkInfo
	SystemInfo_           *runtime.DockerSystemInfo
	SystemDiskUsage_      *runtime.DockerDiskUsage

	// ResolveImageDigest_ is returned by ResolveImageDigest; ResolveDigestCalls
	// records the refs it was asked to resolve.
	ResolveImageDigest_ string
	ResolveDigestCalls  []string

	// ExecFunc, when set, backs ContainerExec — lets tests simulate dump output
	// and exit codes. When nil, ContainerExec is a no-op returning exit 0.
	ExecFunc func(ctx context.Context, container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// RunHelperFunc, when set, backs RunHelper (volume tar snapshot/restore).
	// When nil, RunHelper is a no-op returning exit 0.
	RunHelperFunc func(ctx context.Context, cfg runtime.ContainerConfig, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// SpawnUpdateHelperCalls records every SpawnUpdateHelper invocation, so a
	// test can assert an update was (or was not) triggered without a real
	// container ever starting.
	SpawnUpdateHelperCalls []runtime.UpdateHelperConfig
	// PullErr, when set, is returned by PullImage after recording the call.
	PullErr error
	// PullFunc, when set, runs inside PullImage (outside the mock's lock) and
	// its error is returned — lets a test block a pull to prove it does not
	// depend on the caller's context.
	PullFunc func(ctx context.Context, image string) error
	// SpawnUpdateHelperErr, when set, is returned by SpawnUpdateHelper instead
	// of a fake container id.
	SpawnUpdateHelperErr error
}

func (m *MockContainerRuntime) CreateContainer(_ context.Context, cfg runtime.ContainerConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreateCalls = append(m.CreateCalls, cfg)
	return "mock-container-id", nil
}

func (m *MockContainerRuntime) StartContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StartCalls = append(m.StartCalls, id)
	return m.StartErr
}

func (m *MockContainerRuntime) StopContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StopCalls = append(m.StopCalls, id)
	return m.StopErr
}

func (m *MockContainerRuntime) RemoveContainer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RemoveCalls = append(m.RemoveCalls, id)
	return nil
}

func (m *MockContainerRuntime) RestartContainer(_ context.Context, id string, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RestartCalls = append(m.RestartCalls, id)
	return m.RestartErr
}

func (m *MockContainerRuntime) UpdateContainerResources(_ context.Context, _ string, _ float64, _ int64) error {
	return nil
}

func (m *MockContainerRuntime) ContainerLogs(_ context.Context, _ string, _ bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (m *MockContainerRuntime) ContainerLogsSince(_ context.Context, _ string, _ time.Time) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// ContainerLogsTail_ is the canned tail returned by ContainerLogsTail. Empty
// keeps the previous behaviour (no output); the update-status handler quotes
// this to explain WHY an update helper failed, so a test asserting that message
// needs to seed it.
func (m *MockContainerRuntime) ContainerLogsTail(_ context.Context, _ string, _ int) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return io.NopCloser(strings.NewReader(m.ContainerLogsTail_)), nil
}

func (m *MockContainerRuntime) ListContainers(_ context.Context) ([]runtime.ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListContainers_ == nil {
		return []runtime.ContainerInfo{}, nil
	}
	return m.ListContainers_, nil
}

func (m *MockContainerRuntime) ListAllContainers(_ context.Context) ([]runtime.ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListAllContainers_ == nil {
		return []runtime.ContainerInfo{}, nil
	}
	return m.ListAllContainers_, nil
}

func (m *MockContainerRuntime) ListSystemContainers(_ context.Context) ([]runtime.ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ListSystemContainers_, nil
}

func (m *MockContainerRuntime) ListImages(_ context.Context) ([]runtime.ImageInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListImages_ == nil {
		return []runtime.ImageInfo{}, nil
	}
	return m.ListImages_, nil
}

func (m *MockContainerRuntime) ListVolumes(_ context.Context) ([]runtime.VolumeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListVolumes_ == nil {
		return []runtime.VolumeInfo{}, nil
	}
	return m.ListVolumes_, nil
}

func (m *MockContainerRuntime) ListNetworks(_ context.Context) ([]runtime.NetworkInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListNetworks_ == nil {
		return []runtime.NetworkInfo{}, nil
	}
	return m.ListNetworks_, nil
}

func (m *MockContainerRuntime) SystemInfo(_ context.Context) (*runtime.DockerSystemInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SystemInfo_ == nil {
		return &runtime.DockerSystemInfo{}, nil
	}
	return m.SystemInfo_, nil
}

func (m *MockContainerRuntime) SystemDiskUsage(_ context.Context) (*runtime.DockerDiskUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SystemDiskUsage_ == nil {
		return &runtime.DockerDiskUsage{}, nil
	}
	return m.SystemDiskUsage_, nil
}

func (m *MockContainerRuntime) ResolveImageDigest(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ResolveDigestCalls = append(m.ResolveDigestCalls, ref)
	return m.ResolveImageDigest_, nil
}

func (m *MockContainerRuntime) PullImage(ctx context.Context, image string) error {
	m.mu.Lock()
	m.PullCalls = append(m.PullCalls, image)
	fn, err := m.PullFunc, m.PullErr
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, image)
	}
	return err
}
func (m *MockContainerRuntime) BuildImage(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *MockContainerRuntime) CreateNetwork(_ context.Context, _ string) error { return nil }
func (m *MockContainerRuntime) RemoveNetwork(_ context.Context, _ string) error { return nil }
func (m *MockContainerRuntime) ConnectContainerToNetwork(_ context.Context, _, _ string) error {
	return nil
}
func (m *MockContainerRuntime) CreateVolume(_ context.Context, _ string) error      { return nil }
func (m *MockContainerRuntime) CreateCacheVolume(_ context.Context, _ string) error { return nil }
func (m *MockContainerRuntime) CreateDataVolume(_ context.Context, _ string) error  { return nil }
func (m *MockContainerRuntime) VolumeSize(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (m *MockContainerRuntime) VolumeSizes(_ context.Context, _ []string) (map[string]int64, error) {
	return map[string]int64{}, nil
}
func (m *MockContainerRuntime) RemoveVolume(_ context.Context, _ string) error { return nil }
func (m *MockContainerRuntime) RemoveImage(_ context.Context, _ string) error  { return nil }
func (m *MockContainerRuntime) PruneImages(_ context.Context) error            { return nil }
func (m *MockContainerRuntime) PruneVolumes(_ context.Context) error           { return nil }
func (m *MockContainerRuntime) PruneBuildCache(_ context.Context) error        { return nil }
func (m *MockContainerRuntime) ContainerStats(_ context.Context, _ string) (*runtime.ContainerResourceStats, error) {
	return &runtime.ContainerResourceStats{}, nil
}

// ImageExists returns ImageMissing_ inverted: it reports true (present) by
// default so existing tests are unaffected; set ImageMissing_ to simulate a
// deleted image.
func (m *MockContainerRuntime) ImageExists(_ context.Context, _ string) (bool, error) {
	return !m.ImageMissing_, nil
}

// ContainerExists returns ContainerMissing_ inverted: it reports true (present)
// by default so existing tests are unaffected; set ContainerMissing_ to simulate
// a deleted container.
func (m *MockContainerRuntime) ContainerExists(_ context.Context, _ string) (bool, error) {
	return !m.ContainerMissing_, nil
}

// ContainerHealth returns HealthStatus_, defaulting to "healthy" so the deploy
// gate passes in tests that do not exercise it. Set HealthStatus_ to drive it.
func (m *MockContainerRuntime) ContainerHealth(_ context.Context, _ string) (string, error) {
	if m.HealthStatus_ == "" {
		return "healthy", nil
	}
	return m.HealthStatus_, nil
}

func (m *MockContainerRuntime) ContainerEvents(_ context.Context, _ map[string][]string) (<-chan runtime.ContainerEvent, <-chan error) {
	ch := make(chan runtime.ContainerEvent)
	errCh := make(chan error)
	close(ch)
	close(errCh)
	return ch, errCh
}

func (m *MockContainerRuntime) ContainerExecTTY(_ context.Context, _ string, _ []string) (*runtime.TerminalExecSession, error) {
	return nil, nil
}

func (m *MockContainerRuntime) HostShellSession(_ context.Context, _ string) (*runtime.TerminalExecSession, error) {
	return &runtime.TerminalExecSession{ExecID: "mock-host-exec", RWC: nopRWC{}}, nil
}

func (m *MockContainerRuntime) ContainerExecResize(_ context.Context, _ string, _, _ uint) error {
	return nil
}

// nopRWC is a no-op ReadWriteCloser for terminal-session mocks.
type nopRWC struct{}

func (nopRWC) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

func (m *MockContainerRuntime) ContainerExec(ctx context.Context, container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if m.ExecFunc != nil {
		return m.ExecFunc(ctx, container, cmd, stdin, stdout, stderr)
	}
	return 0, nil
}

func (m *MockContainerRuntime) RunHelper(ctx context.Context, cfg runtime.ContainerConfig, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if m.RunHelperFunc != nil {
		return m.RunHelperFunc(ctx, cfg, stdin, stdout, stderr)
	}
	return 0, nil
}

func (m *MockContainerRuntime) SpawnUpdateHelper(_ context.Context, cfg runtime.UpdateHelperConfig) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SpawnUpdateHelperCalls = append(m.SpawnUpdateHelperCalls, cfg)
	if m.SpawnUpdateHelperErr != nil {
		return "", m.SpawnUpdateHelperErr
	}
	return "mock-update-helper-id", nil
}

// MockProxyManager implements proxy.ProxyManager for testing.
type MockProxyManager struct {
	mu            sync.Mutex
	AddedRoutes   []proxy.RouteConfig
	RemovedRoutes []string
}

func (m *MockProxyManager) AddRoute(_ context.Context, cfg proxy.RouteConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.AddedRoutes = append(m.AddedRoutes, cfg)
	return nil
}

func (m *MockProxyManager) RemoveRoute(_ context.Context, hostname, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RemovedRoutes = append(m.RemovedRoutes, hostname)
	return nil
}

func (m *MockProxyManager) SetupTLS(_ context.Context, _, _, _, _ string) error { return nil }

func (m *MockProxyManager) SyncAutoHTTPS(_ context.Context, _, _ []string) error { return nil }

func (m *MockProxyManager) SyncCertificates(_ context.Context, _ []proxy.HostCertificate) error {
	return nil
}

func (m *MockProxyManager) SetDashboardRoute(_ context.Context, _, _ string) error { return nil }

func (m *MockProxyManager) UsesInternalIssuer(_ context.Context) (bool, error) { return false, nil }

func (m *MockProxyManager) EnsureRoute(ctx context.Context, cfg proxy.RouteConfig) (bool, error) {
	return true, m.AddRoute(ctx, cfg)
}
func (m *MockProxyManager) ListRoutes(_ context.Context) ([]proxy.RouteConfig, error) {
	return nil, nil
}

// MockReconciler implements handler.ReconcilerStatusProvider for testing.
type MockReconciler struct {
	mu                sync.Mutex
	ReconcileNowCalls int
	Status_           proxy.ReconcilerStatus
	ReconcileErr      error
}

func (m *MockReconciler) Status() proxy.ReconcilerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Status_
}

func (m *MockReconciler) ReconcileNow(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReconcileNowCalls++
	return m.ReconcileErr
}

// MockQueueInspector implements handler.QueueInspector for testing.
type MockQueueInspector struct {
	mu                  sync.Mutex
	Info                map[string]*asynq.QueueInfo // per-queue info; missing → zero-value
	ArchivedByQ         map[string]int              // counts returned by DeleteAllArchivedTasks
	RetryByQ            map[string]int              // counts returned by DeleteAllRetryTasks
	PendingByQ          map[string]int              // counts returned by DeleteAllPendingTasks
	DeleteArchivedCalls []string
	DeleteRetryCalls    []string
	DeletePendingCalls  []string
	DeleteTaskCalls     []string // "queue/id" for each DeleteTask call
	DeleteTaskErr       error    // if set, DeleteTask returns it (simulates active task)
}

func (m *MockQueueInspector) GetQueueInfo(queue string) (*asynq.QueueInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if qi, ok := m.Info[queue]; ok {
		return qi, nil
	}
	return &asynq.QueueInfo{Queue: queue}, nil
}

func (m *MockQueueInspector) DeleteAllArchivedTasks(queue string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteArchivedCalls = append(m.DeleteArchivedCalls, queue)
	return m.ArchivedByQ[queue], nil
}

func (m *MockQueueInspector) DeleteAllRetryTasks(queue string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteRetryCalls = append(m.DeleteRetryCalls, queue)
	return m.RetryByQ[queue], nil
}

func (m *MockQueueInspector) DeleteAllPendingTasks(queue string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeletePendingCalls = append(m.DeletePendingCalls, queue)
	return m.PendingByQ[queue], nil
}

func (m *MockQueueInspector) DeleteTask(queue, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteTaskCalls = append(m.DeleteTaskCalls, queue+"/"+id)
	return m.DeleteTaskErr
}

// EnqueuedTask records a task that was enqueued.
type EnqueuedTask struct {
	TypeName string
	Payload  []byte
	Queue    string
}

// MockTaskEnqueuer implements handler.TaskEnqueuer for testing.
type MockTaskEnqueuer struct {
	mu          sync.Mutex
	Tasks       []EnqueuedTask
	EnqueueErr  error // when set, Enqueue returns this error
	EnqueueOnce bool  // when true, EnqueueErr is cleared after first use
}

func (m *MockTaskEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.EnqueueErr != nil {
		err := m.EnqueueErr
		if m.EnqueueOnce {
			m.EnqueueErr = nil
		}
		return nil, err
	}

	et := EnqueuedTask{
		TypeName: task.Type(),
		Payload:  task.Payload(),
	}
	m.Tasks = append(m.Tasks, et)
	return &asynq.TaskInfo{}, nil
}
