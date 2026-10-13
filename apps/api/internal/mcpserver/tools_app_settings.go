package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// minMemoryLimitBytes is the floor this package enforces on a memory limit,
// above zero.
//
// ⚠️ Not arbitrary, and not mirrored from REST: Docker refuses to create a
// container with a memory limit under 6 MiB, so any smaller positive value
// produces an application that cannot start at all — and the REST endpoint
// accepts it, because a human typing into a form with the current value in
// front of them does not make that mistake. An assistant filling a number
// from conversation does. The floor is here rather than in the service so the
// REST wire contract is unchanged; see the changelog note.
const minMemoryLimitBytes = 6 << 20 // 6 MiB, Docker's own minimum

type setResourcesInput struct {
	ApplicationID string  `json:"application_id" jsonschema:"the application's id"`
	CPULimit      float64 `json:"cpu_limit" jsonschema:"CPU cores, e.g. 0.5 or 2; 0 means unlimited"`
	MemoryLimit   int64   `json:"memory_limit" jsonschema:"memory in BYTES, e.g. 536870912 for 512 MiB; 0 means unlimited"`
}

type setRuntimeInput struct {
	ApplicationID  string `json:"application_id" jsonschema:"the application's id"`
	ReadonlyRootfs bool   `json:"readonly_rootfs" jsonschema:"mount the container's root filesystem read-only"`
	ContainerCaps  string `json:"container_caps" jsonschema:"'minimal' drops all Linux capabilities; 'standard' keeps Docker's default set"`
}

