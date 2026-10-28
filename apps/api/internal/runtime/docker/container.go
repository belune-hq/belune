package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/runtime"
)

const labelManagedBy = "managed-by"
const labelValue = "belune"

// labelSystem marks Belune's own infrastructure containers (the Caddy proxy).
// Mirrors logcollector.labelSystem; kept here to avoid an import cycle.
const labelSystem = "belune-system"

func (c *Client) CreateContainer(ctx context.Context, cfg runtime.ContainerConfig) (id string, err error) {
	defer func() { metrics.RecordDockerOp("create_container", err) }()
	// Convert env map to Docker format
	var env []string
	for k, v := range cfg.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// Convert port map to Docker format
	hostBindIP := cfg.HostBindIP
	if hostBindIP == "" {
		hostBindIP = "0.0.0.0"
	}
	exposedPorts := nat.PortSet{}
	portBindings := nat.PortMap{}
	for hostPort, containerPort := range cfg.Ports {
		cp := nat.Port(containerPort + "/tcp")
		exposedPorts[cp] = struct{}{}
		portBindings[cp] = []nat.PortBinding{
			{HostIP: hostBindIP, HostPort: hostPort},
		}
	}

	// Convert volume map to Docker format
	var binds []string
	for hostPath, containerPath := range cfg.Volumes {
		binds = append(binds, fmt.Sprintf("%s:%s", hostPath, containerPath))
	}
	// Read-only file/config mounts: bind a managed host file at a container path.
	for _, b := range cfg.ReadOnlyBinds {
		binds = append(binds, fmt.Sprintf("%s:%s:ro", b.Source, b.Target))
	}

	labels := map[string]string{
		labelManagedBy: labelValue,
	}
	for k, v := range cfg.Labels {
		labels[k] = v
	}

	// Apply optional resource limits
	resources := container.Resources{}
	if cfg.CPULimit > 0 {
		resources.NanoCPUs = int64(cfg.CPULimit * 1e9)
	}
	if cfg.MemoryLimit > 0 {
		resources.Memory = cfg.MemoryLimit
	}

	// Native Docker HEALTHCHECK, only when a command health check is configured.
	// Left nil otherwise so the image's own HEALTHCHECK (usually none) is
	// untouched — the HTTP check is a separate control-plane probe, not this.
	var healthConfig *container.HealthConfig
	if cfg.HealthCheckCommand != "" {
		healthConfig = &container.HealthConfig{
			Test:        []string{"CMD-SHELL", cfg.HealthCheckCommand},
			Interval:    cfg.HealthCheckInterval,
			Timeout:     cfg.HealthCheckTimeout,
			Retries:     cfg.HealthCheckRetries,
			StartPeriod: cfg.HealthCheckStartPeriod,
		}
	}

	resp, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        cfg.Image,
			Env:          env,
			Cmd:          cfg.Cmd,
			ExposedPorts: exposedPorts,
			Labels:       labels,
			Healthcheck:  healthConfig,
		},
		&container.HostConfig{
			PortBindings:   portBindings,
			Binds:          binds,
			RestartPolicy:  container.RestartPolicy{Name: "unless-stopped"},
			Resources:      resources,
			CapDrop:        cfg.CapDrop,
			CapAdd:         cfg.CapAdd,
			SecurityOpt:    cfg.SecurityOpt,
			ReadonlyRootfs: cfg.ReadonlyRootfs,
			Tmpfs:          cfg.Tmpfs,
		},
		&network.NetworkingConfig{
			EndpointsConfig: func() map[string]*network.EndpointSettings {
				if cfg.Network == "" {
					return nil
				}
				return map[string]*network.EndpointSettings{
					cfg.Network: {},
				}
			}(),
		},
		nil,
		cfg.Name,
	)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	return resp.ID, nil
}

