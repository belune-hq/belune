package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

type ApplicationService struct {
	db            *pgxpool.Pool
	queries       *generated.Queries
	runtimes      runtime.Runtimes
	keyring       *crypto.Keyring
	fileMountsDir string
	// destinations resolves where a volume backup was written, so deleting an
	// application can erase its archives instead of orphaning them. Optional:
	// nil means remote objects are left alone, which is the pre-existing
	// behaviour and is logged rather than silently skipped.
	destinations *BackupDestinationService
}

func NewApplicationService(db *pgxpool.Pool, queries *generated.Queries, rts runtime.Runtimes, keyring *crypto.Keyring, fileMountsDir string, destinations *BackupDestinationService) *ApplicationService {
	return &ApplicationService{
		db: db, queries: queries, runtimes: rts, keyring: keyring,
		fileMountsDir: fileMountsDir, destinations: destinations,
	}
}

// cleanupVolumeBackups erases the archives taken of an application's volumes.
//
// ⚠️ Read BEFORE the application row is deleted. application_volume_backups
// cascades away with the volumes, so afterwards the remote objects exist with
// no row anywhere recording their keys: unreachable, unprunable, and still
// billed. That is the exact mirror of the database bug this release fixes —
// there the rows cascaded and the objects were destroyed; here they cascaded
// and the objects were left behind.
//
// Best-effort, like the database path: a storage error must not leave an
// application undeletable.
func (s *ApplicationService) cleanupVolumeBackups(ctx context.Context, appID pgtype.UUID) {
	runs, err := s.queries.ListVolumeBackupsForApplication(ctx, appID)
	if err != nil {
		slog.Warn("could not list volume backups during app deletion",
			"application_id", uuidToString(appID), "error", err)
		return
	}
	if len(runs) == 0 {
		return
	}

	for _, run := range runs {
		if run.LocalPath.Valid {
			if err := os.Remove(run.LocalPath.String); err != nil && !os.IsNotExist(err) {
				slog.Warn("could not remove volume backup file during app deletion",
					"path", run.LocalPath.String, "error", err)
			}
		}
	}

	if s.destinations == nil {
		// Nothing can resolve which bucket these went to. Loud, because the
		// objects survive and someone has to know they are still being paid for.
		slog.Warn("volume backups have remote copies but no destination service is wired; their objects are left behind",
			"application_id", uuidToString(appID), "backups", len(runs))
		return
	}

	// The per-run recorded location decides the bucket. No fallback config id:
	// unlike a config teardown there is no single config here — these runs may
	// come from several, and guessing one would delete from the wrong bucket.
	if err := s.destinations.PurgeVolumeBackupObjects(ctx, runs, pgtype.UUID{}); err != nil {
		slog.Warn("could not remove every volume backup object during app deletion",
			"application_id", uuidToString(appID), "error", err)
	}
}

// CreateApplicationParams holds the parameters for creating an application.
type CreateApplicationParams struct {
	ProjectID        pgtype.UUID
	ProjectSlug      string
	Name             string
	BaseSlug         string
	Type             string
	SourceRepo       string
	SourceImage      string
	DockerfilePath   string
	BuildType        string
	CPULimit         float64
	MemoryLimit      int64
	GitToken         string      // plaintext PAT; encrypted before storage
	HealthCheckPath  string      // HTTP path to poll after deploy (e.g. /healthz)
	GitIntegrationID pgtype.UUID // optional FK to a connected provider account
	Branch           string      // ref to build; empty = the repository's default ref
	RootDirectory    string      // subdirectory to build from; empty = the repo root
}

