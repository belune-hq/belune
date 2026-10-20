package docker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	networktypes "github.com/docker/docker/api/types/network"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/runtime"
)

func (c *Client) CreateNetwork(ctx context.Context, name string) (err error) {
	defer func() { metrics.RecordDockerOp("create_network", err) }()
	// Check if network already exists
	networks, err := c.cli.NetworkList(ctx, networktypes.ListOptions{})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == name {
			return nil // Already exists
		}
	}

	_, err = c.cli.NetworkCreate(ctx, name, networktypes.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			labelManagedBy: labelValue,
		},
	})
	if err != nil {
		return fmt.Errorf("create network %s: %w", name, err)
	}

	return nil
}

// RemoveNetwork tears down a project network. Caddy and the control-plane
// container are bridged into project networks and stay running, so a plain
// NetworkRemove fails with "network has active endpoints". This force-disconnects
// every remaining endpoint first (app/DB containers should already be removed by
// the caller; this covers Caddy, the API, and any stragglers), then removes the
// network. Idempotent: a missing network is treated as success.
func (c *Client) RemoveNetwork(ctx context.Context, name string) (err error) {
	defer func() { metrics.RecordDockerOp("remove_network", err) }()

	if inspect, insErr := c.cli.NetworkInspect(ctx, name, networktypes.InspectOptions{}); insErr == nil {
		for id := range inspect.Containers {
			if dErr := c.cli.NetworkDisconnect(ctx, name, id, true); dErr != nil {
				slog.Warn("could not disconnect endpoint before network removal",
					"network", name, "container", id, "error", dErr)
			}
		}
	}

	if err = c.cli.NetworkRemove(ctx, name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	return nil
}

// ListNetworks lists all networks on the host for the read-only admin inspect
// page. NetworkList does not populate attached containers, so each network is
// inspected (best-effort) to surface which containers are wired to it.
func (c *Client) ListNetworks(ctx context.Context) (result []runtime.NetworkInfo, err error) {
	defer func() { metrics.RecordDockerOp("list_networks", err) }()

	networks, err := c.cli.NetworkList(ctx, networktypes.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}

	result = make([]runtime.NetworkInfo, 0, len(networks))
	for _, n := range networks {
		info := runtime.NetworkInfo{
			ID:        n.ID,
			Name:      n.Name,
			Driver:    n.Driver,
			Scope:     n.Scope,
			Internal:  n.Internal,
			Labels:    n.Labels,
			CreatedAt: n.Created,
		}
		// Inspect to resolve attached containers; ignore per-network errors so
		// one removed network mid-list doesn't fail the whole page.
		if inspect, insErr := c.cli.NetworkInspect(ctx, n.ID, networktypes.InspectOptions{}); insErr == nil {
			for id, ep := range inspect.Containers {
				info.Containers = append(info.Containers, runtime.NetworkContainer{
					ID:          id,
					Name:        ep.Name,
					IPv4Address: ep.IPv4Address,
				})
			}
		}
		result = append(result, info)
	}
	return result, nil
}

// ConnectContainerToNetwork attaches a container to a Docker network. The
// operation is idempotent — if the container is already attached to the
// network, the call is a no-op rather than an error. This matters because
// Caddy is attached to per-project networks on every deploy and we should
// not surface "endpoint already exists" warnings for the steady state.
func (c *Client) ConnectContainerToNetwork(ctx context.Context, containerID, networkName string) (err error) {
	defer func() { metrics.RecordDockerOp("connect_network", err) }()
	if err = c.cli.NetworkConnect(ctx, networkName, containerID, nil); err != nil {
		// Docker returns 403 with a message containing "already exists" when
		// the endpoint is already wired up. errdefs would be cleaner but a
		// plain string match avoids pulling in another import for a single
		// check.
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("connect container %s to network %s: %w", containerID, networkName, err)
	}
	return nil
}
