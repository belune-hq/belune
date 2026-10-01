package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/naming"
	"github.com/weiliang79/belune/internal/pkg/tracing"
	"github.com/weiliang79/belune/internal/quota"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/status"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/worker"
)

// parseOptionalUUID converts a possibly-empty UUID string into a pgtype.UUID.
// An empty or unparseable string yields an invalid (NULL) UUID.
func parseOptionalUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	if s == "" {
		return u
	}
	_ = u.Scan(s)
	return u
}

// resolveOptionalString applies keep/clear/set semantics for a nullable text
// column on update: nil (key absent) keeps the stored value; a non-nil pointer,
// including to "", is the new value ("" clears it).
func resolveOptionalString(in *string, current pgtype.Text) string {
	if in == nil {
		return current.String
	}
	return *in
}

// resolveOptionalUUID applies preserve/clear/set semantics for an optional FK on
// update: nil pointer (key absent) preserves the current value; an empty string
// clears it to NULL; a UUID string sets it.
func resolveOptionalUUID(in *string, current pgtype.UUID) pgtype.UUID {
	if in == nil {
		return current
	}
	return parseOptionalUUID(*in)
}

// Field/column names here are the stable internal identifiers. The UI renames
// two of them in its copy only: `type` is shown as "Source" and `build_type`
// as "Build Method". The schema and JSON keep the original names, so when
// something in the UI mentions Source or Build Method, this is the field.
type createApplicationRequest struct {
	Name             string  `json:"name"`
	Slug             string  `json:"slug"`               // optional, auto-generated from name if empty
	Type             string  `json:"type"`               // UI "Source": "git" or "image"
	SourceRepo       string  `json:"source_repo"`        // for git type
	SourceImage      string  `json:"source_image"`       // for image type
	DockerfilePath   string  `json:"dockerfile_path"`    // optional
	BuildType        string  `json:"build_type"`         // UI "Build Method": dockerfile, buildpacks, railpack, image
	CPULimit         float64 `json:"cpu_limit"`          // CPU cores (0 = unlimited)
	MemoryLimit      int64   `json:"memory_limit"`       // bytes (0 = unlimited)
	GitToken         string  `json:"git_token"`          // PAT for private repos; encrypted server-side
	HealthCheckPath  string  `json:"health_check_path"`  // HTTP path to poll after deploy (e.g. /healthz)
	GitIntegrationID string  `json:"git_integration_id"` // optional connected provider account
	Branch           string  `json:"branch"`             // ref to build; empty = repository default
	RootDirectory    string  `json:"root_directory"`     // subdirectory to build from; empty = repo root
}