type setHealthCheckInput struct {
	ApplicationID string `json:"application_id" jsonschema:"the application's id"`
	Type          string `json:"type" jsonschema:"one of none, http, command"`

	Path         string `json:"path,omitempty" jsonschema:"http only: the path to probe, must start with /"`
	ExpectStatus int    `json:"expect_status,omitempty" jsonschema:"http only: the status code to treat as healthy; omit for the default"`

	Command            string `json:"command,omitempty" jsonschema:"command only: the command to run inside the container"`
	IntervalSeconds    int    `json:"interval_seconds,omitempty" jsonschema:"command only: seconds between checks; omit for the default"`
	Retries            int    `json:"retries,omitempty" jsonschema:"command only: consecutive failures before unhealthy; omit for the default"`
	StartPeriodSeconds int    `json:"start_period_seconds,omitempty" jsonschema:"command only: grace period after start; omit for the default"`

	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"seconds before a single check is considered failed; omit for the default"`
}

type setPortInput struct {
	ApplicationID string `json:"application_id" jsonschema:"the application's id"`
	ContainerPort int    `json:"container_port" jsonschema:"the port the application listens on inside its container, 1-65535; 0 clears it"`
}

// settingsResult is the shared shape for the application settings tools:
// the updated application plus a note saying when the change takes effect,
// which differs per tool and is the thing an assistant most often gets wrong.
type settingsResult struct {
	Application application `json:"application"`
	Note        string      `json:"note"`
}

func registerApplicationSettingsTools(srv *mcp.Server, d Deps) {
	// authorizedApp is the preamble all of them share: resolve the application by
	// its OWN id, then authorize against the project that row actually belongs
	// to. Never a project id from the caller — there is no such argument on
	// these tools, which is what makes them immune to naming a parent you may
	// reach and a child you may not.
	authorizedApp := func(ctx context.Context, applicationID string) (generated.Application, error) {
		id, err := parseUUID(applicationID)
		if err != nil {
			return generated.Application{}, err
		}
		app, err := d.Queries.GetApplication(ctx, id)
		if err != nil {
			return generated.Application{}, notFoundOr(ctx, "application not found")
		}
		if err := authorizeApplication(ctx, d.Queries, app.ID, app.ProjectID); err != nil {
			return generated.Application{}, err
		}
		return app, nil
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_application_resources",
		Description: "Set an application's CPU and memory limits. Both are REPLACED by what you pass, so read the " +
			"current values with get_application first and pass both — omitting one sets it to 0, which means " +
			"unlimited. cpu_limit is in cores (0.5 is half a core); memory_limit is in BYTES, not megabytes. " +
			"The change is saved immediately but the running container keeps its current limits until the " +
			"application is redeployed — call trigger_deployment to apply them. A memory limit below 6 MiB is " +
			"refused because Docker cannot start a container with one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setResourcesInput) (*mcp.CallToolResult, any, error) {
		app, err := authorizedApp(ctx, in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		if in.CPULimit < 0 || in.MemoryLimit < 0 {
			return nil, nil, errors.New("limits cannot be negative; 0 means unlimited")
		}
		if in.MemoryLimit > 0 && in.MemoryLimit < minMemoryLimitBytes {
			return nil, nil, errors.New("memory_limit must be 0 (unlimited) or at least 6291456 bytes (6 MiB) — " +
				"Docker cannot create a container with a smaller limit. Note the unit is bytes, not megabytes")
		}

		updated, err := d.Queries.SetApplicationResources(ctx, generated.SetApplicationResourcesParams{
			ID:          app.ID,
			CpuLimit:    in.CPULimit,
			MemoryLimit: in.MemoryLimit,
		})
		if err != nil {
			return nil, nil, internalError("failed to update resources", err)
		}
		d.Apps.MarkConfigChanged(ctx, app.ID)

		// Same action, resource type and details shape as the REST endpoint, so
		// one filter finds a resource change however it was made.
		auditTool(ctx, d.Audit, "set_resources", "application", uuidToString(app.ID),
			map[string]any{"cpu_limit": in.CPULimit, "memory_limit": in.MemoryLimit})

		return textResult(settingsResult{
			Application: toApplication(updated),
			Note:        "saved, not applied — the running container keeps its current limits until trigger_deployment",
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_application_health_check",
		Description: "Configure how an application's health is checked. Pass type 'http' to probe a path after each " +
			"deploy, 'command' for a command run inside the container continuously, or 'none' to disable checking. " +
			"The WHOLE configuration is replaced: switching type clears the other type's settings, and any tuning " +
			"field you omit returns to its default. The change is saved immediately but does not affect the running " +
			"container until the application is redeployed — call trigger_deployment to apply it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setHealthCheckInput) (*mcp.CallToolResult, any, error) {
		app, err := authorizedApp(ctx, in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}

		// The same validator the REST endpoint uses, so the two cannot disagree
		// about a valid configuration — and so the unselected mechanism's
		// columns are cleared as one unit rather than left stale.
		params, err := service.ValidateHealthCheck(app.ID, service.HealthCheckInput{
			Type:               in.Type,
			Path:               in.Path,
			ExpectStatus:       in.ExpectStatus,
			Command:            in.Command,
			IntervalSeconds:    in.IntervalSeconds,
			RetriesCount:       in.Retries,
			StartPeriodSeconds: in.StartPeriodSeconds,
			TimeoutSeconds:     in.TimeoutSeconds,
		})
		if err != nil {
			return nil, nil, err
		}

		updated, err := d.Queries.SetApplicationHealthCheck(ctx, params)
		if err != nil {
			return nil, nil, internalError("failed to update health check", err)
		}
		d.Apps.MarkConfigChanged(ctx, app.ID)

		auditTool(ctx, d.Audit, "set_health_check", "application", uuidToString(app.ID),
			map[string]any{"type": in.Type})

		return textResult(settingsResult{
			Application: toApplication(updated),
			Note:        "saved, not applied — the running container is unaffected until trigger_deployment",
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_application_runtime",
		Description: "Set an application's container hardening profile. readonly_rootfs mounts the root filesystem " +
			"read-only, and container_caps 'minimal' drops all Linux capabilities while 'standard' keeps Docker's " +
			"default set. Hardened (read-only + minimal) is the default for an application's own code and is the " +
			"safer setting; relax it only when an image genuinely needs to write to its own filesystem or needs a " +
			"capability, and prefer a volume for writable paths. Both values are REPLACED by what you pass, so read " +
			"the current profile with get_application first. The change is saved immediately but does not affect the " +
			"running container until the application is redeployed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setRuntimeInput) (*mcp.CallToolResult, any, error) {
		app, err := authorizedApp(ctx, in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		if in.ContainerCaps != "minimal" && in.ContainerCaps != "standard" {
			return nil, nil, errors.New("container_caps must be 'minimal' or 'standard'")
		}

		if err := d.Queries.UpdateApplicationRuntime(ctx, generated.UpdateApplicationRuntimeParams{
			ID:             app.ID,
			ReadonlyRootfs: in.ReadonlyRootfs,
			ContainerCaps:  in.ContainerCaps,
		}); err != nil {
			return nil, nil, internalError("failed to update runtime profile", err)
		}
		d.Apps.MarkConfigChanged(ctx, app.ID)

		auditTool(ctx, d.Audit, "update_application_runtime", "application", uuidToString(app.ID),
			map[string]any{"readonly_rootfs": in.ReadonlyRootfs, "container_caps": in.ContainerCaps})

		// Re-read rather than trusting the input: the query is :exec, and the
		// result should say what the row holds, not what was asked for.
		updated, err := d.Queries.GetApplication(ctx, app.ID)
		if err != nil {
			return nil, nil, internalError("failed to read the updated application", err)
		}
		return textResult(settingsResult{
			Application: toApplication(updated),
			Note:        "saved, not applied — the running container keeps its current profile until trigger_deployment",
		})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_application_port",
		Description: "Set the port an application's container listens on. The deploy health check probes this port and " +
			"the proxy forwards to it, so an app that listens on anything but 8080 and has no domain with a port set " +
			"will fail its health check until this is set. Pass 0 to clear it, after which the port is taken from " +
			"the application's first domain and then defaults to 8080. container_port is required: it is REPLACED by " +
			"what you pass, so read the current value with get_application first. The change is saved immediately " +
			"but does not affect the running container or the proxy route until the application is redeployed — " +
			"call trigger_deployment to apply it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setPortInput) (*mcp.CallToolResult, any, error) {
		app, err := authorizedApp(ctx, in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		// The same validator the REST endpoint uses.
		port, err := service.ValidateContainerPort(in.ContainerPort)
		if err != nil {
			return nil, nil, err
		}

		if err := d.Queries.SetApplicationContainerPort(ctx, generated.SetApplicationContainerPortParams{
			ID:            app.ID,
			ContainerPort: port,
		}); err != nil {
			return nil, nil, internalError("failed to update container port", err)
		}
		d.Apps.MarkConfigChanged(ctx, app.ID)

		// Same action, resource type and details shape as the REST endpoint.
		auditTool(ctx, d.Audit, "set_container_port", "application", uuidToString(app.ID),
			map[string]any{"container_port": in.ContainerPort})

		updated, err := d.Queries.GetApplication(ctx, app.ID)
		if err != nil {
			return nil, nil, internalError("failed to read the updated application", err)
		}
		return textResult(settingsResult{
			Application: toApplication(updated),
			Note:        "saved, not applied — the health check and proxy route keep the old port until trigger_deployment",
		})
	})
}
