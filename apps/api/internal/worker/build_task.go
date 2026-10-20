package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/build"
	"github.com/belune-hq/belune/internal/git"
	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/pkg/buildlog"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

type buildPayload struct {
	ApplicationID string `json:"application_id"`
	DeploymentID  string `json:"deployment_id"`
}

func (h *TaskHandler) HandleBuildTask(ctx context.Context, t *asynq.Task) error {
	var payload buildPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal build payload: %w", err)
	}

	slog.Info("handling build task", "application_id", payload.ApplicationID, "deployment_id", payload.DeploymentID)

	applicationID, err := parseUUID(payload.ApplicationID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid application_id (permanent): %w", err), asynq.SkipRetry)
	}
	deploymentID, err := parseUUID(payload.DeploymentID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid deployment_id (permanent): %w", err), asynq.SkipRetry)
	}

	// A standalone build never touches the running container, so failures here
	// must never change the application's status — buildOnly tells
	// failDeployment to leave it alone.
	dc := &deployContext{
		applicationID: applicationID,
		deploymentID:  deploymentID,
		buildOnly:     true,
	}

	// Update deployment status to building
	h.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
		ID:     deploymentID,
		Status: status.DeploymentBuilding,
	})

	// Fetch application details with project slug
	appRow, err := h.Queries.GetApplicationWithProjectSlug(ctx, applicationID)
	if err != nil {
		h.failDeployment(ctx, dc, "build", fmt.Sprintf("fetch application: %v", err))
		return errors.Join(fmt.Errorf("get application (permanent): %w", err), asynq.SkipRetry)
	}
	app := generated.Application{
		ID: appRow.ID, ProjectID: appRow.ProjectID, Name: appRow.Name,
		Type: appRow.Type, SourceRepo: appRow.SourceRepo, DockerfilePath: appRow.DockerfilePath,
		BuildType: appRow.BuildType, BuilderImage: appRow.BuilderImage,
		CustomBuildpacks: appRow.CustomBuildpacks, Status: appRow.Status,
		GitCredentialsEncrypted: appRow.GitCredentialsEncrypted,
		RootDirectory:           appRow.RootDirectory,
	}
	dc.app = app

	// Image-type applications have nothing to build
	if app.Type == "image" {
		slog.Info("skipping build for image-type application", "application_id", payload.ApplicationID)
		h.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
			ID:     deploymentID,
			Status: status.DeploymentSuccess,
		})
		return nil
	}

	// Clone the repository
	tmpDir, err := os.MkdirTemp("", "belune-build-*")
	if err != nil {
		h.failDeployment(ctx, dc, "build", fmt.Sprintf("create temp dir: %v", err))
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			slog.Warn("failed to clean up build dir", "path", tmpDir, "error", err)
		}
	}()

	buildCtx, buildCancel := context.WithTimeout(ctx, time.Duration(h.Config.BuildTimeoutMinutes)*time.Minute)
	defer buildCancel()

	var gitToken string
	if len(app.GitCredentialsEncrypted) > 0 {
		tokenBytes, decErr := h.Keyring.Decrypt(app.GitCredentialsEncrypted)
		if decErr != nil {
			slog.Warn("failed to decrypt git credentials, cloning without token", "error", decErr)
		} else {
			gitToken = string(tokenBytes)
		}
	}

	slog.Info("cloning repository", "repo", app.SourceRepo.String, "dest", tmpDir)
	cloneResult, err := git.Clone(buildCtx, app.SourceRepo.String, tmpDir, "", gitToken)
	if err != nil {
		h.failDeployment(ctx, dc, "build", fmt.Sprintf("git clone: %v", err))
		return fmt.Errorf("git clone: %w", err)
	}
	slog.Info("cloned repository", "commit", cloneResult.CommitSHA)

	buildDir, err := resolveBuildDir(tmpDir, app.RootDirectory.String)
	if err != nil {
		h.failDeployment(ctx, dc, "build", err.Error())
		return err
	}

	// Fetch and decrypt env vars for build-time use
	envVars, err := h.Queries.ListEnvVarsByApplication(ctx, applicationID)
	if err != nil {
		slog.Warn("failed to fetch env vars, continuing without them", "error", err)
	}

	env := make(map[string]string)
	for _, ev := range envVars {
		decrypted, err := h.Keyring.Decrypt(ev.ValueEncrypted)
		if err != nil {
			slog.Warn("failed to decrypt env var, skipping", "key", ev.Key, "error", err)
			continue
		}
		env[ev.Key] = string(decrypted)
	}

	// Parse custom buildpacks
	var customBuildpacks []string
	if len(app.CustomBuildpacks) > 0 {
		if err := json.Unmarshal(app.CustomBuildpacks, &customBuildpacks); err != nil {
			slog.Debug("could not parse custom buildpacks, using defaults", "app_id", payload.ApplicationID, "error", err)
		}
	}

	imageName := naming.ImageTag(appRow.ProjectSlug, appRow.Slug, payload.ApplicationID, payload.DeploymentID)

	// Set up build log streaming via Redis pub/sub
	pub := buildlog.NewPublisher(h.RedisClient, payload.DeploymentID)
	sink := buildlog.NewLogSink(pub, buildCtx)

	buildOpts := build.BuildOptions{
		SourceDir:      buildDir,
		ImageTag:       imageName,
		DockerfilePath: app.DockerfilePath.String,
		BuilderImage:   app.BuilderImage.String,
		Buildpacks:     customBuildpacks,
		Env:            env,
		StdoutWriter:   sink.Writer("stdout"),
		StderrWriter:   sink.Writer("stderr"),
		ApplicationID:  payload.ApplicationID,
	}

	slog.Info("building image", "tag", imageName)
	stopFlush := h.startBuildLogFlusher(ctx, deploymentID, sink)
	result, err := h.Chain.Build(buildCtx, buildOpts)
	sink.Flush()
	stopFlush()
	pub.Close(ctx)

	// Store the structured (NDJSON) build log built from the streamed lines. Do
	// this on failure too, so a failed build surfaces the full output in the log
	// viewer instead of only the (summarised) error message.
	if buildLogs := sink.NDJSON(); buildLogs != "" {
		h.Queries.UpdateDeploymentBuildLogs(ctx, generated.UpdateDeploymentBuildLogsParams{
			ID:        deploymentID,
			BuildLogs: pgtype.Text{String: buildLogs, Valid: true},
		})
	}

	if err != nil {
		h.failDeployment(ctx, dc, "build", fmt.Sprintf("build: %v", err))
		return fmt.Errorf("build: %w", err)
	}

	// Mark as success — no container creation
	h.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
		ID:     deploymentID,
		Status: status.DeploymentSuccess,
	})

	slog.Info("build completed",
		"application_id", payload.ApplicationID,
		"deployment_id", payload.DeploymentID,
		"image", result.ImageTag,
		"commit", cloneResult.CommitSHA,
	)
	return nil
}
