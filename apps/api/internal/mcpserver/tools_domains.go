package mcpserver

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	defaultDomainsLimit = 50
	maxDomainsLimit     = 200
)

// domainTLSStatus is the tool-facing shape of one row of the central TLS
// view: what the server last observed for a domain, not what its
// configuration claims should happen. Mirrors handler.domainTLSStatus.
type domainTLSStatus struct {
	ID               string `json:"id"`
	Hostname         string `json:"hostname"`
	SSLMode          string `json:"ssl_mode"`
	TLSStatus        string `json:"tls_status"`
	TLSIssuer        string `json:"tls_issuer,omitempty"`
	TLSNotAfter      string `json:"tls_not_after,omitempty"`
	TLSLastCheckedAt string `json:"tls_last_checked_at,omitempty"`
	// TLSError is authoritative and decides the status. TLSAdvisory is a
	// suspicion, not a verdict — it explains a domain that is still pending
	// without asserting that anything is wrong.
	TLSError        string `json:"tls_error,omitempty"`
	TLSAdvisory     string `json:"tls_advisory,omitempty"`
	CertificateName string `json:"certificate_name,omitempty"`
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	ProjectID       string `json:"project_id"`
}

type listDomainTLSStatusInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of domains to return, alphabetical by hostname (default 50, max 200)"`
}

// registerDomainTools registers the differentiator tool: unlike a status
// check that can only report "unknown", this surfaces the SAME failure
// reason (TLSError) and suspicion (TLSAdvisory) the dashboard's own TLS page
// shows an operator.
func registerDomainTools(srv *mcp.Server, queries *generated.Queries) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_domain_tls_status",
		Description: "List every domain's observed TLS state, with the certificate it serves and " +
			"why a pending or failed certificate hasn't issued. An admin sees every domain on the " +
			"install; a member sees only domains in their own projects and any shared with them. " +
			"Bounded — defaults to 50, capped at 200.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listDomainTLSStatusInput) (*mcp.CallToolResult, any, error) {
		// Same scope + pin construction as handler.ListDomainTLSStatus: a NULL
		// user_id asks the query for every domain (what an admin gets), and
		// this route has no project_id argument for a per-tool pin check to
		// compare against, so the pin is passed straight into the query
		// instead — see ListDomainsWithTLSStatusLimit's own NULL-means-
		// unfiltered handling.
		var scope pgtype.UUID
		if middleware.RoleFromContext(ctx) != "admin" {
			if err := scope.Scan(middleware.UserIDFromContext(ctx)); err != nil {
				return nil, nil, internalError("failed to list domain TLS status", err)
			}
		}

		pinnedIDs, err := pinnedProjectUUIDs(ctx)
		if err != nil {
			return nil, nil, internalError("failed to list domain TLS status", err)
		}

		limit := clampLimit(in.Limit, defaultDomainsLimit, maxDomainsLimit)
		rows, err := queries.ListDomainsWithTLSStatusLimit(ctx, generated.ListDomainsWithTLSStatusLimitParams{
			UserID:     scope,
			ProjectIds: pinnedIDs,
			RowLimit:   int32(limit),
		})
		if err != nil {
			return nil, nil, internalError("failed to list domain TLS status", err)
		}

		out := make([]domainTLSStatus, 0, len(rows))
		for _, row := range rows {
			out = append(out, domainTLSStatus{
				ID:               uuidToString(row.ID),
				Hostname:         row.Hostname,
				SSLMode:          row.SslMode,
				TLSStatus:        row.TlsStatus,
				TLSIssuer:        row.TlsIssuer.String,
				TLSNotAfter:      formatTimestamp(row.TlsNotAfter),
				TLSLastCheckedAt: formatTimestamp(row.TlsLastCheckedAt),
				TLSError:         row.TlsError.String,
				TLSAdvisory:      row.TlsAdvisory.String,
				CertificateName:  row.CertificateName.String,
				ApplicationID:    uuidToString(row.ApplicationID),
				ApplicationName:  row.ApplicationName,
				ProjectID:        uuidToString(row.ProjectID),
			})
		}
		return textResult(out)
	})
}
