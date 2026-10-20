package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/quota"
	"github.com/belune-hq/belune/internal/service"
)

type createApplicationInput struct {
	ProjectID string `json:"project_id" jsonschema:"the project to create the application in"`
	Name      string `json:"name" jsonschema:"display name; must not already exist in the project"`
	Type      string `json:"type" jsonschema:"'git' to build from a repository, or 'image' to run a prebuilt image"`
	BuildType string `json:"build_type" jsonschema:"for git: dockerfile, buildpacks or railpack. for image: 'image'"`

	SourceRepo     string `json:"source_repo,omitempty" jsonschema:"git only: the repository URL"`
	SourceImage    string `json:"source_image,omitempty" jsonschema:"image only: the container image reference"`
	Branch         string `json:"branch,omitempty" jsonschema:"git only: the ref to build; omit for the repository's default"`
	RootDirectory  string `json:"root_directory,omitempty" jsonschema:"git only: subdirectory to build from; omit for the repository root"`
	DockerfilePath string `json:"dockerfile_path,omitempty" jsonschema:"git only: path to the Dockerfile when build_type is dockerfile"`
}

type createdApplication struct {
	Application application `json:"application"`
	Note        string      `json:"note"`
}

// ⚠️ No git_integration_id, no git_token, and no cpu/memory on create.
//
// A git integration and a PAT are both CREDENTIALS. The REST endpoint accepts
// them because a person is choosing which of their own connected accounts to
// attach; letting a tool pass one would have an assistant deciding which
// credential an application uses, which is not a decision a tool call should
// make without a confirmation step. A private repository is therefore set up
// in the dashboard, and this tool is for the public and image cases.
//
// Resource limits are left to set_application_resources so there is one way to
// set them, and so the create call cannot silently pin an application to
// limits an assistant invented — the platform defaults (unlimited) apply until
// someone chooses otherwise.
func registerCreateTools(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_application",
		Description: "Create a new application in a project. It is created but NOT deployed — call " +
			"trigger_deployment afterwards to build and start it. " +
			"For a prebuilt image: type='image', build_type='image', and source_image. " +
			"For a git repository: type='git', a build_type of dockerfile, buildpacks or railpack, and " +
			"source_repo. Only PUBLIC repositories work here: attaching a connected git account is done " +
			"in the dashboard, because it decides which credential the application uses. " +
			"Resource limits are not set here — use set_application_resources after. " +
			"⚠️ Creating an application with a name that already exists in the project is REFUSED rather " +
			"than returning the existing one, so if a call fails check with list_applications before " +
			"retrying: a create that succeeded but whose reply was lost will report a conflict on retry.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createApplicationInput) (*mcp.CallToolResult, any, error) {
		projectID, err := parseUUID(in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		// Pin + ownership against the project id the caller named. Unlike the
		// nested tools there is nothing to resolve from a row — the pin IS the
		// ceiling here, so a project outside it is refused outright.
		project, err := authorizeProject(ctx, d.Queries, projectID)
		if err != nil {
			return nil, nil, err
		}

		name := strings.TrimSpace(in.Name)
		if name == "" || in.Type == "" || in.BuildType == "" {
			return nil, nil, errors.New("name, type and build_type are required")
		}

		// ⚠️ Error on duplicate, and name the conflict. A slug carries the
		// application's own id, so the database happily accepts two
		// applications with the same name — nothing below this would catch a
		// retry. The decision not to return the existing one is deliberate:
		// returning it leaves the caller unable to tell "I created this" from
		// "this already existed", so it would report success for work it did
		// not do. The cost is the mirror case, a successful create whose reply
		// was lost, which is why the message says what to do about it.
		existing, err := d.Queries.ListApplicationsByProject(ctx, projectID)
		if err != nil {
			return nil, nil, internalError("failed to check for an existing application", err)
		}
		for _, app := range existing {
			if strings.EqualFold(strings.TrimSpace(app.Name), name) {
				return nil, nil, fmt.Errorf(
					"an application named %q already exists in project %q (id %s) — "+
						"nothing was created. If you expected to create it, read it with get_application; "+
						"if you meant to change it, use update_application",
					app.Name, project.Name, uuidToString(app.ID))
			}
		}

		if err := service.ValidateSource(service.SourceFields{
			Type:           in.Type,
			BuildType:      in.BuildType,
			DockerfilePath: in.DockerfilePath,
			SourceRepo:     in.SourceRepo,
			SourceImage:    in.SourceImage,
		}); err != nil {
			return nil, nil, err
		}
		if !service.ValidBranchName(in.Branch) {
			return nil, nil, errors.New("invalid branch name")
		}
		if !service.ValidRootDirectory(in.RootDirectory) {
			return nil, nil, errors.New("invalid root directory")
		}

		// The same quota gate the REST endpoint applies. Without it MCP would
		// be a way around a project's application limit, which is exactly the
		// kind of divergence that turns a convenience into a finding.
		if d.Quotas != nil {
			if err := d.Quotas.CheckApplicationCreate(ctx, projectID, project.UserID, 0, 0); err != nil {
				var qe *quota.ExceededError
				if errors.As(err, &qe) {
					return nil, nil, errors.New(qe.Error())
				}
				return nil, nil, internalError("quota check failed", err)
			}
		}

		app, err := d.Apps.Create(ctx, service.CreateApplicationParams{
			ProjectID:      projectID,
			ProjectSlug:    project.Slug,
			Name:           name,
			BaseSlug:       naming.Slugify(name),
			Type:           in.Type,
			BuildType:      in.BuildType,
			SourceRepo:     in.SourceRepo,
			SourceImage:    in.SourceImage,
			DockerfilePath: in.DockerfilePath,
			Branch:         in.Branch,
			RootDirectory:  in.RootDirectory,
		})
		if err != nil {
			return nil, nil, internalError("failed to create the application", err)
		}

		auditTool(ctx, d.Audit, "create_application", "application", uuidToString(app.ID),
			map[string]any{"name": name, "project_id": in.ProjectID})

		// The warning half of the same quota check applied above. Without it
		// an owner creating applications through MCP crossed their threshold
		// in silence and only learnt of the limit by being refused at it,
		// while the REST path had warned them. Fire-and-forget by design: the
		// application exists either way.
		if d.Alerts != nil {
			d.Alerts.MaybeEnqueueQuotaAlert(ctx, projectID, project.UserID)
		}

		return textResult(createdApplication{
			Application: toApplication(app),
			Note:        "created but not deployed — call trigger_deployment to build and start it",
		})
	})
}

