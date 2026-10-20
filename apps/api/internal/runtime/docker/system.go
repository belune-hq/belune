package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/runtime"
)

// SystemInfo returns a trimmed `docker info` for the read-only overview page.
func (c *Client) SystemInfo(ctx context.Context) (_ *runtime.DockerSystemInfo, err error) {
	defer func() { metrics.RecordDockerOp("system_info", err) }()

	info, err := c.cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker info: %w", err)
	}
	return &runtime.DockerSystemInfo{
		ServerVersion:     info.ServerVersion,
		OperatingSystem:   info.OperatingSystem,
		OSType:            info.OSType,
		Architecture:      info.Architecture,
		KernelVersion:     info.KernelVersion,
		StorageDriver:     info.Driver,
		LoggingDriver:     info.LoggingDriver,
		CgroupDriver:      info.CgroupDriver,
		NCPU:              info.NCPU,
		MemTotal:          info.MemTotal,
		DockerRootDir:     info.DockerRootDir,
		Name:              info.Name,
		Containers:        info.Containers,
		ContainersRunning: info.ContainersRunning,
		ContainersPaused:  info.ContainersPaused,
		ContainersStopped: info.ContainersStopped,
		Images:            info.Images,
	}, nil
}

// SystemDiskUsage returns a `docker system df` summary for the overview page.
// Reclaimable figures mirror the Docker CLI's own computation so operators see
// the same numbers as `docker system df`.
func (c *Client) SystemDiskUsage(ctx context.Context) (_ *runtime.DockerDiskUsage, err error) {
	defer func() { metrics.RecordDockerOp("system_disk_usage", err) }()

	du, err := c.cli.DiskUsage(ctx, types.DiskUsageOptions{})
	if err != nil {
		return nil, fmt.Errorf("disk usage: %w", err)
	}

	out := &runtime.DockerDiskUsage{LayersSize: du.LayersSize}

	// Images: total size is the shared layer size; reclaimable is that minus the
	// unique size consumed by images still referenced by a container.
	var usedByActive int64
	for _, img := range du.Images {
		if img == nil {
			continue
		}
		if img.Containers != 0 && img.Size != -1 && img.SharedSize != -1 {
			usedByActive += img.Size - img.SharedSize
		}
	}
	out.Images = runtime.DiskUsageEntry{
		Count:       len(du.Images),
		Size:        du.LayersSize,
		Reclaimable: du.LayersSize - usedByActive,
	}

	// Containers: writable-layer bytes; reclaimable when not running.
	for _, ctr := range du.Containers {
		if ctr == nil {
			continue
		}
		out.Containers.Count++
		out.Containers.Size += ctr.SizeRw
		if string(ctr.State) != "running" {
			out.Containers.Reclaimable += ctr.SizeRw
		}
	}

	// Volumes: on-disk bytes; reclaimable when unreferenced.
	for _, v := range du.Volumes {
		if v == nil || v.UsageData == nil {
			continue
		}
		out.Volumes.Count++
		out.Volumes.Size += v.UsageData.Size
		if v.UsageData.RefCount <= 0 {
			out.Volumes.Reclaimable += v.UsageData.Size
		}
	}

	// Build cache: reclaimable when not in use.
	for _, bc := range du.BuildCache {
		if bc == nil {
			continue
		}
		out.BuildCache.Count++
		out.BuildCache.Size += bc.Size
		if !bc.InUse {
			out.BuildCache.Reclaimable += bc.Size
		}
	}

	return out, nil
}
