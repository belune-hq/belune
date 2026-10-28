package runtime

import (
	"context"
	"io"
	"time"
)

// TerminalExecSession represents an active exec session with TTY in a container.
type TerminalExecSession struct {
	ExecID string
	RWC    io.ReadWriteCloser // combined stdin+stdout stream (TTY mode — no mux header)
}

// BindMount is a single host-path → container-path mount. Used for read-only
// file/config mounts, where Source is a managed host file the deploy worker
// wrote (never a user-chosen host path).
type BindMount struct {
	Source string // host path
	Target string // container path
}

type ContainerConfig struct {
	Name            string
	Image           string
	Env             map[string]string
	Ports           map[string]string // host:container
	HostBindIP      string            // host IP for port bindings (empty = 0.0.0.0; "127.0.0.1" = loopback only)
	Volumes         map[string]string // host:container
	ReadOnlyBinds   []BindMount       // host-file → container-path, mounted read-only (file/config mounts)
	Network         string
	Cmd             []string
	Labels          map[string]string
	CPULimit        float64 // CPU cores (0 = unlimited, e.g. 0.5 = half a core)
	MemoryLimit     int64   // bytes (0 = unlimited, e.g. 536870912 = 512 MB)
	HealthCheckPath string  // HTTP path for control-plane polling after deploy (e.g. /healthz); empty = skip

	// Command health check — a native Docker HEALTHCHECK run inside the
	// container. Empty HealthCheckCommand means no HEALTHCHECK is set, leaving
	// whatever the image declares (usually none). Zero durations/retries are
	// resolved to platform defaults by the caller before this is populated.
	HealthCheckCommand     string        // shell command; run as CMD-SHELL
	HealthCheckInterval    time.Duration // between checks
	HealthCheckTimeout     time.Duration // per check
	HealthCheckRetries     int           // consecutive failures before unhealthy
	HealthCheckStartPeriod time.Duration // grace window where failures do not count

	// Security hardening — applied as-is to the Docker HostConfig. Defaults
	// (zero-value) preserve legacy permissive behaviour so this struct can be
	// embedded by managed-database provisioning that has different needs.
	//
	// For user app containers, deploy_task.go fills these with:
	//   CapDrop=["ALL"], SecurityOpt=["no-new-privileges"], ReadonlyRootfs=true,
	//   Tmpfs={"/tmp":"", "/run":""}
	CapDrop        []string          // capabilities to drop (e.g. "ALL")
	CapAdd         []string          // capabilities to add back (e.g. "NET_BIND_SERVICE")
	SecurityOpt    []string          // security options (e.g. "no-new-privileges")
	ReadonlyRootfs bool              // when true, container rootfs is read-only
	Tmpfs          map[string]string // path → mount options (empty options OK)
}

type ContainerInfo struct {
	ID        string
	Name      string
	Image     string
	Status    string
	Ports     map[string]string
	Labels    map[string]string
	CreatedAt time.Time
	// Addrs are the container's IP addresses, one per network it is attached to.
	// Empty for a container that is not running.
	Addrs []string
}

// LabelHelper marks the throwaway containers RunHelper starts to do work inside
// a volume (backup, restore, snapshot). They carry the managed-by label like
// everything else, so they appear in ListContainers — but they are unnamed, so
// no database row can ever vouch for one and the orphan sweep would treat a
// healthy in-flight restore as garbage. It lives here rather than in the Docker
// package because the writer and the reader are on opposite sides of this
// interface, and a constant either of them can rename privately is a constant
// that will drift.
const LabelHelper = "belune-helper"

// LabelUpdateHelper additionally marks the ONE helper kind that must never run
// twice at once: the self-updater. LabelHelper alone cannot answer "is an
// update already running" — backup, restore and snapshot helpers all carry it,
// and a running volume restore must not block an update (or vice versa). It
// lives beside LabelHelper for the same reason: SpawnUpdateHelper writes it and
// the handler's conflict check reads it, on opposite sides of this interface.
const LabelUpdateHelper = "belune-update"

// LabelApplicationID and LabelDatabaseID tie a container back to the row that
// owns it. They live here for the same reason as LabelHelper: the deploy and
// provision workers write them, the event watcher and the orphan sweep read
// them, and a constant either side could rename privately is one that will
// drift. The orphan sweep deletes what it cannot match, so drift there is not
// a cosmetic bug.
const (
	LabelApplicationID = "application-id"
	LabelDatabaseID    = "database-id"
)

