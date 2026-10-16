// Package mcpserver implements Belune's Model Context Protocol server. It is
// registered as a single stateless JSON-RPC-over-HTTP handler at POST /mcp
// (see internal/server/routes.go) so an AI assistant can inspect and operate
// projects, applications, deployments and infrastructure state through the
// same personal-access-token model a CI script would use.
//
// Destructive tools (delete, restore, anything dropping a volume or backup)
// are out of scope permanently: a token cannot destroy, so registerXTools
// functions in this package must never add one. Mutating tools are a later
// phase than the read tools, and each must declare the PAT scope it requires,
// because the /mcp route can only enforce the read floor. Tool handlers read
// request identity (role, user id, project pin) off the context via
// internal/server/middleware accessors — the SDK threads the originating
// *http.Request's context through to every tool call, so the same
// Auth/RequireScope/RequireToken chain that gates the route gates each tool
// invocation too.
package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/quota"
	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/version"
)

// Deps is everything the tools need. A struct rather than positional
// arguments because each mutating tool brings its own service, and a
// positional list of same-shaped pointers is how two get swapped.
type Deps struct {
	Queries  *generated.Queries
	Runtimes runtime.Runtimes
	Audit    Auditor
	Deploys  *service.DeployQueue
	Apps     *service.ApplicationService
	Env      *service.EnvVarService
	// Quotas gates create_application on the same limit the REST endpoint
	// applies, so MCP cannot be used to get around a project's application
	// quota. nil disables the check, matching the handler.
	Quotas *quota.Service
	// Databases is the narrow slice of database provisioning create_database
	// needs — satisfied by *handler.Handler. An interface rather than a
	// service call because createDatabaseRecord's 156 lines of credential and
	// provisioning code have two existing callers and one of them
	// (InstantiateTemplate) has no integration test; injecting leaves that
	// body untouched. nil omits the tool.
	Databases Provisioner
	// DBs is needed only by set_database_resources, which applies a limit to
	// the running container as well as persisting it — the live half lives in
	// the service so REST and MCP cannot drift on it.
	DBs *service.DatabaseService
	// Alerts fires the quota threshold warning after create_application, the
	// half of the quota story Quotas above does not cover: Quotas REFUSES at
	// the limit, Alerts warns on the way to it. Separate from Databases
	// because this tool is registered whether or not provisioning is wired.
	// nil skips the warning, which is what the handler's own nil checks do.
	Alerts QuotaAlerter
}

// QuotaAlerter sends the approaching-the-limit warning for a project.
// Satisfied by *handler.Handler, which owns both the quota service and the
// asynq client the email goes through; an interface because this package must
// not import internal/handler.
type QuotaAlerter interface {
	MaybeEnqueueQuotaAlert(ctx context.Context, projectID, ownerUserID pgtype.UUID)
}

// New builds the MCP server and wraps it in a stateless streamable-HTTP
// handler. Stateless + JSONResponse: every tool here is a plain
// request/response with nothing server-initiated, so there is no SSE stream,
// no session store, and nothing lost on a control-plane restart — each
// request stands alone, authenticated by its own Bearer token exactly like
// REST.
func New(d Deps) http.Handler {
	srv := newServer(d)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

// newServer is split out of New so a test can enumerate the REAL registered
// tools rather than a hand-written list.
// Provisioner is the narrow slice of provisioning the create tools need —
// the two paths too coupled to internal/handler to call as services. Both are
// implemented by *handler.Handler; see ProvisionDatabase and ProvisionDomain
// there for why each stayed put.
type Provisioner interface {
	ProvisionDatabase(ctx context.Context, project generated.Project, in service.NewDatabase) (generated.Database, error)
	ProvisionDomain(ctx context.Context, applicationID pgtype.UUID, hostname string) (generated.Domain, error)
	// DomainConflict and DomainTLSDisagreement distinguish the two refusals a
	// domain write can produce from a real failure, without exporting the
	// sentinel errors.
	DomainConflict(err error) bool
	DomainTLSDisagreement(err error) bool
}

func newServer(d Deps) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "belune",
		Version: version.Version,
	}, nil)

	srv.AddReceivingMiddleware(requireToolScope)

	registerProjectTools(srv, d.Queries)
	registerApplicationTools(srv, d.Queries)
	registerDatabaseTools(srv, d.Queries)
	registerDeploymentTools(srv, d.Queries)
	registerLogTools(srv, d.Queries, d.Runtimes)
	registerDomainTools(srv, d.Queries)
	registerBackupTools(srv, d.Queries)
	registerDeployActionTools(srv, d.Queries, d.Deploys, d.Audit)
	registerApplicationActionTools(srv, d)
	registerApplicationLifecycleTools(srv, d)
	registerEnvVarTools(srv, d)
	registerApplicationSettingsTools(srv, d)
	registerDatabaseSettingsTools(srv, d)
	registerCreateTools(srv, d)
	registerCreateDatabaseTool(srv, d)
	registerCreateDomainTool(srv, d)

	return srv
}