func (c *Client) StartContainer(ctx context.Context, id string) (err error) {
	defer func() { metrics.RecordDockerOp("start_container", err) }()
	return c.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (c *Client) StopContainer(ctx context.Context, id string) (err error) {
	defer func() { metrics.RecordDockerOp("stop_container", err) }()
	timeout := 30
	return c.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func (c *Client) RemoveContainer(ctx context.Context, id string) (err error) {
	defer func() { metrics.RecordDockerOp("remove_container", err) }()
	return c.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

// ContainerExists reports whether a container with the given name (or ID) is
// present on the host, running or stopped. A "no such container" inspect is
// reported as (false, nil); any other error is surfaced so a transient Docker
// failure is not mistaken for a missing container. Used to tell a genuinely
// removed container apart from a merely-stopped one.
func (c *Client) ContainerExists(ctx context.Context, name string) (exists bool, err error) {
	defer func() { metrics.RecordDockerOp("container_exists", err) }()

	_, err = c.cli.ContainerInspect(ctx, name)
	if err != nil {
		// Docker returns a 404 whose message contains "No such container" when the
		// container is absent. A plain string match (as ImageExists does for "No
		// such image") avoids pulling in errdefs for one check.
		if strings.Contains(err.Error(), "No such container") {
			return false, nil
		}
		return false, fmt.Errorf("inspect container %s: %w", name, err)
	}
	return true, nil
}

// RestartContainer restarts a container in place (stop, then start the same
// container). Docker gives it up to `timeout` seconds to stop gracefully before
// killing it.
func (c *Client) RestartContainer(ctx context.Context, id string, timeout int) (err error) {
	defer func() { metrics.RecordDockerOp("restart_container", err) }()
	return c.cli.ContainerRestart(ctx, id, container.StopOptions{Timeout: &timeout})
}

// UpdateContainerResources applies new CPU/memory limits to a running container
// live (no recreate), using the same conversion as create time. A zero value
// means unlimited for that dimension.
func (c *Client) UpdateContainerResources(ctx context.Context, id string, cpuCores float64, memoryBytes int64) (err error) {
	defer func() { metrics.RecordDockerOp("update_container", err) }()
	_, err = c.cli.ContainerUpdate(ctx, id, container.UpdateConfig{
		Resources: updateResources(cpuCores, memoryBytes),
	})
	return err
}

// updateResources builds the resource block for a live container update.
// Separated from the call so the swap rule below has a test that does not need
// a daemon — it is the part that was wrong and is invisible in a passing
// integration run, because the only caller logs the failure and carries on.
//
// Zero means "leave this limit alone", matching Docker's own treatment of 0 in
// an update: it is how an unlimited container stays unlimited here. ⚠️ It also
// means a limit cannot be REMOVED live — persisting 0 takes effect when the
// container is next created, which is the existing behaviour and is why the
// caller's reply distinguishes "applied" from "saved".
func updateResources(cpuCores float64, memoryBytes int64) container.Resources {
	resources := container.Resources{}
	if cpuCores > 0 {
		resources.NanoCPUs = int64(cpuCores * 1e9)
	}
	if memoryBytes > 0 {
		resources.Memory = memoryBytes
		// ⚠️ MemorySwap must be sent WITH Memory or the daemon refuses the
		// whole update: "Memory limit should be smaller than already set
		// memoryswap limit, update the memoryswap at the same time". A running
		// container always has a memoryswap value, so a memory-only update
		// never succeeded — and because the caller logs the failure and
		// carries on (the limit is persisted either way), it looked like
		// nothing had happened rather than like an error. Found by driving the
		// new MCP tool against a real database.
		//
		// 2x is not a choice so much as a match: creating a container with
		// only Memory set makes Docker default MemorySwap to twice it, so a
		// container whose limit arrived by live update ends up in the same
		// state as one recreated with it. Verified both ways against a real
		// daemon.
		resources.MemorySwap = memoryBytes * 2
	}
	return resources
}

func (c *Client) ContainerLogs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	return c.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Timestamps: true,
	})
}

func (c *Client) ContainerLogsTail(ctx context.Context, id string, tail int) (io.ReadCloser, error) {
	tailStr := "all"
	if tail > 0 {
		tailStr = strconv.Itoa(tail)
	}
	return c.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
		Tail:       tailStr,
		// Docker prefixes every line with an RFC3339Nano timestamp, which is the
		// only thing that reads uniformly across platform services: Caddy emits
		// a JSON "ts" field, Go's slog emits "time", and postgres/redis emit
		// plain text, so without this only Caddy showed a time. Callers strip
		// the prefix (see splitTimestamp in the collector, and parse.ts).
		Timestamps: true,
	})
}

func (c *Client) ContainerLogsSince(ctx context.Context, id string, since time.Time) (io.ReadCloser, error) {
	// An empty Since tells Docker to return the full buffered log history.
	// A non-zero time resumes from that point onward.
	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.Format(time.RFC3339Nano)
	}
	return c.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
		Since:      sinceStr,
	})
}

func (c *Client) ListContainers(ctx context.Context) ([]runtime.ContainerInfo, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", fmt.Sprintf("%s=%s", labelManagedBy, labelValue)),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return mapContainerSummaries(containers), nil
}

// ListSystemContainers lists Belune's own infrastructure containers, matched on
// the belune-system label. They are not managed-by=belune (see the interface
// doc: that label makes the cleanup worker consider them orphans), so
// ListContainers filters them out at the Docker API and never sees them.
func (c *Client) ListSystemContainers(ctx context.Context) ([]runtime.ContainerInfo, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", labelSystem),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("list system containers: %w", err)
	}
	return mapContainerSummaries(containers), nil
}

