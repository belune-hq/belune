package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/belune-hq/belune/internal/runtime"
)

// RunHelper creates a short-lived container, attaches to it, runs it to
// completion while streaming stdin in and stdout/stderr out, then force-removes
// it. The image must already be pulled by the caller. Only Image, Cmd, and
// Volumes from cfg are used (network is forced to "none" — helpers only touch a
// mounted volume and must not reach the project network).
func (c *Client) RunHelper(ctx context.Context, cfg runtime.ContainerConfig, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var binds []string
	for hostPath, containerPath := range cfg.Volumes {
		binds = append(binds, fmt.Sprintf("%s:%s", hostPath, containerPath))
	}

	resp, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        cfg.Image,
			Cmd:          cfg.Cmd,
			AttachStdin:  stdin != nil,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    stdin != nil,
			StdinOnce:    stdin != nil,
			Labels: map[string]string{
				labelManagedBy: labelValue,
				// Marks this as work in progress rather than a workload. The
				// orphan sweep spares a running one: it holds a volume open and
				// killing it mid-operation is how a restore loses data.
				runtime.LabelHelper: "true",
			},
		},
		&container.HostConfig{
			Binds:       binds,
			NetworkMode: "none",
		},
		nil, nil, "",
	)
	if err != nil {
		return -1, fmt.Errorf("helper create: %w", err)
	}
	// Best-effort cleanup regardless of outcome.
	defer func() {
		_ = c.cli.ContainerRemove(context.WithoutCancel(ctx), resp.ID, container.RemoveOptions{Force: true})
	}()

	hr, err := c.cli.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  stdin != nil,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return -1, fmt.Errorf("helper attach: %w", err)
	}
	defer hr.Close()

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return -1, fmt.Errorf("helper start: %w", err)
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(hr.Conn, stdin)
			_ = hr.CloseWrite()
		}()
	}

	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if _, err := stdcopy.StdCopy(stdout, stderr, hr.Reader); err != nil {
		return -1, fmt.Errorf("helper stream copy: %w", err)
	}

	statusCh, errCh := c.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case werr := <-errCh:
		if werr != nil {
			return -1, fmt.Errorf("helper wait: %w", werr)
		}
		return -1, fmt.Errorf("helper wait: closed without status")
	case st := <-statusCh:
		return int(st.StatusCode), nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}
