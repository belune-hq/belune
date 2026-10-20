package docker

import (
	"context"
	"fmt"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/docker/docker/api/types/container"

	"github.com/belune-hq/belune/internal/runtime"
)

// hostShellRWC is the exec stream for a host shell, extended so that closing it
// also force-removes the throwaway privileged helper container. The helper runs
// `sleep infinity`, so it will not stop on its own when the exec'd shell exits —
// it must be removed explicitly.
type hostShellRWC struct {
	hijackedRWC
	cli         *dockerclient.Client
	containerID string
}

func (h *hostShellRWC) Close() error {
	_ = h.hijackedRWC.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = h.cli.ContainerRemove(ctx, h.containerID, container.RemoveOptions{Force: true})
	return nil
}

// HostShellSession launches a privileged helper container that shares the host's
// PID namespace, then execs a root shell into the host itself via
// `nsenter -t 1`. `image` must contain nsenter — Belune's Debian-based image
// does, and reusing it means no extra pull. The returned session's RWC.Close()
// force-removes the helper.
//
// This yields root on the host. It is only reachable because Belune holds the
// Docker socket (which is already host-root-equivalent by design); the caller
// MUST gate it hard (enabled setting + admin + re-auth + audit).
func (c *Client) HostShellSession(ctx context.Context, image string) (*runtime.TerminalExecSession, error) {
	created, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image: image,
			// The helper only needs to stay alive so we can exec into it; the real
			// shell is the nsenter exec below. Override the belune entrypoint.
			Entrypoint: []string{"sleep"},
			Cmd:        []string{"infinity"},
			User:       "0:0", // nsenter needs root; the belune image defaults to non-root
			Labels:     map[string]string{"belune-host-shell": "true"},
		},
		&container.HostConfig{
			Privileged: true,
			PidMode:    "host",
			AutoRemove: true,
		},
		nil, nil, "", // empty name → daemon assigns a unique one
	)
	if err != nil {
		return nil, fmt.Errorf("create host-shell helper: %w", err)
	}

	removeHelper := func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = c.cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
	}

	if err := c.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		removeHelper()
		return nil, fmt.Errorf("start host-shell helper: %w", err)
	}

	execResp, err := c.cli.ContainerExecCreate(ctx, created.ID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		User:         "0:0",
		// Enter the host init's (PID 1) mount/uts/ipc/net/pid namespaces, then sh.
		Cmd: []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sh"},
	})
	if err != nil {
		removeHelper()
		return nil, fmt.Errorf("host-shell exec create: %w", err)
	}

	hr, err := c.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		removeHelper()
		return nil, fmt.Errorf("host-shell exec attach: %w", err)
	}

	return &runtime.TerminalExecSession{
		ExecID: execResp.ID,
		RWC:    &hostShellRWC{hijackedRWC: hijackedRWC{resp: hr}, cli: c.cli, containerID: created.ID},
	}, nil
}
