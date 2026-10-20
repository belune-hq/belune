package handler

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/server/middleware"
)

const (
	gitConnectStatePrefix = "git_connect:"
	gitConnectStateCookie = "git_connect_state"
	connectStateTTL       = 10 * time.Minute
)

type connectState struct {
	UserID   string `json:"user_id"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
}

type gitIntegrationResponse struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url"`
	AccountLogin string `json:"account_login"`
	CreatedAt    string `json:"created_at"`
}

// ListGitIntegrations returns the current user's connected provider accounts.
//
//apidoc:tag platform/git/integrations
//apidoc:title Get Integrations
func (h *Handler) ListGitIntegrations(w http.ResponseWriter, r *http.Request) {
	var userID pgtype.UUID
	if err := userID.Scan(middleware.UserIDFromContext(r.Context())); err != nil {
		writeServerError(w, r, "invalid user id", err)
		return
	}
	rows, err := h.gitIntegrationSvc.ListByUser(r.Context(), userID)
	if err != nil {
		writeServerError(w, r, "failed to list connections", err)
		return
	}
	result := make([]gitIntegrationResponse, 0, len(rows))
	for _, row := range rows {
		result = append(result, gitIntegrationResponse{
			ID:           uuidToString(row.ID),
			Provider:     row.Provider,
			BaseURL:      row.BaseUrl,
			AccountLogin: row.AccountLogin,
			CreatedAt:    row.CreatedAt.Time.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, result)
}

type availableProviderResponse struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	AppSlug  string `json:"app_slug"`
}

// ListAvailableProviders returns the configured providers a user can connect to.
//
//apidoc:tag platform/git/integrations
//apidoc:title Get Available Providers
func (h *Handler) ListAvailableProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.gitProviderSvc.List(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list providers", err)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	result := make([]availableProviderResponse, 0, len(rows))
	for _, c := range rows {
		if !c.HasSecret.Bool {
			continue
		}
		// Visibility: public providers are offered to everyone; private ones
		// only to the admin who created them.
		if !c.IsPublic && !(c.CreatedBy.Valid && uuidToString(c.CreatedBy) == userID) {
			continue
		}
		result = append(result, availableProviderResponse{
			Provider: c.Provider,
			BaseURL:  c.BaseUrl,
			AppSlug:  c.AppSlug,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ListIntegrationRepos returns the repositories accessible to a connection.
//
//apidoc:tag platform/git/integrations
//apidoc:title Get Repos
func (h *Handler) ListIntegrationRepos(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "integrationId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid integration id")
		return
	}
	if !h.canAccessIntegration(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	repos, err := h.gitIntegrationSvc.ListRepos(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to list repositories")
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

// ListIntegrationBranches returns the branches of a repo for a connection.
//
//apidoc:tag platform/git/integrations
//apidoc:title Get Repo Branches
func (h *Handler) ListIntegrationBranches(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "integrationId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid integration id")
		return
	}
	if !h.canAccessIntegration(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, "repo is required")
		return
	}
	branches, err := h.gitIntegrationSvc.ListBranches(r.Context(), uuid, repo)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to list branches")
		return
	}
	writeJSON(w, http.StatusOK, branches)
}

// StartGitIntegrationConnect generates a one-time state, stores the connect
// context, and returns the provider auth URL for the browser to navigate to.
//
//apidoc:tag platform/git/integrations
func (h *Handler) StartGitIntegrationConnect(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	baseURL := r.URL.Query().Get("base_url")
	if !validGitProviders[provider] {
		writeError(w, http.StatusBadRequest, "invalid provider")
		return
	}

	state, err := crypto.GenerateWebhookSecret()
	if err != nil {
		writeServerError(w, r, "failed to generate state", err)
		return
	}
	payload, _ := json.Marshal(connectState{
		UserID:   middleware.UserIDFromContext(r.Context()),
		Provider: provider,
		BaseURL:  baseURL,
	})
	if err := h.rdb.Set(r.Context(), gitConnectStatePrefix+state, payload, connectStateTTL).Err(); err != nil {
		writeServerError(w, r, "failed to persist state", err)
		return
	}

	// Bind the state to this browser: the public callback requires this cookie
	// to match the state query param, so a stolen/forged state alone is useless.
	http.SetCookie(w, &http.Cookie{
		Name:     gitConnectStateCookie,
		Value:    state,
		Path:     "/api/git/integrations/callback",
		HttpOnly: true,
		Secure:   h.secureCookies(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(connectStateTTL.Seconds()),
	})

	base, err := h.publicBaseURL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	redirectURI := base + "/api/git/integrations/callback"
	authURL, err := h.gitIntegrationSvc.AuthURL(r.Context(), provider, baseURL, redirectURI, state)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": authURL})
}

// HandleGitIntegrationCallback is the public provider redirect target. It is
// guarded by the one-time state nonce (a top-level redirect carries no
// Authorization header), completes the connect flow, and redirects to the UI.
func (h *Handler) HandleGitIntegrationCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := r.URL.Query().Get("state")
	if state == "" {
		writeError(w, http.StatusBadRequest, "missing state")
		return
	}

	// The state must match the browser-bound cookie set when the flow started.
	// Clear the cookie regardless of outcome.
	cookie, cookieErr := r.Cookie(gitConnectStateCookie)
	http.SetCookie(w, &http.Cookie{
		Name: gitConnectStateCookie, Value: "", Path: "/api/git/integrations/callback",
		HttpOnly: true, Secure: h.secureCookies(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	if cookieErr != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		writeError(w, http.StatusBadRequest, "state cookie mismatch")
		return
	}

	raw, err := h.rdb.GetDel(ctx, gitConnectStatePrefix+state).Result()
	if err != nil || raw == "" {
		writeError(w, http.StatusBadRequest, "invalid or expired state")
		return
	}
	var st connectState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		writeServerError(w, r, "corrupt state", err)
		return
	}
	var userID pgtype.UUID
	if err := userID.Scan(st.UserID); err != nil {
		writeServerError(w, r, "invalid user id", err)
		return
	}

	// Must match the redirect_uri sent at the authorize step byte-for-byte, which
	// is why both sides resolve it the same way. It cannot have changed between the
	// two: the source is operator config, not the request.
	base, err := h.publicBaseURL(r)
	if err != nil {
		writeServerError(w, r, "cannot resolve callback base URL", err)
		return
	}
	redirectURI := base + "/api/git/integrations/callback"
	if _, err := h.gitIntegrationSvc.Connect(ctx, st.Provider, st.BaseURL, redirectURI, r.URL.Query(), userID); err != nil {
		http.Redirect(w, r, base+"/git?tab=connections&error="+st.Provider, http.StatusFound)
		return
	}
	http.Redirect(w, r, base+"/git?tab=connections&connected="+st.Provider, http.StatusFound)
}

// DeleteGitIntegration disconnects a connected account.
//
//apidoc:tag platform/git/integrations
//apidoc:title Delete Integration
func (h *Handler) DeleteGitIntegration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "integrationId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid integration id")
		return
	}
	// Ownership: only the creator (or an admin) may disconnect.
	if !h.canAccessIntegration(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	if err := h.gitIntegrationSvc.Delete(r.Context(), uuid); err != nil {
		writeServerError(w, r, "failed to delete connection", err)
		return
	}
	h.audit(r, "delete_git_integration", "git_integration", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