// managedDatabaseTypes mirrors the handler's defaultVersions keys. Duplicated
// rather than exported from there because this package must not import
// internal/handler (handler imports this one to wire the /mcp route), and the
// scope test plus create_database's own test pin the set either way.
var managedDatabaseTypes = map[string]bool{
	"postgres": true, "mysql": true, "redis": true, "mongo": true,
}

type createDatabaseInput struct {
	ProjectID string `json:"project_id" jsonschema:"the project to create the database in"`
	Name      string `json:"name" jsonschema:"display name; must not already exist in the project"`
	Type      string `json:"type" jsonschema:"one of postgres, mysql, redis, mongo"`
	Version   string `json:"version,omitempty" jsonschema:"engine version; omit for the engine's default"`
}

type createdDatabase struct {
	Database database `json:"database"`
	Note     string   `json:"note"`
}

func registerCreateDatabaseTool(srv *mcp.Server, d Deps) {
	if d.Databases == nil {
		// Nothing can provision, so registering the tool would advertise a
		// capability that always fails. Omitting it is the honest answer, and
		// the scope table's exact-match test is why this cannot silently drift
		// — a tool absent from the server is absent from the table too.
		return
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_database",
		Description: "Create a managed database in a project: postgres, mysql, redis or mongo. " +
			"Credentials are generated and stored encrypted; they are never returned by this or any " +
			"other tool — read them from the dashboard. Provisioning is asynchronous, so the database " +
			"comes back as 'creating' and becomes 'running' shortly after; poll get_database. " +
			"Omit version for the engine's default. " +
			"⚠️ Creating a database with a name that already exists in the project is REFUSED rather " +
			"than returning the existing one, so if a call fails check with list_databases before " +
			"retrying: a create that succeeded but whose reply was lost will report a conflict on retry. " +
			"An arbitrary container image run as a database ('other') is not available here — that is " +
			"assembled from an image's documentation in the dashboard.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createDatabaseInput) (*mcp.CallToolResult, any, error) {
		projectID, err := parseUUID(in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		project, err := authorizeProject(ctx, d.Queries, projectID)
		if err != nil {
			return nil, nil, err
		}

		name := strings.TrimSpace(in.Name)
		if name == "" || in.Type == "" {
			return nil, nil, errors.New("name and type are required")
		}
		// "other" is excluded here rather than rejected by the type check, so
		// the message says why instead of listing it as merely invalid.
		if in.Type == "other" {
			return nil, nil, errors.New(
				"type 'other' — an arbitrary image run as a database — is not available through MCP; " +
					"create it in the dashboard, where its image, port, data directory and backup commands are set together")
		}
		if _, ok := managedDatabaseTypes[in.Type]; !ok {
			return nil, nil, errors.New("type must be one of: postgres, mysql, redis, mongo")
		}

		// Error on duplicate, naming the conflict — same reasoning as
		// create_application above.
		existing, err := d.Queries.ListDatabasesByProject(ctx, projectID)
		if err != nil {
			return nil, nil, internalError("failed to check for an existing database", err)
		}
		for _, db := range existing {
			if strings.EqualFold(strings.TrimSpace(db.Name), name) {
				return nil, nil, fmt.Errorf(
					"a database named %q already exists in project %q (id %s) — "+
						"nothing was created. If you expected to create it, read it with get_database",
					db.Name, project.Name, uuidToString(db.ID))
			}
		}

		db, err := d.Databases.ProvisionDatabase(ctx, project, service.NewDatabase{
			Name:    name,
			Type:    in.Type,
			Version: in.Version,
		})
		if err != nil {
			return nil, nil, internalError("failed to create the database", err)
		}

		auditTool(ctx, d.Audit, "create_database", "database", uuidToString(db.ID),
			map[string]any{"name": name, "type": in.Type})

		return textResult(createdDatabase{
			Database: toDatabase(db),
			Note:     "provisioning started — poll get_database until its status is running",
		})
	})
}

