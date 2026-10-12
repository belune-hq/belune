package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/service"
)

type setDatabaseResourcesInput struct {
	DatabaseID  string  `json:"database_id" jsonschema:"the database's id"`
	CPULimit    float64 `json:"cpu_limit" jsonschema:"CPU cores, e.g. 0.5 or 2; 0 means unlimited"`
	MemoryLimit int64   `json:"memory_limit" jsonschema:"memory in BYTES, e.g. 536870912 for 512 MiB; 0 means unlimited"`
}

type setDatabaseResourcesResult struct {
	Database database `json:"database"`
	Note     string   `json:"note"`
}

func registerDatabaseSettingsTools(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_database_resources",
		Description: "Set a managed database's CPU and memory limits. Both are REPLACED by what you pass, so read the " +
			"current values with get_database first and pass both — omitting one sets it to 0, which means unlimited. " +
			"cpu_limit is in cores; memory_limit is in BYTES, not megabytes. " +
			"⚠️ Unlike the application equivalent this takes effect IMMEDIATELY on the running database container, " +
			"with no redeploy and no confirmation step: too small a memory limit can make the database fail under " +
			"load or be killed outright, so change it deliberately and read the current value first. A limit below " +
			"6 MiB is refused because Docker cannot apply one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setDatabaseResourcesInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.DatabaseID)
		if err != nil {
			return nil, nil, err
		}
		// Resolve the database by its OWN id, then authorize against the
		// project that row actually belongs to — never a caller-supplied
		// project id, of which these tools have none.
		db, err := d.Queries.GetDatabase(ctx, id)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "database not found")
		}
		if err := authorizeDatabase(ctx, d.Queries, db.ID, db.ProjectID); err != nil {
			return nil, nil, err
		}

		if in.CPULimit < 0 || in.MemoryLimit < 0 {
			return nil, nil, errors.New("limits cannot be negative; 0 means unlimited")
		}
		// Same floor and same reasoning as set_application_resources, and it
		// matters more here because the change is applied live.
		if in.MemoryLimit > 0 && in.MemoryLimit < minMemoryLimitBytes {
			return nil, nil, errors.New("memory_limit must be 0 (unlimited) or at least 6291456 bytes (6 MiB) — " +
				"Docker cannot apply a smaller limit. Note the unit is bytes, not megabytes")
		}

		updated, appliedLive, err := d.DBs.SetResources(ctx, db.ID, in.CPULimit, in.MemoryLimit)
		if err != nil {
			if errors.Is(err, service.ErrNegativeLimit) {
				return nil, nil, err
			}
			return nil, nil, internalError("failed to update database resources", err)
		}

		auditTool(ctx, d.Audit, "set_database_resources", "database", uuidToString(db.ID),
			map[string]any{"cpu_limit": in.CPULimit, "memory_limit": in.MemoryLimit})

		return textResult(setDatabaseResourcesResult{
			Database: toDatabase(updated),
			Note:     setDatabaseResourcesNote(appliedLive),
		})
	})
}

// setDatabaseResourcesNote says what actually happened.
//
// ⚠️ It takes the service's appliedLive result rather than inferring from the
// database's status, which is what an earlier version of this did. A live
// apply is best-effort — the limit is persisted either way and the failure is
// only logged — so "running" does NOT imply the new limits are in force, and
// saying they were would have the assistant report success for a change the
// container had not taken.
func setDatabaseResourcesNote(appliedLive bool) string {
	if appliedLive {
		return "saved, and applied to the running container"
	}
	return "saved; it takes effect the next time the container is created"
}
