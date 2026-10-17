package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/pkg/crypto"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
)

var validGitProviders = map[string]bool{"github": true, "gitlab": true, "bitbucket": true, "gitea": true}

const (
	githubManifestStatePrefix = "gh_app_manifest:"
	manifestStateTTL          = 10 * time.Minute
)

type providerConfigResponse struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	ClientID  string `json:"client_id"`
	AppID     string `json:"app_id"`
	AppSlug   string `json:"app_slug"`
	HasSecret bool   `json:"has_secret"`
	IsPublic  bool   `json:"is_public"`
	IsOwner   bool   `json:"is_owner"`
}

// ListGitProviderConfigs returns all configured provider apps (without secrets).
//
//apidoc:tag platform/git/providers
//apidoc:title Get Providers
//apidoc:order 1
func (h *Handler) ListGitProviderConfigs(w http.ResponseWriter, r *http.Request) {
	rows, err := h.gitProviderSvc.List(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list provider configs", err)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	result := make([]providerConfigResponse, 0, len(rows))
	for _, c := range rows {
		result = append(result, providerConfigResponse{
			ID:        uuidToString(c.ID),
			Provider:  c.Provider,
			BaseURL:   c.BaseUrl,
			ClientID:  c.ClientID,
			AppID:     c.AppID,
			AppSlug:   c.AppSlug,
			HasSecret: c.HasSecret.Bool,
			IsPublic:  c.IsPublic,
			IsOwner:   c.CreatedBy.Valid && uuidToString(c.CreatedBy) == userID,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

type saveGitProviderConfigRequest struct {
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	ClientID      string `json:"client_id"`
	AppID         string `json:"app_id"`
	AppSlug       string `json:"app_slug"`
	ClientSecret  string `json:"client_secret"`  // empty = preserve existing
	PrivateKey    string `json:"private_key"`    // GitHub App only
	WebhookSecret string `json:"webhook_secret"` // GitHub App only
	IsPublic      bool   `json:"is_public"`
}

// SaveGitProviderConfig upserts a provider app config from manual admin entry.
//
//apidoc:tag platform/git/providers
//apidoc:title Update Provider
//apidoc:order 2
func (h *Handler) SaveGitProviderConfig(w http.ResponseWriter, r *http.Request) {
	var req saveGitProviderConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validGitProviders[req.Provider] {
		writeError(w, http.StatusBadRequest, "provider must be one of: github, gitlab, bitbucket, gitea")
		return
	}

	secret := h.mergeProviderSecret(r.Context(), req)

	var owner pgtype.UUID
	_ = owner.Scan(middleware.UserIDFromContext(r.Context()))

	cfg, err := h.gitProviderSvc.Save(r.Context(), service.SaveGitProviderConfigParams{
		Provider: req.Provider,
		BaseURL:  req.BaseURL,
		ClientID: req.ClientID,
		AppID:    req.AppID,
		AppSlug:  req.AppSlug,
		Secret:   secret,
		OwnerID:  owner,
		IsPublic: req.IsPublic,
	})
	if err != nil {
		writeServerError(w, r, "failed to save provider config", err)
		return
	}

	h.audit(r, "save_git_provider_config", "git_provider_config", uuidToString(cfg.ID), map[string]any{"provider": req.Provider, "base_url": req.BaseURL, "is_public": req.IsPublic})
	writeJSON(w, http.StatusOK, providerConfigResponse{
		ID:        uuidToString(cfg.ID),
		Provider:  cfg.Provider,
		BaseURL:   cfg.BaseUrl,
		ClientID:  cfg.ClientID,
		AppID:     cfg.AppID,
		AppSlug:   cfg.AppSlug,
		HasSecret: len(cfg.SecretEncrypted) > 0,
		IsPublic:  cfg.IsPublic,
		IsOwner:   true,
	})
}

// mergeProviderSecret overlays any provided secret fields onto the existing
// stored secret so rotating one field does not wipe the others. Returns nil
// when no secret fields are provided (preserve existing).
func (h *Handler) mergeProviderSecret(ctx context.Context, req saveGitProviderConfigRequest) *service.ProviderSecret {
	if req.ClientSecret == "" && req.PrivateKey == "" && req.WebhookSecret == "" {
		return nil
	}
	merged := service.ProviderSecret{}
	if _, existing, err := h.gitProviderSvc.Get(ctx, req.Provider, req.BaseURL); err == nil {
		merged = existing
	}
	if req.ClientSecret != "" {
		merged.ClientSecret = req.ClientSecret
	}
	if req.PrivateKey != "" {
		merged.PrivateKey = req.PrivateKey
	}
	if req.WebhookSecret != "" {
		merged.WebhookSecret = req.WebhookSecret
	}
	return &merged
}

// DeleteGitProviderConfig removes a provider app config.
//
//apidoc:tag platform/git/providers
//apidoc:title Delete Provider
//apidoc:order 3
func (h *Handler) DeleteGitProviderConfig(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "configId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid config id")
		return
	}
	if err := h.gitProviderSvc.Delete(r.Context(), uuid); err != nil {
		writeServerError(w, r, "failed to delete provider config", err)
		return
	}
	h.audit(r, "delete_git_provider_config", "git_provider_config", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- GitHub App manifest flow ---------------------------------------------

// GetGitHubAppManifest returns a GitHub App manifest plus a one-time state nonce
// and the URL the browser should POST the manifest to. The admin's browser then
// submits the manifest form to GitHub, which creates the App and redirects to
// our public callback with a temporary code.
//
//apidoc:tag platform/git/providers
//apidoc:title Get GitHub Manifest
//apidoc:order 4
func (h *Handler) GetGitHubAppManifest(w http.ResponseWriter, r *http.Request) {
	// Resolve the public origin the same way the OAuth connect flow does:
	// PUBLIC_BASE_URL when set, otherwise the configured dashboard domain (over
	// https). A production install rarely sets PUBLIC_BASE_URL — it configures a
	// dashboard domain instead — so requiring the env var here made GitHub App
	// creation impossible on a normal VPS. GitHub still needs a valid absolute URL
	// for the manifest's redirect/callback URLs, so validate whatever we resolved.
	base, err := h.publicBaseURL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if u, perr := url.Parse(base); perr != nil || u.Scheme == "" || u.Host == "" {
		writeError(w, http.StatusBadRequest,
			"the configured public URL is not a valid absolute URL — set PUBLIC_BASE_URL, or the dashboard domain, correctly before creating a GitHub App")
		return
	}

	// Public visibility makes the App installable by any GitHub account (not just
	// the owner) — required for it to be a shared/public provider.
	isPublic := r.URL.Query().Get("public") == "true"

	state, err := crypto.GenerateWebhookSecret()
	if err != nil {
		writeServerError(w, r, "failed to generate state", err)
		return
	}
	// Carry the initiating admin and chosen visibility through the (public)
	// manifest callback, which has no session of its own.
	statePayload, _ := json.Marshal(manifestState{
		UserID:   middleware.UserIDFromContext(r.Context()),
		IsPublic: isPublic,
	})
	if err := h.rdb.Set(r.Context(), githubManifestStatePrefix+state, statePayload, manifestStateTTL).Err(); err != nil {
		writeServerError(w, r, "failed to persist state", err)
		return
	}

	manifest := map[string]any{
		"name":         githubAppName(h.instanceName(r.Context())),
		"url":          base,
		"redirect_url": base + "/api/git/providers/github/manifest/callback",
		// setup_url receives the post-install redirect (with installation_id +
		// state), which our unified connect callback turns into a connection.
		"setup_url":       base + "/api/git/integrations/callback",
		"setup_on_update": true,
		"callback_urls": []string{
			base + "/api/git/integrations/callback",
		},
		"public": isPublic,
		"default_permissions": map[string]string{
			"contents": "read",
			"metadata": "read",
		},
		"default_events": []string{"push"},
	}

	// GitHub validates the webhook URL for public reachability at App-creation
	// time and rejects localhost/private hosts. Only advertise a hook URL when
	// it's publicly reachable; otherwise omit it so the App can still be created
	// in local dev (webhooks can be added later in the App settings or via a
	// WEBHOOK_PUBLIC_URL tunnel). webhookConfigured tells the UI which happened.
	webhookBase := h.cfg.WebhookPublicURL
	if webhookBase == "" {
		webhookBase = base
	}
	webhookConfigured := false
	if publiclyReachableURL(webhookBase) {
		manifest["hook_attributes"] = map[string]any{
			"url": webhookBase + "/api/git/webhooks/github",
		}
		webhookConfigured = true
	}

	// org is optional; when set, the App is created under the organization.
	postURL := "https://github.com/settings/apps/new"
	if org := r.URL.Query().Get("org"); org != "" {
		postURL = fmt.Sprintf("https://github.com/organizations/%s/settings/apps/new", org)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"post_url":           postURL,
		"state":              state,
		"manifest":           manifest,
		"webhook_configured": webhookConfigured,
	})
}

// publiclyReachableURL reports whether raw is an absolute http(s) URL whose host
// GitHub (and other providers) can reach over the public Internet. Loopback,
// .local, and RFC1918/link-local/ULA private addresses are treated as not
// reachable so we omit webhook URLs that the provider would reject.
func publiclyReachableURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return false
		}
	}
	return true
}

// githubAppName sanitizes an instance name for use as a GitHub App name.
// GitHub requires the name be globally unique and caps its length, so we
// collapse whitespace and cap to 34 chars; the manifest page still lets the
// admin edit it (and resolve any global collision) before creating.
func githubAppName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = defaultInstanceName
	}
	if len(name) > 34 {
		name = strings.TrimSpace(name[:34])
	}
	return name
}

type githubManifestConversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// manifestState is the JSON payload stored under the one-time manifest state
// nonce so the public callback can recover the initiating admin and the chosen
// visibility (it carries no session of its own).
type manifestState struct {
	UserID   string `json:"user_id"`
	IsPublic bool   `json:"is_public"`
}

// HandleGitHubAppManifestCallback is the public redirect target GitHub sends the
// admin's browser to after creating the App. It is guarded by the one-time state
// nonce (not the JWT, since a top-level redirect carries no Authorization
// header). It exchanges the code for the App credentials and stores them.
func (h *Handler) HandleGitHubAppManifestCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		writeError(w, http.StatusBadRequest, "missing code or state")
		return
	}

	// Validate and consume the one-time state, recovering the owner + visibility.
	raw, err := h.rdb.GetDel(ctx, githubManifestStatePrefix+state).Result()
	if err != nil || raw == "" {
		writeError(w, http.StatusBadRequest, "invalid or expired state")
		return
	}
	var st manifestState
	_ = json.Unmarshal([]byte(raw), &st)
	var owner pgtype.UUID
	_ = owner.Scan(st.UserID)

	conv, err := convertGitHubManifest(ctx, code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to convert manifest: "+err.Error())
		return
	}

	if _, err := h.gitProviderSvc.Save(ctx, service.SaveGitProviderConfigParams{
		Provider: "github",
		BaseURL:  "",
		ClientID: conv.ClientID,
		AppID:    fmt.Sprintf("%d", conv.ID),
		AppSlug:  conv.Slug,
		Secret: &service.ProviderSecret{
			ClientSecret:  conv.ClientSecret,
			PrivateKey:    conv.PEM,
			WebhookSecret: conv.WebhookSecret,
		},
		OwnerID:  owner,
		IsPublic: st.IsPublic,
	}); err != nil {
		writeServerError(w, r, "failed to store provider config", err)
		return
	}

	http.Redirect(w, r, h.cfg.PublicBaseURL+"/git?tab=providers&connected=github", http.StatusFound)
}

// convertGitHubManifest exchanges a manifest code for App credentials.
func convertGitHubManifest(ctx context.Context, code string) (*githubManifestConversion, error) {
	url := fmt.Sprintf("https://api.github.com/app-manifests/%s/conversions", code)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned status %d", resp.StatusCode)
	}

	var conv githubManifestConversion
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		return nil, fmt.Errorf("decode conversion: %w", err)
	}
	return &conv, nil
}
