package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// Changing where an application comes from used to mean deleting it and making
// a new one, which is not a workaround but data loss: ApplicationService.Delete
// removes the persistent data volumes, and ON DELETE CASCADE takes domains,
// env vars, file mounts, volumes, deployments, and request logs with it.
// Re-adding the domains then re-issues certificates against Let's Encrypt's
// five-duplicates-per-week limit.
//
// Domains, volumes, mounts, and env belong to the *application*; the source is
// configuration, and swapping it keeps all of them.
//
// This is an explicit action rather than an editable field because several
// columns have to move together and be validated as a unit — type, build_type,
// and whichever source column now applies. A form that let them drift is how
// incoherent rows were created in the first place.

type changeSourceRequest struct {
	Type string `json:"type"` // the type to switch TO

	// Image target.
	SourceImage string `json:"source_image"`

	// Git target.
	SourceRepo       string `json:"source_repo"`
	Branch           string `json:"branch"`
	BuildType        string `json:"build_type"`
	DockerfilePath   string `json:"dockerfile_path"`
	RootDirectory    string `json:"root_directory"`
	GitIntegrationID string `json:"git_integration_id"`
	GitToken         string `json:"git_token"`
}

//apidoc:tag applications
func (h *Handler) ChangeApplicationSource(w http.ResponseWriter, r *http.Request) {
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

	current, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	var req changeSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Type == current.Type {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("this application is already a %s application; edit its source in Settings instead", current.Type))
		return
	}

	if req.Type == "git" {
		if !h.canAttachGitIntegration(r, parseOptionalUUID(req.GitIntegrationID)) {
			writeError(w, http.StatusForbidden, "access denied to git integration")
			return
		}
	}

	// A preview child is git-only by construction — it exists because a branch
	// matched a pattern — so switching the parent to an image would orphan it.
	previews, err := h.queries.ListPreviewsByParent(r.Context(), applicationUUID)
	if err == nil && len(previews) > 0 {
		writeError(w, http.StatusConflict, "delete this application's preview environments before changing its source")
		return
	}

	// The worker re-reads the application row at several stages, so switching
	// underneath a running deploy could build one source and deploy the other.
	if active, err := h.queries.CountActiveDeployments(r.Context(), applicationUUID); err == nil && active > 0 {
		writeError(w, http.StatusConflict, "a deployment is in progress — wait for it to finish before changing the source")
		return
	}

	params, err := h.buildChangeSourceParams(applicationUUID, current, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := validateSource(sourceFields{
		Type:           params.Type,
		BuildType:      params.BuildType,
		DockerfilePath: params.DockerfilePath.String,
		SourceRepo:     params.SourceRepo.String,
		SourceImage:    params.SourceImage.String,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	app, err := h.queries.ChangeApplicationSource(r.Context(), params)
	if err != nil {
		slog.Error("failed to change application source", "error", err, "application_id", applicationID)
		writeError(w, http.StatusInternalServerError, "failed to change application source")
		return
	}

	h.audit(r, "change_application_source", "application", applicationID, map[string]any{
		"from": current.Type,
		"to":   app.Type,
	})

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}

// buildChangeSourceParams assembles the full row state for the target type.
//
// Every column is written explicitly, including the ones being abandoned: a
// leftover source_repo on an image application is precisely the incoherence
// that let a push webhook match an application it could not build.
func (h *Handler) buildChangeSourceParams(
	id pgtype.UUID,
	current generated.Application,
	req changeSourceRequest,
) (generated.ChangeApplicationSourceParams, error) {
	p := generated.ChangeApplicationSourceParams{ID: id, Type: req.Type}

	switch req.Type {
	case "image":
		if req.SourceImage == "" {
			return p, fmt.Errorf("an image application needs a source_image")
		}
		p.BuildType = "image"
		p.SourceImage = pgtype.Text{String: req.SourceImage, Valid: true}
		// Everything git-shaped is dropped. The credentials and the push
		// webhook secret authenticate against a repository this application no
		// longer has, so keeping them would leave secrets at rest for no
		// reason — and has_webhook_secret would report a push hook that can
		// never fire.
		return p, nil

	case "git":
		if req.SourceRepo == "" {
			return p, fmt.Errorf("a git application needs a source_repo")
		}
		if !validBranchName(req.Branch) {
			return p, fmt.Errorf("invalid branch name")
		}
		if !validRootDirectory(req.RootDirectory) {
			return p, fmt.Errorf("invalid root directory")
		}
		buildType := req.BuildType
		if buildType == "" {
			// Matches the create dialog's default. Railpack detects the stack
			// rather than requiring a Dockerfile, so it is the safest guess
			// when the caller does not care.
			buildType = "railpack"
		}
		p.BuildType = buildType
		p.SourceRepo = pgtype.Text{String: req.SourceRepo, Valid: true}
		p.DockerfilePath = pgtype.Text{String: req.DockerfilePath, Valid: req.DockerfilePath != ""}
		p.RootDirectory = pgtype.Text{String: req.RootDirectory, Valid: req.RootDirectory != ""}
		// Both branch columns move together, as everywhere else: one
		// user-facing branch decides what is built and which pushes deploy.
		p.Branch = pgtype.Text{String: req.Branch, Valid: req.Branch != ""}
		p.AutoDeployBranch = p.Branch
		p.GitIntegrationID = parseOptionalUUID(req.GitIntegrationID)
		if req.GitToken != "" {
			encrypted, err := h.cfg.Keyring.Encrypt([]byte(req.GitToken))
			if err != nil {
				return p, fmt.Errorf("could not encrypt git token")
			}
			p.GitCredentialsEncrypted = encrypted
		}
		return p, nil

	default:
		return p, fmt.Errorf("type must be one of: git, image")
	}
}
