package handler

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

// applicationResponse is the wire shape of an application.
//
// It exists because handlers used to return the generated row verbatim, which
// shipped four secret columns to the browser on every fetch: the webhook
// secret in plaintext, the encrypted git credentials, and both deploy-hook
// token columns. None of them are needed to render anything — the UI only
// needs to know *whether* each is set — so they are replaced by booleans and
// the values themselves are reachable only through audited reveal endpoints.
//
// Fields are listed explicitly rather than embedded: embedding would carry any
// column added later straight to the client, which is how the leak happened in
// the first place.
type applicationResponse struct {
	ID                        pgtype.UUID        `json:"id"`
	ProjectID                 pgtype.UUID        `json:"project_id"`
	Name                      string             `json:"name"`
	Slug                      string             `json:"slug"`
	Type                      string             `json:"type"`
	SourceRepo                pgtype.Text        `json:"source_repo"`
	SourceImage               pgtype.Text        `json:"source_image"`
	DockerfilePath            pgtype.Text        `json:"dockerfile_path"`
	RootDirectory             pgtype.Text        `json:"root_directory"`
	BuildType                 string             `json:"build_type"`
	BuildTypeOverride         pgtype.Text        `json:"build_type_override"`
	BuilderImage              pgtype.Text        `json:"builder_image"`
	CustomBuildpacks          []byte             `json:"custom_buildpacks"`
	CpuLimit                  float64            `json:"cpu_limit"`
	MemoryLimit               int64              `json:"memory_limit"`
	AutoDeployBranch          pgtype.Text        `json:"auto_deploy_branch"`
	Status                    string             `json:"status"`
	HealthCheckPath           pgtype.Text        `json:"health_check_path"`
	CreatedAt                 pgtype.Timestamptz `json:"created_at"`
	UpdatedAt                 pgtype.Timestamptz `json:"updated_at"`
	ParentApplicationID       pgtype.UUID        `json:"parent_application_id"`
	Branch                    pgtype.Text        `json:"branch"`
	PreviewBranchPattern      pgtype.Text        `json:"preview_branch_pattern"`
	PreviewDomainTemplate     pgtype.Text        `json:"preview_domain_template"`
	LastActivityAt            pgtype.Timestamptz `json:"last_activity_at"`
	GitIntegrationID          pgtype.UUID        `json:"git_integration_id"`
	SourceKind                pgtype.Text        `json:"source_kind"`
	SourceRef                 pgtype.Text        `json:"source_ref"`
	ReadonlyRootfs            bool               `json:"readonly_rootfs"`
	ContainerCaps             string             `json:"container_caps"`
	ContainerPort             pgtype.Int4        `json:"container_port"`
	HealthCheckTimeoutSeconds pgtype.Int4        `json:"health_check_timeout_seconds"`
	HealthCheckExpectStatus   pgtype.Int4        `json:"health_check_expect_status"`

	HealthCheckType               string      `json:"health_check_type"`
	HealthCheckCommand            pgtype.Text `json:"health_check_command"`
	HealthCheckIntervalSeconds    pgtype.Int4 `json:"health_check_interval_seconds"`
	HealthCheckRetries            pgtype.Int4 `json:"health_check_retries"`
	HealthCheckStartPeriodSeconds pgtype.Int4 `json:"health_check_start_period_seconds"`

	// Presence flags standing in for the secrets themselves.
	HasWebhookSecret  bool `json:"has_webhook_secret"`
	HasGitCredentials bool `json:"has_git_credentials"`
	DeployHookEnabled bool `json:"deploy_hook_enabled"`

	LastDeployedAt pgtype.Timestamptz `json:"last_deployed_at"`

	// PendingChange is derived rather than raw so every caller agrees on the
	// answer: "source", "config", or "" for nothing pending. Deriving it here
	// also keeps the application list correct without a per-row deployment
	// query, which a client-side rule would have needed.
	PendingChange   string             `json:"pending_change"`
	ConfigChangedAt pgtype.Timestamptz `json:"config_changed_at"`
	SourceChangedAt pgtype.Timestamptz `json:"source_changed_at"`
}

// toApplicationResponse masks an application row for the wire.
func toApplicationResponse(a generated.Application) applicationResponse {
	return applicationResponse{
		ID:                        a.ID,
		ProjectID:                 a.ProjectID,
		Name:                      a.Name,
		Slug:                      a.Slug,
		Type:                      a.Type,
		SourceRepo:                a.SourceRepo,
		SourceImage:               a.SourceImage,
		DockerfilePath:            a.DockerfilePath,
		RootDirectory:             a.RootDirectory,
		BuildType:                 a.BuildType,
		BuildTypeOverride:         a.BuildTypeOverride,
		BuilderImage:              a.BuilderImage,
		CustomBuildpacks:          a.CustomBuildpacks,
		CpuLimit:                  a.CpuLimit,
		MemoryLimit:               a.MemoryLimit,
		AutoDeployBranch:          a.AutoDeployBranch,
		Status:                    a.Status,
		HealthCheckPath:           a.HealthCheckPath,
		CreatedAt:                 a.CreatedAt,
		UpdatedAt:                 a.UpdatedAt,
		ParentApplicationID:       a.ParentApplicationID,
		Branch:                    a.Branch,
		PreviewBranchPattern:      a.PreviewBranchPattern,
		PreviewDomainTemplate:     a.PreviewDomainTemplate,
		LastActivityAt:            a.LastActivityAt,
		GitIntegrationID:          a.GitIntegrationID,
		SourceKind:                a.SourceKind,
		SourceRef:                 a.SourceRef,
		ReadonlyRootfs:            a.ReadonlyRootfs,
		ContainerCaps:             a.ContainerCaps,
		ContainerPort:             a.ContainerPort,
		HealthCheckTimeoutSeconds: a.HealthCheckTimeoutSeconds,
		HealthCheckExpectStatus:   a.HealthCheckExpectStatus,

		HealthCheckType:               a.HealthCheckType,
		HealthCheckCommand:            a.HealthCheckCommand,
		HealthCheckIntervalSeconds:    a.HealthCheckIntervalSeconds,
		HealthCheckRetries:            a.HealthCheckRetries,
		HealthCheckStartPeriodSeconds: a.HealthCheckStartPeriodSeconds,

		// Either column counts — a row may not be backfilled yet.
		HasWebhookSecret:  len(a.WebhookSecretEncrypted) > 0 || (a.WebhookSecret.Valid && a.WebhookSecret.String != ""),
		HasGitCredentials: len(a.GitCredentialsEncrypted) > 0,
		DeployHookEnabled: len(a.DeployHookTokenHash) > 0,

		LastDeployedAt:  a.LastDeployedAt,
		PendingChange:   service.ApplicationPendingChange(a),
		ConfigChangedAt: a.ConfigChangedAt,
		SourceChangedAt: a.SourceChangedAt,
	}
}

// toApplicationResponses maps a list of rows for the wire.
func toApplicationResponses(apps []generated.Application) []applicationResponse {
	out := make([]applicationResponse, 0, len(apps))
	for _, a := range apps {
		out = append(out, toApplicationResponse(a))
	}
	return out
}