// Create inserts the application record and sets its final slug atomically.
// A webhook secret is auto-generated. If GitToken is set, it is encrypted before storage.
func (s *ApplicationService) Create(ctx context.Context, p CreateApplicationParams) (generated.Application, error) {
	webhookSecret, err := crypto.GenerateWebhookSecret()
	if err != nil {
		return generated.Application{}, fmt.Errorf("generate webhook secret: %w", err)
	}
	encryptedSecret, err := s.keyring.Encrypt([]byte(webhookSecret))
	if err != nil {
		return generated.Application{}, fmt.Errorf("encrypt webhook secret: %w", err)
	}

	var gitCreds []byte
	if p.GitToken != "" {
		encrypted, err := s.keyring.Encrypt([]byte(p.GitToken))
		if err != nil {
			return generated.Application{}, fmt.Errorf("encrypt git token: %w", err)
		}
		gitCreds = encrypted
	}

	var app generated.Application
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var err error
		app, err = q.CreateApplication(ctx, generated.CreateApplicationParams{
			ProjectID:               p.ProjectID,
			Name:                    p.Name,
			Slug:                    p.BaseSlug,
			Type:                    p.Type,
			SourceRepo:              pgtype.Text{String: p.SourceRepo, Valid: p.SourceRepo != ""},
			SourceImage:             pgtype.Text{String: p.SourceImage, Valid: p.SourceImage != ""},
			DockerfilePath:          pgtype.Text{String: p.DockerfilePath, Valid: p.DockerfilePath != ""},
			BuildType:               p.BuildType,
			CpuLimit:                p.CPULimit,
			MemoryLimit:             p.MemoryLimit,
			WebhookSecretEncrypted:  encryptedSecret,
			GitCredentialsEncrypted: gitCreds,
			HealthCheckPath:         pgtype.Text{String: p.HealthCheckPath, Valid: p.HealthCheckPath != ""},
			GitIntegrationID:        p.GitIntegrationID,
			Branch:                  branchValue(p.Branch),
			AutoDeployBranch:        branchValue(p.Branch),
			RootDirectory:           pgtype.Text{String: p.RootDirectory, Valid: p.RootDirectory != ""},
		})
		if err != nil {
			return err
		}
		appIDStr := uuidToString(app.ID)
		finalSlug := fmt.Sprintf("%s-%s-%s", p.ProjectSlug, p.BaseSlug, appIDStr[:8])
		if err := q.UpdateApplicationSlug(ctx, generated.UpdateApplicationSlugParams{
			ID:   app.ID,
			Slug: finalSlug,
		}); err != nil {
			return err
		}
		app.Slug = finalSlug
		return nil
	})
	return app, err
}

// UpdateApplicationParams holds parameters for updating an application.
type UpdateApplicationParams struct {
	Name              string
	SourceRepo        string
	SourceImage       string
	DockerfilePath    string
	BuildTypeOverride string
	BuilderImage      string
	CPULimit          float64
	MemoryLimit       int64
	GitToken          string      // plaintext PAT; encrypted before storage; empty = no change
	HealthCheckPath   string      // empty = clear existing
	GitIntegrationID  pgtype.UUID // optional FK to a connected provider account; zero = clear
	Branch            string      // ref to build; empty = the repository's default ref
	RootDirectory     string      // subdirectory to build from; empty = the repo root
}

// Update applies field changes to an application.
// If GitToken is non-empty, it replaces the stored credentials; if empty, existing credentials are preserved.
func (s *ApplicationService) Update(
	ctx context.Context,
	appID pgtype.UUID,
	current generated.Application,
	p UpdateApplicationParams,
) (generated.Application, error) {
	name := p.Name
	if name == "" {
		name = current.Name
	}

	// A preview child's branch is its identity — it was materialised for that
	// branch and its domain is derived from it. Never let a general update
	// retarget one.
	branch, autoDeployBranch := branchValue(p.Branch), branchValue(p.Branch)
	if current.ParentApplicationID.Valid {
		branch, autoDeployBranch = current.Branch, current.AutoDeployBranch
	}

	gitCreds := current.GitCredentialsEncrypted
	if p.GitToken != "" {
		encrypted, err := s.keyring.Encrypt([]byte(p.GitToken))
		if err != nil {
			return generated.Application{}, fmt.Errorf("encrypt git token: %w", err)
		}
		gitCreds = encrypted
	}

	return s.queries.UpdateApplication(ctx, generated.UpdateApplicationParams{
		ID:                appID,
		Name:              name,
		SourceRepo:        pgtype.Text{String: p.SourceRepo, Valid: p.SourceRepo != ""},
		SourceImage:       pgtype.Text{String: p.SourceImage, Valid: p.SourceImage != ""},
		DockerfilePath:    pgtype.Text{String: p.DockerfilePath, Valid: p.DockerfilePath != ""},
		BuildTypeOverride: pgtype.Text{String: p.BuildTypeOverride, Valid: p.BuildTypeOverride != ""},
		BuilderImage:      pgtype.Text{String: p.BuilderImage, Valid: p.BuilderImage != ""},
		CustomBuildpacks:  current.CustomBuildpacks,
		Status:            current.Status,
		// Resource limits are owned by SetApplicationResources, not this general
		// update — preserve them so saving settings can never reset them to
		// unlimited out from under the Resources section.
		CpuLimit:                current.CpuLimit,
		MemoryLimit:             current.MemoryLimit,
		GitCredentialsEncrypted: gitCreds,
		// Health configuration is owned by SetApplicationHealthCheck, not this
		// general update — preserve it so saving settings can never clear the
		// path out from under the Health Check section.
		HealthCheckPath:  current.HealthCheckPath,
		GitIntegrationID: p.GitIntegrationID,
		Branch:           branch,
		AutoDeployBranch: autoDeployBranch,
		RootDirectory:    pgtype.Text{String: p.RootDirectory, Valid: p.RootDirectory != ""},
	})
}