//apidoc:tag applications
//apidoc:title Create Application
//apidoc:order 3
func (h *Handler) CreateApplication(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req createApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Type == "" || req.BuildType == "" {
		writeError(w, http.StatusBadRequest, "name, type, and build_type are required")
		return
	}

	if err := validateSource(sourceFields{
		Type:           req.Type,
		BuildType:      req.BuildType,
		DockerfilePath: req.DockerfilePath,
		SourceRepo:     req.SourceRepo,
		SourceImage:    req.SourceImage,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !validBranchName(req.Branch) {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}

	if !validRootDirectory(req.RootDirectory) {
		writeError(w, http.StatusBadRequest, "invalid root directory")
		return
	}

	gitIntegrationID := parseOptionalUUID(req.GitIntegrationID)
	if !h.canAttachGitIntegration(r, gitIntegrationID) {
		writeError(w, http.StatusForbidden, "access denied to git integration")
		return
	}

	// Fetch project to get its slug for the final slug format
	project, err := h.queries.GetProject(r.Context(), projectUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	if h.quotaSvc != nil {
		if err := h.quotaSvc.CheckApplicationCreate(r.Context(), projectUUID, project.UserID, req.CPULimit, req.MemoryLimit); err != nil {
			var qe *quota.ExceededError
			if errors.As(err, &qe) {
				writeError(w, qe.HTTPStatus(), qe.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "quota check failed")
			return
		}
	}

	baseSlug := naming.Slugify(req.Name)
	if req.Slug != "" {
		baseSlug = naming.Slugify(req.Slug)
	}

	app, err := h.appService.Create(r.Context(), service.CreateApplicationParams{
		ProjectID:        projectUUID,
		ProjectSlug:      project.Slug,
		Name:             req.Name,
		BaseSlug:         baseSlug,
		Type:             req.Type,
		SourceRepo:       req.SourceRepo,
		SourceImage:      req.SourceImage,
		DockerfilePath:   req.DockerfilePath,
		BuildType:        req.BuildType,
		CPULimit:         req.CPULimit,
		MemoryLimit:      req.MemoryLimit,
		GitToken:         req.GitToken,
		HealthCheckPath:  req.HealthCheckPath,
		GitIntegrationID: gitIntegrationID,
		Branch:           req.Branch,
		RootDirectory:    req.RootDirectory,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create application")
		return
	}

	h.audit(r, "create_application", "application", uuidToString(app.ID), map[string]any{"name": req.Name, "project_id": projectID})

	h.maybeEnqueueQuotaAlert(r, projectUUID, project.UserID)

	writeJSON(w, http.StatusCreated, toApplicationResponse(app))
}

//apidoc:tag applications
//apidoc:title Get Application
//apidoc:order 2
func (h *Handler) GetApplication(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "applicationId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	app, err := h.queries.GetApplication(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}

// GetApplicationHealth returns the most recent post-deploy health probe
// result. The probe runs as the final stage of HandleDeployTask and writes
// to deployments.health_status; this endpoint is a read-only view of that
// row. Status values:
//
//   - passing | failing  — deploy probe outcome
//   - skipped            — app has no health_check_path configured
//   - pending            — no deploy has run yet (no row found)
//
//apidoc:tag applications/resources
//apidoc:title Get Health
//apidoc:order 2
func (h *Handler) GetApplicationHealth(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "applicationId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	row, err := h.queries.GetLatestApplicationHealth(r.Context(), uuid)
	if err != nil {
		// pgx.ErrNoRows here means the application has no deployments yet;
		// surface that as a "pending" state rather than a 404 so the UI can
		// distinguish "never deployed" from "app missing".
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "pending",
			"message": "no deployments yet",
		})
		return
	}

	status := row.HealthStatus.String
	if !row.HealthStatus.Valid || status == "" {
		status = "pending"
	}
	resp := map[string]any{
		"deployment_id": row.ID,
		"deploy_status": row.Status,
		"status":        status,
		"message":       row.HealthMessage.String,
	}
	if row.HealthCheckedAt.Valid {
		resp["checked_at"] = row.HealthCheckedAt.Time
	}
	writeJSON(w, http.StatusOK, resp)
}

//apidoc:tag applications
//apidoc:title Get Applications
//apidoc:order 1
func (h *Handler) ListApplications(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	applications, err := h.queries.ListApplicationsByProject(r.Context(), projectUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list applications")
		return
	}

	writeJSON(w, http.StatusOK, toApplicationResponses(applications))
}

// deployPayload is the worker payload; the type lives with the service that
// builds it so REST and MCP cannot drift.
type deployPayload = service.DeployPayload

//apidoc:tag applications/deployments
//apidoc:title Deploy App
//apidoc:order 1
func (h *Handler) DeployApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// Verify application exists
	_, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	deployment, err := h.deployQueue.TriggerDeployment(r.Context(), applicationUUID, "manual")
	if err != nil {
		if errors.Is(err, service.ErrDeployInProgress) {
			writeError(w, http.StatusConflict, "a deployment is already in progress for this application")
			return
		}
		slog.Error("failed to trigger deployment", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create deployment")
		return
	}

	h.audit(r, "deploy_application", "application", applicationID, nil)

	writeJSON(w, http.StatusAccepted, deployment)
}

//apidoc:tag applications/actions
//apidoc:title Stop App
//apidoc:order 4
func (h *Handler) StopApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	app, err := h.appService.Stop(r.Context(), applicationUUID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrApplicationNotFound):
			writeError(w, http.StatusNotFound, "application not found")
		case errors.Is(err, service.ErrServerUnreachable):
			writeError(w, http.StatusInternalServerError, "failed to reach the application's server")
		case errors.Is(err, service.ErrContainerAction):
			writeError(w, http.StatusInternalServerError, "failed to stop application")
		default:
			writeError(w, http.StatusInternalServerError, "failed to update application status")
		}
		return
	}

	h.audit(r, "stop_application", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}

//apidoc:tag applications/actions
//apidoc:title Start App
//apidoc:order 3
func (h *Handler) StartApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	app, err := h.appService.Start(r.Context(), applicationUUID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrApplicationNotFound):
			writeError(w, http.StatusNotFound, "application not found")
		case errors.Is(err, service.ErrServerUnreachable):
			writeError(w, http.StatusInternalServerError, "failed to reach the application's server")
		case errors.Is(err, service.ErrContainerAction):
			writeError(w, http.StatusInternalServerError, "failed to start application")
		default:
			writeError(w, http.StatusInternalServerError, "failed to update application status")
		}
		return
	}

	h.audit(r, "start_application", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}

