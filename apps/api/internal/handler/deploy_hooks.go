package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"go.opentelemetry.io/otel/attribute"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/pkg/tracing"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// deployHookPath is the public trigger route; the token is appended to it. The
// UI joins this to its own origin rather than a server-configured base URL,
// because the browser already knows the host the operator actually reaches us on.
const deployHookPath = "/api/webhooks/deploy/"

// deployHookTokenBytes is the entropy behind a hook token. 32 bytes matches the
// webhook secret and PAT sizing: the token is the *only* credential on the
// trigger endpoint, so it has to stand alone against offline guessing.
const deployHookTokenBytes = 32

type deployHookResponse struct {
	Enabled bool `json:"enabled"`
	// Path is returned (not a full URL) so the client composes the URL from the
	// origin it is served on. Empty when the hook is disabled.
	Path string `json:"path,omitempty"`
	// Token is populated only by generate and reveal — never by the status read.
	Token string `json:"token,omitempty"`
}

// generateDeployHookToken returns a URL-safe token and its SHA-256 digest. The
// digest is what we store for lookup: a leaked database read should not hand an
// attacker working deploy triggers.
func generateDeployHookToken() (token string, digest []byte, err error) {
	raw := make([]byte, deployHookTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate deploy hook token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

// GenerateDeployHook creates (or rotates) the deploy hook token for an
// application and returns it once in full. Rotating overwrites the stored hash,
// so the previous URL stops working immediately.
//
//apidoc:tag applications/deployments
//apidoc:title Generate Deploy Hook
//apidoc:order 7
func (h *Handler) GenerateDeployHook(w http.ResponseWriter, r *http.Request) {
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

	existing, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	token, digest, err := generateDeployHookToken()
	if err != nil {
		slog.Error("deploy hook: failed to generate token", "application", applicationID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to generate deploy hook token")
		return
	}

	encrypted, err := h.cfg.Keyring.Encrypt([]byte(token))
	if err != nil {
		slog.Error("deploy hook: failed to encrypt token", "application", applicationID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store deploy hook token")
		return
	}

	if _, err := h.queries.SetApplicationDeployHook(r.Context(), generated.SetApplicationDeployHookParams{
		ID:                       applicationUUID,
		DeployHookTokenHash:      digest,
		DeployHookTokenEncrypted: encrypted,
	}); err != nil {
		slog.Error("deploy hook: failed to persist token", "application", applicationID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store deploy hook token")
		return
	}

	action := "create_deploy_hook"
	if len(existing.DeployHookTokenHash) > 0 {
		action = "regenerate_deploy_hook"
	}
	h.audit(r, action, "application", applicationID, nil)

	writeJSON(w, http.StatusOK, deployHookResponse{
		Enabled: true,
		Path:    deployHookPath + token,
		Token:   token,
	})
}

// GetDeployHook reports whether the hook is enabled without returning the
// token — the settings page renders this on every load, and the token only
// leaves the server through the explicit reveal below.
//
//apidoc:tag applications/deployments
//apidoc:title Get Deploy Hook
//apidoc:order 6
func (h *Handler) GetDeployHook(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, deployHookResponse{Enabled: len(app.DeployHookTokenHash) > 0})
}

// RevealDeployHook returns the stored token so the operator can copy the URL
// again later (the alternative — show-once — means a lost URL forces a rotation
// and a CI config change). Audited, because it hands back a live credential.
//
//apidoc:tag applications/deployments
func (h *Handler) RevealDeployHook(w http.ResponseWriter, r *http.Request) {
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
	if len(app.DeployHookTokenEncrypted) == 0 {
		writeError(w, http.StatusNotFound, "deploy hook is not enabled")
		return
	}

	token, err := h.cfg.Keyring.Decrypt(app.DeployHookTokenEncrypted)
	if err != nil {
		slog.Error("deploy hook: failed to decrypt token", "application", applicationID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to decrypt deploy hook token")
		return
	}

	h.audit(r, "reveal_deploy_hook", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, deployHookResponse{
		Enabled: true,
		Path:    deployHookPath + string(token),
		Token:   string(token),
	})
}

// DeleteDeployHook disables the hook. The stored hash goes away, so the URL
// stops resolving on the next request.
//
//apidoc:tag applications/deployments
//apidoc:title Delete Deploy Hook
//apidoc:order 8
func (h *Handler) DeleteDeployHook(w http.ResponseWriter, r *http.Request) {
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

	if _, err := h.queries.ClearApplicationDeployHook(r.Context(), applicationUUID); err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}

	h.audit(r, "delete_deploy_hook", "application", applicationID, nil)

	writeJSON(w, http.StatusOK, deployHookResponse{Enabled: false})
}

// HandleDeployHook is the public trigger: POST /api/webhooks/deploy/{token}.
// There is no auth middleware on this route — the token in the path *is* the
// credential, the same shape CI systems expect from Vercel/Netlify-style hooks.
//
// Deliberately no branch logic: the caller decides when to fire. Branch
// filtering stays a git-push-webhook concept, where the branch is payload data.
func (h *Handler) HandleDeployHook(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracing.Tracer().Start(r.Context(), "webhook.deploy_hook")
	defer span.End()
	r = r.WithContext(ctx)

	start := time.Now()
	defer func() { metrics.RecordWebhookDelivery("deploy-hook", time.Since(start)) }()

	token := chi.URLParam(r, "token")
	if token == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	sum := sha256.Sum256([]byte(token))

	app, err := h.queries.GetApplicationByDeployHookToken(ctx, sum[:])
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("deploy hook: lookup failed", "error", err)
		}
		// Same response for an unknown, disabled, or malformed token: the
		// endpoint must not tell a URL-bearer which of those they hit.
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	applicationID := uuidToString(app.ID)
	span.SetAttributes(attribute.String("application.id", applicationID))

	if err := h.triggerDeployHook(r, app); err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			// A deploy is already running for this app. Treat a colliding
			// trigger as satisfied rather than an error: CI that retries on
			// non-2xx would otherwise hammer an app that is already deploying.
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "in_progress"})
			return
		}
		slog.Error("deploy hook: failed to trigger deploy", "application", app.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to trigger deploy")
		return
	}

	h.audit(r, "trigger_deploy_hook", "application", applicationID, map[string]any{
		"application_type": app.Type,
	})

	slog.Info("deploy hook: triggered deploy", "application", app.Name, "type", app.Type)
	// Minimal body on purpose — the caller holds a URL, not an account, so the
	// response should not describe the application it just deployed.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// triggerDeployHook enqueues a normal deploy — the same task a manual deploy
// runs, for both application types.
//
// Note it does NOT take the Reload path for image apps. Reload pins the
// deployment to the image digest that is already running, which is right for
// "apply my config changes" but wrong here: a hook fires precisely because CI
// just pushed a NEW image to the same tag, and pinning the old digest would
// redeploy the very image the caller is trying to replace. The plain deploy
// path re-pulls the configured tag and re-pins to the freshly resolved digest,
// which is what a deploy hook means.
func (h *Handler) triggerDeployHook(r *http.Request, app generated.Application) error {
	applicationID := uuidToString(app.ID)

	deployment, err := h.queries.CreateDeployment(r.Context(), generated.CreateDeploymentParams{
		ApplicationID: app.ID,
		Status:        status.DeploymentPending,
		TriggeredBy:   "hook",
	})
	if err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}

	payload, err := json.Marshal(deployPayload{
		ApplicationID: applicationID,
		DeploymentID:  formatDeploymentID(deployment.ID),
		TraceCarrier:  tracing.InjectContext(r.Context()),
	})
	if err != nil {
		h.failDeploymentEnqueue(r.Context(), deployment.ID, err)
		return fmt.Errorf("marshal deploy payload: %w", err)
	}

	if err := h.enqueueDeployTask(applicationID, payload); err != nil {
		h.failDeploymentEnqueue(r.Context(), deployment.ID, err)
		return err
	}
	return nil
}