// branchValue maps the user-facing Branch field onto the column. Empty means
// "the repository's default ref", which is stored as NULL — the state every
// application was in before branch selection existed, so leaving it blank
// preserves the previous behaviour exactly.
func branchValue(branch string) pgtype.Text {
	return pgtype.Text{String: branch, Valid: branch != ""}
}

// Delete stops and removes the application container, then deletes the DB record.
// The DB delete cascades deployments, env vars, and domains via FK ON DELETE CASCADE.
func (s *ApplicationService) Delete(ctx context.Context, appID pgtype.UUID, projectSlug, appSlug string) error {
	appIDStr := uuidToString(appID)

	// Resolved before anything is removed: the lookup joins projects, so it has
	// to happen while both rows still exist.
	rt, err := RuntimeForApplication(ctx, s.queries, s.runtimes, appID)
	if err != nil {
		return err
	}

	containerName := naming.ContainerName(projectSlug, appSlug, appIDStr)
	intermediateContainerName := naming.IntermediateContainerName(projectSlug, appIDStr)
	oldContainerName := naming.OldContainerName(appIDStr)
	for _, name := range []string{containerName, intermediateContainerName, oldContainerName} {
		if err := rt.StopContainer(ctx, name); err != nil {
			slog.Warn("could not stop container during app deletion", "container", name, "error", err)
		}
		if err := rt.RemoveContainer(ctx, name); err != nil {
			slog.Warn("could not remove container during app deletion", "container", name, "error", err)
		}
	}

	// Drop the per-app CNB cache volumes too. They are tagged belune-cache=true
	// so PruneVolumes would not have reclaimed them; leaving them behind after
	// delete would leak disk space permanently.
	for _, vol := range []string{
		naming.CNBCacheVolumeName(appIDStr),
		naming.CNBLaunchCacheVolumeName(appIDStr),
	} {
		if err := rt.RemoveVolume(ctx, vol); err != nil {
			slog.Debug("could not remove cache volume during app deletion (may not exist)", "volume", vol, "error", err)
		}
	}

	// Erase the archives taken of those volumes. Same ordering requirement as
	// the volumes themselves and for a sharper reason: the backup rows cascade
	// away with them, taking the only record of where the objects live.
	s.cleanupVolumeBackups(ctx, appID)

	// Drop persistent data volumes. They are tagged belune-data=true so
	// PruneVolumes never reclaims them; app deletion is the only point they can
	// be cleaned up. List them BEFORE DeleteApplication, whose ON DELETE CASCADE
	// removes the application_volumes rows.
	dataVols, err := s.queries.ListApplicationVolumes(ctx, appID)
	if err != nil {
		slog.Warn("could not list data volumes during app deletion", "application_id", appIDStr, "error", err)
	}
	for _, v := range dataVols {
		volName := naming.AppVolumeName(appIDStr, v.Name)
		if err := rt.RemoveVolume(ctx, volName); err != nil {
			slog.Debug("could not remove data volume during app deletion (may not exist)", "volume", volName, "error", err)
		}
	}

	// Remove materialised file/config mounts (the DB rows cascade with the app;
	// the host files do not). Best-effort — a leftover dir only wastes a little
	// disk. Skip when unconfigured to avoid an accidental broad path.
	if s.fileMountsDir != "" {
		if err := os.RemoveAll(filepath.Join(s.fileMountsDir, appIDStr)); err != nil {
			slog.Warn("could not remove file mounts dir during app deletion", "application_id", appIDStr, "error", err)
		}
	}

	return s.queries.DeleteApplication(ctx, appID)
}