// ContainerResourceStats holds resource usage stats for a single container.
type ContainerResourceStats struct {
	CPUPercent     float64
	MemoryUsage    int64
	MemoryLimit    int64
	NetworkRxBytes int64
	NetworkTxBytes int64
}

// ContainerRuntime abstracts container operations.
type ContainerRuntime interface {
	CreateContainer(ctx context.Context, cfg ContainerConfig) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string) error
	RemoveContainer(ctx context.Context, id string) error
	// ContainerExists reports whether a container with the given name (or ID) is
	// present on the host, running or stopped. A missing container is (false,
	// nil); other failures are surfaced as errors. Lets callers tell a genuinely
	// removed container apart from a merely-stopped one (e.g. to offer Reload).
	ContainerExists(ctx context.Context, name string) (bool, error)
	// RestartContainer restarts a container in place, giving it `timeout` seconds
	// to stop gracefully before it is killed.
	RestartContainer(ctx context.Context, id string, timeout int) error
	// UpdateContainerResources applies CPU (cores) / memory (bytes) limits to a
	// running container without recreating it. Zero means unlimited.
	UpdateContainerResources(ctx context.Context, id string, cpuCores float64, memoryBytes int64) error
	ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error)
	// ContainerLogsSince streams logs from a container starting at the given time.
	// Pass time.Now() to receive only new log lines (no backlog).
	ContainerLogsSince(ctx context.Context, id string, since time.Time) (io.ReadCloser, error)
	// ContainerLogsTail returns at most the last `tail` lines of a container's
	// logs and does not follow. Bounded on purpose: the platform log viewer must
	// not read the whole (up to hundreds of MB) json-file history to show recent
	// lines. The stream is stdcopy-multiplexed for non-TTY containers.
	ContainerLogsTail(ctx context.Context, id string, tail int) (io.ReadCloser, error)
	ListContainers(ctx context.Context) ([]ContainerInfo, error)
	// ListAllContainers lists every container on the host (running and stopped),
	// including ones not managed by the platform. Read-only; powers the admin
	// Docker inspect page. Unlike ListContainers it does not filter by the
	// managed-by label.
	ListAllContainers(ctx context.Context) ([]ContainerInfo, error)
	// ListSystemContainers lists Belune's own infrastructure containers — the ones
	// carrying the belune-system label, currently just the Caddy proxy.
	//
	// These deliberately do NOT carry the managed-by label that ListContainers
	// filters on: that label marks containers the platform owns and may reap, and
	// the cleanup worker treats anything wearing it with no matching database row
	// as an orphan. Labelling the proxy that way would eventually delete it. So
	// system containers need a lookup of their own; the log collector uses it to
	// read Caddy's logs, which is where ACME failure reasons come from.
	ListSystemContainers(ctx context.Context) ([]ContainerInfo, error)
	// ListImages lists all images on the host (read-only, admin inspect page).
	ListImages(ctx context.Context) ([]ImageInfo, error)
	// ResolveImageDigest inspects a locally-present image reference and returns a
	// digest-pinned reference ("repo@sha256:…") from its RepoDigests. Returns an
	// empty string (nil error) when the image has no repo digest (e.g. built
	// locally, never pushed). Used to pin database / prebuilt-image versions so
	// recreates don't silently follow a moved mutable tag.
	ResolveImageDigest(ctx context.Context, ref string) (string, error)
	// ListVolumes lists all volumes on the host with on-disk sizes (read-only).
	ListVolumes(ctx context.Context) ([]VolumeInfo, error)
	// ListNetworks lists all networks on the host with attached containers (read-only).
	ListNetworks(ctx context.Context) ([]NetworkInfo, error)
	// SystemInfo returns a trimmed `docker info` for the overview page (read-only).
	SystemInfo(ctx context.Context) (*DockerSystemInfo, error)
	// SystemDiskUsage returns a `docker system df` summary (read-only).
	SystemDiskUsage(ctx context.Context) (*DockerDiskUsage, error)
	PullImage(ctx context.Context, image string) error
	BuildImage(ctx context.Context, contextDir, dockerfile, tag string) error
	CreateNetwork(ctx context.Context, name string) error
	RemoveNetwork(ctx context.Context, name string) error
	ConnectContainerToNetwork(ctx context.Context, containerID, networkName string) error
	CreateVolume(ctx context.Context, name string) error
	// CreateCacheVolume creates (or no-ops on) a Docker volume labelled as a
	// build cache. The cache label opts the volume out of PruneVolumes so the
	// periodic cleanup worker cannot wipe layer history between builds.
	CreateCacheVolume(ctx context.Context, name string) error
	// CreateDataVolume creates (or no-ops on) a Docker volume labelled as
	// persistent application data. The data label opts the volume out of
	// PruneVolumes so the cleanup worker cannot delete user data while an app's
	// container is absent between deploys. Idempotent.
	CreateDataVolume(ctx context.Context, name string) error
	// VolumeSize reports the on-disk size in bytes of a named volume, or
	// (0, nil) when the volume does not exist. Used to surface per-app cache
	// usage in the UI.
	VolumeSize(ctx context.Context, name string) (int64, error)
	// VolumeSizes reports on-disk sizes for multiple volumes in a single
	// Docker DiskUsage call. Use this when you need sizes for several volumes
	// at once — DiskUsage computes sizes for every volume on the host, so
	// calling VolumeSize N times does N full scans.
	VolumeSizes(ctx context.Context, names []string) (map[string]int64, error)
	RemoveVolume(ctx context.Context, name string) error
	RemoveImage(ctx context.Context, image string) error
	PruneImages(ctx context.Context) error
	PruneVolumes(ctx context.Context) error
	// PruneBuildCache reclaims build caches: it removes the platform's per-app
	// CNB cache volumes (labelled belune-cache, which PruneVolumes deliberately
	// preserves) and prunes the BuildKit builder cache. Disposable — the next
	// build repopulates them.
	PruneBuildCache(ctx context.Context) error
	// ImageExists reports whether an image is present in the local store.
	// "not found" is (false, nil); other failures are surfaced as errors.
	ImageExists(ctx context.Context, ref string) (bool, error)
	ContainerStats(ctx context.Context, containerID string) (*ContainerResourceStats, error)
	// ContainerHealth returns Docker's health status for a container: one of
	// "healthy", "unhealthy", "starting", or "none" (no HEALTHCHECK configured).
	ContainerHealth(ctx context.Context, containerID string) (string, error)
	ContainerEvents(ctx context.Context, filters map[string][]string) (<-chan ContainerEvent, <-chan error)
	// ContainerExecTTY creates a new exec session in the named container with TTY enabled.
	// cmd is the command to run (e.g. ["sh"] or ["bash"]).
	// Returns a TerminalExecSession with an exec ID (for resize) and a combined RWC.
	ContainerExecTTY(ctx context.Context, containerName string, cmd []string) (*TerminalExecSession, error)
	// HostShellSession launches a privileged helper container in the host's PID
	// namespace and execs a root shell into the host via nsenter, using `image`
	// (which must contain nsenter). The returned session's RWC.Close() removes the
	// helper. Grants host root — callers must gate it hard.
	HostShellSession(ctx context.Context, image string) (*TerminalExecSession, error)
	// ContainerExecResize resizes the PTY for the given exec session.
	ContainerExecResize(ctx context.Context, execID string, rows, cols uint) error
	// ContainerExec runs a command in the named container without a TTY and
	// blocks until it exits. stdin (if non-nil) is streamed to the command;
	// stdout and stderr are written to the respective writers (either may be
	// nil to discard). Returns the command's exit code. Used for managed-database
	// logical dump/restore (pg_dump → stdout file, restore ← stdin file).
	ContainerExec(ctx context.Context, containerName string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// RunHelper creates and runs a short-lived container to completion, streaming
	// stdin in and stdout/stderr out (stdcopy-demuxed, non-TTY), then removes it.
	// Used for cold volume tar snapshot/restore against a stopped database's
	// volume (the helper mounts the volume and runs tar). Returns the exit code.
	RunHelper(ctx context.Context, cfg ContainerConfig, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// SpawnUpdateHelper launches a detached container from the TARGET
	// image that runs `/usr/local/bin/belune-update <version>` against the host
	// install directory, then exits on its own. Unlike RunHelper it is NOT attached to or waited on —
	// it must outlive the caller, because `docker compose up -d` inside it is
	// what replaces the calling (belune) container. Returns the helper's
	// container ID immediately after it starts. Labelled LabelHelper, same as
	// RunHelper's containers, so the orphan reaper spares it while running and
	// reaps it like any other exited helper afterward.
	SpawnUpdateHelper(ctx context.Context, cfg UpdateHelperConfig) (string, error)
}

// UpdateHelperConfig configures the detached self-update helper started by
// SpawnUpdateHelper.
type UpdateHelperConfig struct {
	// TargetImage is the image of the release being installed, NOT Belune's
	// current one: the helper runs the updater baked into it, so a fix to the
	// updater reaches the very update that installs it. The caller must have
	// pulled it already — container creation fails on an absent image, and a
	// failed pull has to abort before anything on the host is touched.
	TargetImage string
	// WorkingDir is the host's compose project directory — where
	// docker-compose.yml, .env and scripts/ live — read off this container's
	// own com.docker.compose.project.working_dir label. Bind-mounted into the
	// helper at the SAME path, so the updater's relative paths resolve exactly
	// as they would running directly on the host. The updater itself is NOT
	// under this mount (it is /usr/local/bin/belune-update in the image), which
	// is what stops it resolving to the host's older copy.
	WorkingDir string
	// Version is the target release, passed to belune-update as its sole
	// argument. Always resolved from the cached manifest ahead of time, never
	// re-resolved inside the helper — the operator updates to exactly the
	// version the Server-page card showed them, not whatever happens to be
	// latest by the time the helper runs.
	Version string
}

// ContainerEvent represents a Docker container lifecycle event.
type ContainerEvent struct {
	ContainerID   string
	ContainerName string
	Status        string // start, stop, die, restart, oom
	Labels        map[string]string
	Time          time.Time
}

// ImageInfo describes a Docker image for the read-only admin inspect view.
type ImageInfo struct {
	ID         string
	RepoTags   []string
	Size       int64
	SharedSize int64 // shared layer bytes; -1 if not computed
	Containers int64 // containers using this image; -1 if unknown
	Dangling   bool  // untagged (no usable RepoTags)
	Labels     map[string]string
	CreatedAt  time.Time
}

// VolumeInfo describes a Docker volume for the read-only admin inspect view.
type VolumeInfo struct {
	Name       string
	Driver     string
	Mountpoint string
	Scope      string
	Size       int64 // on-disk bytes; -1 if unknown
	RefCount   int64 // containers referencing; -1 if unknown
	Labels     map[string]string
	CreatedAt  time.Time
}

// NetworkContainer is a container attached to a Docker network.
type NetworkContainer struct {
	ID          string
	Name        string
	IPv4Address string
}

// NetworkInfo describes a Docker network for the read-only admin inspect view.
type NetworkInfo struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Labels     map[string]string
	CreatedAt  time.Time
	Containers []NetworkContainer
}