// ListAllContainers lists every container on the host (running and stopped),
// including ones not managed by the platform. Read-only; used by the admin
// Docker inspect page. It intentionally omits the managed-by label filter that
// ListContainers applies for health probes and metrics.
func (c *Client) ListAllContainers(ctx context.Context) (result []runtime.ContainerInfo, err error) {
	defer func() { metrics.RecordDockerOp("list_all_containers", err) }()
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list all containers: %w", err)
	}
	return mapContainerSummaries(containers), nil
}

// mapContainerSummaries converts Docker container summaries into the runtime DTO.
func mapContainerSummaries(containers []container.Summary) []runtime.ContainerInfo {
	result := make([]runtime.ContainerInfo, 0, len(containers))
	for _, ctr := range containers {
		name := ""
		if len(ctr.Names) > 0 {
			name = ctr.Names[0]
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}
		}

		ports := make(map[string]string)
		for _, p := range ctr.Ports {
			if p.PublicPort > 0 {
				ports[fmt.Sprintf("%d", p.PublicPort)] = fmt.Sprintf("%d", p.PrivatePort)
			}
		}

		var addrs []string
		if ctr.NetworkSettings != nil {
			for _, n := range ctr.NetworkSettings.Networks {
				if n != nil && n.IPAddress != "" {
					addrs = append(addrs, n.IPAddress)
				}
				if n != nil && n.GlobalIPv6Address != "" {
					addrs = append(addrs, n.GlobalIPv6Address)
				}
			}
		}

		result = append(result, runtime.ContainerInfo{
			ID:        ctr.ID,
			Name:      name,
			Image:     ctr.Image,
			Status:    string(ctr.State),
			Ports:     ports,
			Labels:    ctr.Labels,
			CreatedAt: time.Unix(ctr.Created, 0),
			Addrs:     addrs,
		})
	}
	return result
}

// ContainerHealth reports Docker's health status for a container. A container
// with no HEALTHCHECK has no Health object at all, which is reported as "none"
// so callers can tell "no check configured" apart from "check not yet run".
func (c *Client) ContainerHealth(ctx context.Context, containerID string) (string, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if info.State == nil || info.State.Health == nil {
		return "none", nil
	}
	return info.State.Health.Status, nil
}

func (c *Client) ContainerStats(ctx context.Context, containerID string) (*runtime.ContainerResourceStats, error) {
	resp, err := c.cli.ContainerStats(ctx, containerID, false)
	if err != nil {
		return nil, fmt.Errorf("container stats: %w", err)
	}
	defer resp.Body.Close()

	var stats container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}

	// Calculate CPU percentage
	var cpuPercent float64
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
	if systemDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / systemDelta) * float64(stats.CPUStats.OnlineCPUs) * 100.0
	}

	// Calculate network rx/tx
	var rxBytes, txBytes int64
	for _, netStats := range stats.Networks {
		rxBytes += int64(netStats.RxBytes)
		txBytes += int64(netStats.TxBytes)
	}

	return &runtime.ContainerResourceStats{
		CPUPercent:     cpuPercent,
		MemoryUsage:    int64(stats.MemoryStats.Usage),
		MemoryLimit:    int64(stats.MemoryStats.Limit),
		NetworkRxBytes: rxBytes,
		NetworkTxBytes: txBytes,
	}, nil
}

func (c *Client) ContainerEvents(ctx context.Context, eventFilters map[string][]string) (<-chan runtime.ContainerEvent, <-chan error) {
	outCh := make(chan runtime.ContainerEvent, 64)
	errCh := make(chan error, 1)

	f := filters.NewArgs()
	f.Add("type", string(events.ContainerEventType))
	for key, vals := range eventFilters {
		for _, v := range vals {
			f.Add(key, v)
		}
	}

	msgCh, dockerErrCh := c.cli.Events(ctx, events.ListOptions{Filters: f})

	go func() {
		defer close(outCh)
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-dockerErrCh:
				if !ok {
					return
				}
				errCh <- err
				return
			case msg, ok := <-msgCh:
				if !ok {
					return
				}
				name := msg.Actor.Attributes["name"]
				name = strings.TrimPrefix(name, "/")
				outCh <- runtime.ContainerEvent{
					ContainerID:   msg.Actor.ID,
					ContainerName: name,
					Status:        string(msg.Action),
					Labels:        msg.Actor.Attributes,
					Time:          time.Unix(msg.Time, msg.TimeNano),
				}
			}
		}
	}()

	return outCh, errCh
}