// FindOrCreatePreview returns the preview child for (parent, branch), creating
// it when missing. Children inherit their parent's source config, build type,
// resource limits, and git credential linkage, so the existing deploy worker
// handles them without special casing. The child's slug follows the same
// "{projectSlug}-{baseSlug}-{appID[:8]}" shape as any other application, where
// baseSlug = "{parentBaseSlug}-{branchSlug}".
//
// parentBaseSlug is the pre-finalization base (before the project prefix was
// baked in by Create); callers derive it from the parent row's metadata.
func (s *ApplicationService) FindOrCreatePreview(
	ctx context.Context,
	parent generated.Application,
	projectSlug, parentBaseSlug, branch, branchSlug string,
) (generated.Application, bool, error) {
	existing, err := s.queries.GetPreviewByParentBranch(ctx, generated.GetPreviewByParentBranchParams{
		ParentApplicationID: pgtype.UUID{Bytes: parent.ID.Bytes, Valid: true},
		Branch:              pgtype.Text{String: branch, Valid: true},
	})
	if err == nil {
		return existing, false, nil
	}

	previewBaseSlug := fmt.Sprintf("%s-%s", parentBaseSlug, branchSlug)

	var created generated.Application
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var txErr error
		created, txErr = q.CreatePreviewApplication(ctx, generated.CreatePreviewApplicationParams{
			ProjectID:               parent.ProjectID,
			Name:                    fmt.Sprintf("%s (%s)", parent.Name, branch),
			Slug:                    previewBaseSlug,
			Type:                    parent.Type,
			SourceRepo:              parent.SourceRepo,
			SourceImage:             parent.SourceImage,
			DockerfilePath:          parent.DockerfilePath,
			BuildType:               parent.BuildType,
			CpuLimit:                parent.CpuLimit,
			MemoryLimit:             parent.MemoryLimit,
			GitCredentialsEncrypted: parent.GitCredentialsEncrypted,
			HealthCheckPath:         parent.HealthCheckPath,
			GitIntegrationID:        parent.GitIntegrationID,
			ParentApplicationID:     pgtype.UUID{Bytes: parent.ID.Bytes, Valid: true},
			Branch:                  pgtype.Text{String: branch, Valid: true},
			RootDirectory:           parent.RootDirectory,
		})
		if txErr != nil {
			return txErr
		}
		appIDStr := uuidToString(created.ID)
		finalSlug := fmt.Sprintf("%s-%s-%s", projectSlug, previewBaseSlug, appIDStr[:8])
		if txErr := q.UpdateApplicationSlug(ctx, generated.UpdateApplicationSlugParams{
			ID:   created.ID,
			Slug: finalSlug,
		}); txErr != nil {
			return txErr
		}
		created.Slug = finalSlug
		return nil
	})
	if err != nil {
		return generated.Application{}, false, err
	}
	return created, true, nil
}

// ApplicationPendingChange reports what it would take to make the running
// container match the saved configuration: "source", "config", or "" for
// nothing pending. Shared by the REST wire shape (handler.applicationResponse)
// and the MCP tool-facing shape (internal/mcpserver) so the two can't drift
// on what the same three states mean — the rule itself doesn't belong to
// either presentation.
//
// Source outranks config because it subsumes it: a deploy applies config too,
// so reporting both would offer a "Reload to apply" that cannot actually
// finish the job.
//
// Suppressed entirely until the application has deployed at least once.
// Without that, an app configured before its first deploy would read "needs
// redeploy" from birth — the false positive that teaches people to ignore the
// indicator. Note this cannot key off last_activity_at, which is NOT NULL
// DEFAULT NOW() and so is already set at creation.
func ApplicationPendingChange(a generated.Application) string {
	switch {
	case !a.LastDeployedAt.Valid:
		return ""
	case a.SourceChangedAt.Valid:
		return "source"
	case a.ConfigChangedAt.Valid:
		return "config"
	}
	return ""
}