// DockerSystemInfo is a trimmed view of `docker info` for the overview page.
// JSON tags are snake_case because it is returned directly on the admin API.
type DockerSystemInfo struct {
	ServerVersion     string `json:"server_version"`
	OperatingSystem   string `json:"operating_system"`
	OSType            string `json:"os_type"`
	Architecture      string `json:"architecture"`
	KernelVersion     string `json:"kernel_version"`
	StorageDriver     string `json:"storage_driver"`
	LoggingDriver     string `json:"logging_driver"`
	CgroupDriver      string `json:"cgroup_driver"`
	NCPU              int    `json:"ncpu"`
	MemTotal          int64  `json:"mem_total"`
	DockerRootDir     string `json:"docker_root_dir"`
	Name              string `json:"name"`
	Containers        int    `json:"containers"`
	ContainersRunning int    `json:"containers_running"`
	ContainersPaused  int    `json:"containers_paused"`
	ContainersStopped int    `json:"containers_stopped"`
	Images            int    `json:"images"`
}

// DiskUsageEntry summarizes disk usage for one Docker object type.
type DiskUsageEntry struct {
	Count       int   `json:"count"`
	Size        int64 `json:"size"`
	Reclaimable int64 `json:"reclaimable"`
}

// DockerDiskUsage summarizes `docker system df` for the overview page.
type DockerDiskUsage struct {
	LayersSize int64          `json:"layers_size"`
	Images     DiskUsageEntry `json:"images"`
	Containers DiskUsageEntry `json:"containers"`
	Volumes    DiskUsageEntry `json:"volumes"`
	BuildCache DiskUsageEntry `json:"build_cache"`
}
