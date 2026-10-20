package handler

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/git/providers"
	"github.com/belune-hq/belune/internal/preview"
	"github.com/belune-hq/belune/internal/store/generated"
)

// HandleProviderWebhook receives push webhooks for a connected provider app
// (GitHub App / OAuth). The signature is verified once against the provider
// app's configured webhook secret(s); matching applications are then deployed.
func (h *Handler) HandleProviderWebhook(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if !validGitProviders[provider] {
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	defer r.Body.Close()

	secrets, err := h.gitProviderSvc.WebhookSecrets(r.Context(), provider)
	if err != nil {
		slog.Warn("provider webhook: failed to load secrets", "provider", provider, "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Verify against each configured webhook secret until one matches. With no
	// secret configured nothing verifies, so the delivery is rejected.
	var event providers.PushEvent
	verified := false
	for _, secret := range secrets {
		if ev, perr := providers.VerifyAndParseWebhook(provider, r.Header, body, secret); perr == nil {
			event = ev
			verified = true
			break
		}
	}
	if !verified {
		slog.Warn("provider webhook: signature verification failed", "provider", provider)
		w.WriteHeader(http.StatusOK)
		return
	}
	if event.RepoURL == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	triggered := h.deployMatchingApps(r, event)
	slog.Info("provider webhook: processed", "provider", provider, "repo", event.RepoURL, "triggered", triggered)
	w.WriteHeader(http.StatusOK)
}

// deployMatchingApps finds applications whose source_repo matches the event's
// repo and dispatches a push deploy to each. Returns the number of deploys
// triggered.
func (h *Handler) deployMatchingApps(r *http.Request, event providers.PushEvent) int {
	normalized := normalizeRepoURL(event.RepoURL)
	apps, err := h.queries.ListApplicationsBySourceRepo(r.Context(), pgtype.Text{String: normalized, Valid: true})
	if err != nil {
		slog.Warn("provider webhook: failed to query applications", "error", err)
		return 0
	}
	if len(apps) == 0 {
		apps, _ = h.queries.ListApplicationsBySourceRepo(r.Context(), pgtype.Text{String: normalized + ".git", Valid: true})
	}
	triggered := 0
	for _, app := range apps {
		if h.dispatchAppPush(r, app, event) {
			triggered++
		}
	}
	return triggered
}

// dispatchAppPush routes an already-verified push to an application: a deploy
// when the branch matches auto_deploy_branch, or a preview deploy when it
// matches the parent's preview pattern. Returns true when a deploy was queued.
func (h *Handler) dispatchAppPush(r *http.Request, app generated.Application, event providers.PushEvent) bool {
	branch := event.Branch
	autoBranch := "main"
	if app.AutoDeployBranch.Valid && app.AutoDeployBranch.String != "" {
		autoBranch = app.AutoDeployBranch.String
	}
	if branch == autoBranch {
		return h.triggerPushDeploy(r, app, event)
	}

	// Preview path: branch did not match auto_deploy_branch, but may match a
	// configured preview pattern on this parent app.
	if !app.PreviewBranchPattern.Valid || app.PreviewBranchPattern.String == "" ||
		!app.PreviewDomainTemplate.Valid || app.PreviewDomainTemplate.String == "" {
		return false
	}
	if !preview.MatchesPattern(app.PreviewBranchPattern.String, branch) {
		return false
	}
	previewApp, err := h.ensurePreviewApp(r.Context(), app, branch)
	if err != nil {
		slog.Error("webhook: failed to materialize preview app", "parent", app.Name, "branch", branch, "error", err)
		return false
	}
	return h.triggerPushDeploy(r, previewApp, event)
}