//apidoc:tag applications/actions
//apidoc:title Restart App
//apidoc:order 5
func (h *Handler) RestartApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	rt, err := h.runtimes.For(r.Context(), row.ServerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reach the application's server")
		return
	}

	// Stop and start the existing container (no rebuild)
	containerName := naming.ContainerName(row.ProjectSlug, row.Slug, applicationID)
	if err := rt.StopContainer(r.Context(), containerName); err != nil {
		slog.Error("failed to stop container for restart", "container", containerName, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to stop container")
		return
	}
	if err := rt.StartContainer(r.Context(), containerName); err != nil {
		slog.Error("failed to start container for restart", "container", containerName, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start container")
		return
	}

	app, err := h.queries.UpdateApplicationStatus(r.Context(), generated.UpdateApplicationStatusParams{
		ID:     applicationUUID,
		Status: status.ApplicationRunning,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update application status")
		return
	}

	h.audit(r, "restart_application", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}

// ReloadApplication recreates the application container from its current image
// (the latest successful deployment's image) WITHOUT rebuilding, so config
// changes — volumes, file mounts, env, resource limits — take effect quickly
// and without pulling new code. It reuses the deploy worker's skip-build path
// (RollbackImageTag), which re-reads all config on container recreate.
//
//apidoc:tag applications/actions
//apidoc:title Reload App
//apidoc:order 6
func (h *Handler) ReloadApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	latest, err := h.queries.GetLatestSuccessfulDeployment(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusConflict, "no successful deployment to reload — deploy the application first")
		return
	}

	deployment, err := h.queries.CreateDeployment(r.Context(), generated.CreateDeploymentParams{
		ApplicationID: applicationUUID,
		Status:        status.DeploymentPending,
		TriggeredBy:   "reload",
	})
	if err != nil {
		slog.Error("failed to create reload deployment", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create deployment")
		return
	}

	payload, err := json.Marshal(deployPayload{
		ApplicationID:    applicationID,
		DeploymentID:     formatDeploymentID(deployment.ID),
		RollbackImageTag: latest.ImageTag.String,
		TraceCarrier:     tracing.InjectContext(r.Context()),
	})
	if err != nil {
		slog.Error("failed to marshal reload payload", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create reload task")
		return
	}

	if err := h.enqueueDeployTask(applicationID, payload); err != nil {
		h.failDeploymentEnqueue(r.Context(), deployment.ID, err)
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			writeError(w, http.StatusConflict, "a deployment is already in progress for this application")
			return
		}
		slog.Error("failed to enqueue reload task", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to enqueue reload task")
		return
	}

	h.audit(r, "reload_application", "application", applicationID, nil)
	writeJSON(w, http.StatusAccepted, deployment)
}

// RebuildApplication rebuilds the application from the commit that is currently
// deployed (not branch HEAD) and recreates the container. Git-source apps only:
// it re-runs the build for the running version, picking up patched base images
// and refreshed dependencies without shipping new code.
//
//apidoc:tag applications/actions
//apidoc:title Rebuild App
//apidoc:order 2
func (h *Handler) RebuildApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	app, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	if app.Type != "git" {
		writeError(w, http.StatusBadRequest, "rebuild is only available for git-source applications")
		return
	}

	latest, err := h.queries.GetLatestSuccessfulDeployment(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusConflict, "no successful deployment to rebuild — deploy the application first")
		return
	}
	if !latest.CommitSha.Valid || latest.CommitSha.String == "" {
		writeError(w, http.StatusConflict, "the current deployment has no recorded commit — deploy the application first")
		return
	}

	deployment, err := h.queries.CreateDeployment(r.Context(), generated.CreateDeploymentParams{
		ApplicationID: applicationUUID,
		Status:        status.DeploymentPending,
		TriggeredBy:   "rebuild",
		CommitSha:     latest.CommitSha,
	})
	if err != nil {
		slog.Error("failed to create rebuild deployment", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create deployment")
		return
	}

	payload, err := json.Marshal(deployPayload{
		ApplicationID: applicationID,
		DeploymentID:  formatDeploymentID(deployment.ID),
		CommitSHA:     latest.CommitSha.String,
		TraceCarrier:  tracing.InjectContext(r.Context()),
	})
	if err != nil {
		slog.Error("failed to marshal rebuild payload", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create rebuild task")
		return
	}

	if err := h.enqueueDeployTask(applicationID, payload); err != nil {
		h.failDeploymentEnqueue(r.Context(), deployment.ID, err)
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			writeError(w, http.StatusConflict, "a deployment is already in progress for this application")
			return
		}
		slog.Error("failed to enqueue rebuild task", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to enqueue rebuild task")
		return
	}

	h.audit(r, "rebuild_application", "application", applicationID, nil)
	writeJSON(w, http.StatusAccepted, deployment)
}

func formatDeploymentID(id pgtype.UUID) string { return service.FormatDeploymentID(id) }

// enqueueDeployLike, enqueueDeployTask and failDeploymentEnqueue delegate to
// service.DeployQueue, which owns the per-application TaskID guard, the
// no-retry policy and the stale-task reclaim.
func (h *Handler) enqueueDeployLike(queue, applicationID string, task *asynq.Task) error {
	return h.deployQueue.EnqueueLike(queue, applicationID, task)
}

func (h *Handler) enqueueDeployTask(applicationID string, payload []byte) error {
	return h.deployQueue.EnqueueDeploy(applicationID, payload)
}

func (h *Handler) failDeploymentEnqueue(ctx context.Context, deploymentID pgtype.UUID, cause error) {
	h.deployQueue.FailEnqueue(ctx, deploymentID, cause)
}

// The source fields are pointers so an omitted key (nil: keep the stored value)
// is different from an empty string (clear it). Decoding them as plain strings
// made every omitted key look like a request to clear, and the service stores
// "" as NULL — so a partial PUT silently wiped whatever it did not mention, with
// a 200. name, git_token and the fields the service ignores keep their
// existing "empty means unchanged" reading.
type updateApplicationRequest struct {
	Name              string  `json:"name"`
	SourceRepo        *string `json:"source_repo"`
	SourceImage       *string `json:"source_image"`
	DockerfilePath    *string `json:"dockerfile_path"`
	BuildTypeOverride *string `json:"build_type_override"`
	BuilderImage      *string `json:"builder_image"`
	CPULimit          float64 `json:"cpu_limit"`
	MemoryLimit       int64   `json:"memory_limit"`
	GitToken          string  `json:"git_token"`         // PAT for private repos; encrypted server-side; empty = preserve existing
	HealthCheckPath   string  `json:"health_check_path"` // HTTP path to poll after deploy; empty = clear
	Branch            *string `json:"branch"`            // ref to build; absent = keep, empty = repository default
	RootDirectory     *string `json:"root_directory"`    // subdirectory to build from; absent = keep, empty = repo root
	// GitIntegrationID: pointer so we can tell "absent" (preserve) from ""
	// (clear) from a UUID (set the connected provider account).
	GitIntegrationID *string `json:"git_integration_id"`
}

//apidoc:tag applications
//apidoc:title Update Application
//apidoc:order 4
func (h *Handler) UpdateApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// Get current application to preserve unchanged fields
	current, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	var req updateApplicationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Resolved against the stored row up front so validation and the write see
	// the same effective values.
	sourceRepo := resolveOptionalString(req.SourceRepo, current.SourceRepo)
	sourceImage := resolveOptionalString(req.SourceImage, current.SourceImage)
	dockerfilePath := resolveOptionalString(req.DockerfilePath, current.DockerfilePath)
	buildTypeOverride := resolveOptionalString(req.BuildTypeOverride, current.BuildTypeOverride)
	builderImage := resolveOptionalString(req.BuilderImage, current.BuilderImage)
	branch := resolveOptionalString(req.Branch, current.Branch)
	rootDirectory := resolveOptionalString(req.RootDirectory, current.RootDirectory)

	if !validBranchName(branch) {
		writeError(w, http.StatusBadRequest, "invalid branch name")
		return
	}

	if !validRootDirectory(rootDirectory) {
		writeError(w, http.StatusBadRequest, "invalid root directory")
		return
	}

	// Only checked when the resolved value actually CHANGES the attached
	// integration — not merely whether the field is present in the request.
	// A client that PUTs the full resource back (unchanged git_integration_id
	// included) must not be rejected for an attachment nobody is touching,
	// and a shared member editing unrelated fields must not be blocked by an
	// integration they don't own but didn't attach.
	gitIntegrationID := resolveOptionalUUID(req.GitIntegrationID, current.GitIntegrationID)
	if gitIntegrationID != current.GitIntegrationID && !h.canAttachGitIntegration(r, gitIntegrationID) {
		writeError(w, http.StatusForbidden, "access denied to git integration")
		return
	}

	// type and build_type are not updatable, so they come from the stored row:
	// the request only ever moves the fields that have to stay coherent with
	// them.
	if err := validateSource(sourceFields{
		Type:              current.Type,
		BuildType:         current.BuildType,
		BuildTypeOverride: buildTypeOverride,
		DockerfilePath:    dockerfilePath,
		SourceRepo:        sourceRepo,
		SourceImage:       sourceImage,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	app, err := h.appService.Update(r.Context(), applicationUUID, current, service.UpdateApplicationParams{
		Name:              req.Name,
		SourceRepo:        sourceRepo,
		SourceImage:       sourceImage,
		DockerfilePath:    dockerfilePath,
		BuildTypeOverride: buildTypeOverride,
		BuilderImage:      builderImage,
		CPULimit:          req.CPULimit,
		MemoryLimit:       req.MemoryLimit,
		GitToken:          req.GitToken,
		HealthCheckPath:   req.HealthCheckPath,
		GitIntegrationID:  gitIntegrationID,
		Branch:            branch,
		RootDirectory:     rootDirectory,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update application")
		return
	}

	h.markApplicationUpdate(r.Context(), current, app)

	h.audit(r, "update_application", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}

//apidoc:tag applications
func (h *Handler) DeleteApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	// Owner-only: shared access grants full operational use of the project's
	// applications, but not the right to destroy one.
	if !h.isApplicationOwner(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	if err := h.appService.Delete(r.Context(), applicationUUID, row.ProjectSlug, row.Slug); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete application")
		return
	}

	h.audit(r, "delete_application", "application", applicationID, map[string]any{"name": row.Name})

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetBuildCache reports the size (in bytes) of the CNB build + launch cache
// volumes for this application. Missing volumes contribute zero; an app that
// has never built surfaces 0 B with no error.
//
//apidoc:tag applications/resources
//apidoc:title Get Build Cache
//apidoc:order 5
func (h *Handler) GetBuildCache(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	rt, err := h.runtimes.For(r.Context(), row.ServerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reach the application's server")
		return
	}

	buildVol := naming.CNBCacheVolumeName(applicationID)
	launchVol := naming.CNBLaunchCacheVolumeName(applicationID)

	sizes, err := rt.VolumeSizes(r.Context(), []string{buildVol, launchVol})
	if err != nil {
		slog.Warn("failed to read cache volume sizes", "application_id", applicationID, "error", err)
	}

	buildSize := sizes[buildVol]
	launchSize := sizes[launchVol]

	writeJSON(w, http.StatusOK, map[string]any{
		"build_cache_bytes":  buildSize,
		"launch_cache_bytes": launchSize,
		"total_bytes":        buildSize + launchSize,
	})
}

// ClearBuildCache deletes the application's CNB cache volumes. The next
// build will re-create them from scratch — this is the "make it fresh"
// escape hatch when a cache is suspected of poisoning output.
//
//apidoc:tag applications/resources
//apidoc:title Clear Build Cache
//apidoc:order 6
func (h *Handler) ClearBuildCache(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	rt, err := h.runtimes.For(r.Context(), row.ServerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reach the application's server")
		return
	}

	for _, vol := range []string{
		naming.CNBCacheVolumeName(applicationID),
		naming.CNBLaunchCacheVolumeName(applicationID),
	} {
		if err := rt.RemoveVolume(r.Context(), vol); err != nil {
			slog.Debug("could not remove cache volume (may not exist)", "volume", vol, "error", err)
		}
	}

	h.audit(r, "clear_build_cache", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

//apidoc:tag applications/actions
//apidoc:title Build App
//apidoc:order 1
func (h *Handler) BuildApplication(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// Verify application exists
	_, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	// Create deployment record
	deployment, err := h.queries.CreateDeployment(r.Context(), generated.CreateDeploymentParams{
		ApplicationID: applicationUUID,
		Status:        status.DeploymentPending,
		TriggeredBy:   "manual",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create deployment")
		return
	}

	// Enqueue build task (not deploy)
	payload, _ := json.Marshal(deployPayload{
		ApplicationID: applicationID,
		DeploymentID:  fmt.Sprintf("%x-%x-%x-%x-%x", deployment.ID.Bytes[0:4], deployment.ID.Bytes[4:6], deployment.ID.Bytes[6:8], deployment.ID.Bytes[8:10], deployment.ID.Bytes[10:16]),
		TraceCarrier:  tracing.InjectContext(r.Context()),
	})

	if err := h.enqueueDeployLike("default", applicationID, asynq.NewTask("build", payload)); err != nil {
		h.failDeploymentEnqueue(r.Context(), deployment.ID, err)
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			writeError(w, http.StatusConflict, "a deployment is already in progress for this application")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to enqueue build task")
		return
	}

	writeJSON(w, http.StatusAccepted, deployment)
}

// isNotFound returns true when err is a pgx no-rows sentinel (quota prefs table miss).
func isNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// newAlertQuotaTask builds the asynq email task for a quota threshold alert.
func newAlertQuotaTask(toEmail, projectName string, usagePct, threshold int) (*asynq.Task, error) {
	return worker.NewEmailSendTask("alert_quota_threshold", toEmail, map[string]any{
		"ProjectName":      projectName,
		"QuotaType":        "applications",
		"UsagePercent":     usagePct,
		"ThresholdPercent": threshold,
	})
}

// maybeEnqueueQuotaAlert checks the project's quota usage after an app is created
// and enqueues an alert email if the owner's threshold has been crossed.
func (h *Handler) maybeEnqueueQuotaAlert(r *http.Request, projectID, ownerUserID pgtype.UUID) {
	if h.quotaSvc == nil || h.asynq == nil {
		return
	}

	ownerPrefs, err := h.queries.GetAlertPreferences(r.Context(), ownerUserID)
	threshold := 80
	if err == nil {
		if !ownerPrefs.QuotaThreshold {
			return
		}
		threshold = int(ownerPrefs.QuotaThresholdPercent)
	} else if !isNotFound(err) {
		return
	}

	alert, err := h.quotaSvc.MaybeAlertProjectThreshold(r.Context(), projectID, threshold)
	if err != nil || alert == nil {
		return
	}

	ownerInfo, err := h.queries.GetProjectOwnerInfo(r.Context(), projectID)
	if err != nil {
		return
	}

	task, err := newAlertQuotaTask(ownerInfo.Email, ownerInfo.ProjectName, alert.CurrentPercent, alert.Threshold)
	if err != nil {
		slog.Warn("quota alert: failed to build email task", "error", err)
		return
	}
	if _, err := h.asynq.Enqueue(task); err != nil {
		slog.Warn("quota alert: failed to enqueue email task", "error", err)
	}
}