type createDomainInput struct {
	ApplicationID string `json:"application_id" jsonschema:"the application to serve this hostname"`
	Hostname      string `json:"hostname" jsonschema:"the domain name, e.g. app.example.com"`
}

// createdDomainRow is the shape a newly added domain is reported in. Narrow
// on purpose: enough to confirm what was made and that TLS is off, without
// echoing routing internals the tool cannot set anyway.
type createdDomainRow struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Hostname      string `json:"hostname"`
	SSLMode       string `json:"ssl_mode"`
	Path          string `json:"path"`
}

type createdDomain struct {
	Domain createdDomainRow `json:"domain"`
	Note   string           `json:"note"`
}

func registerCreateDomainTool(srv *mcp.Server, d Deps) {
	if d.Databases == nil {
		return
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "create_domain",
		Description: "Point a hostname at an application. The route is created immediately and the " +
			"domain is stored with TLS off, so Belune does not serve it over HTTPS. " +
			"⚠️ HTTPS is NOT configured and cannot be configured by this tool — turn it on in the " +
			"dashboard once the hostname's DNS points at this server. Do it in that order: a " +
			"certificate requested for a hostname that does not resolve here yet spends that " +
			"hostname's limited validation attempts, which delays the certificate that would " +
			"otherwise have worked. " +
			"Check with list_domain_tls_status, which reports why a certificate is pending or failed. " +
			"The hostname must already be a valid domain name; DNS is not checked, so a typo produces a " +
			"route that serves nothing. There is no tool to remove or edit a domain — a wrong one is " +
			"cleaned up in the dashboard, so get it right rather than retrying.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createDomainInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		// Resolve the application by its own id, then authorize against the
		// project that row actually belongs to.
		app, err := d.Queries.GetApplication(ctx, id)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "application not found")
		}
		if err := authorizeApplication(ctx, d.Queries, app.ID, app.ProjectID); err != nil {
			return nil, nil, err
		}

		hostname := strings.ToLower(strings.TrimSpace(in.Hostname))
		if hostname == "" {
			return nil, nil, errors.New("hostname is required")
		}

		dom, err := d.Databases.ProvisionDomain(ctx, app.ID, hostname)
		if err != nil {
			// A duplicate hostname is the caller's mistake to fix, and it is
			// reported the same way a duplicate name is on the other create
			// tools: say what collided rather than returning a bare failure.
			if d.Databases.DomainConflict(err) {
				return nil, nil, fmt.Errorf("%s is already configured — nothing was created. "+
					"Read the application's domains with get_application", hostname)
			}
			// Every path on one hostname shares a certificate, so a hostname
			// already served over TLS cannot gain an HTTP-only row beside it.
			// The reason reads well as a tool error already.
			if d.Databases.DomainTLSDisagreement(err) {
				return nil, nil, fmt.Errorf("%s — nothing was created. %s is already configured with TLS on "+
					"another path, and every path on one hostname shares a certificate, so this tool cannot "+
					"add it. Add it in the dashboard with a matching TLS mode",
					strings.TrimPrefix(err.Error(), "tls mode disagreement: "), hostname)
			}
			if strings.Contains(err.Error(), "invalid hostname") {
				return nil, nil, errors.New("invalid hostname format — expected a domain name like app.example.com")
			}
			return nil, nil, internalError("failed to add the domain", err)
		}

		auditTool(ctx, d.Audit, "add_domain", "domain", uuidToString(dom.ID),
			map[string]any{"hostname": hostname})

		return textResult(createdDomain{
			Domain: createdDomainRow{
				ID:            uuidToString(dom.ID),
				ApplicationID: uuidToString(dom.ApplicationID),
				Hostname:      dom.Hostname,
				SSLMode:       dom.SslMode,
				Path:          dom.Path,
			},
			Note: "serving over HTTP — enable HTTPS in the dashboard once DNS for this hostname " +
				"points at this server",
		})
	})
}
