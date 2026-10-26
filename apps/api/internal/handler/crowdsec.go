package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/server/middleware"
)

// SetCrowdSec wires the CrowdSec service into the handlers. Called once at startup,
// before the server accepts requests.
func (h *Handler) SetCrowdSec(s *crowdsec.Service) {
	h.crowdsec = s
}

// noteAdminLogin tells CrowdSec an admin has just logged in, so the address they
// came from stops being bannable. Never blocks and never fails the login.
func (h *Handler) noteAdminLogin(role, email, clientIP string) {
	if h.crowdsec == nil || role != "admin" {
		return
	}
	h.crowdsec.NoteAdminLogin(email, clientIP)
}

type crowdsecStatusResponse struct {
	// Capability is whether Caddy has the bouncer module: available, unavailable,
	// or unknown when Belune could not list its modules.
	Capability featureCapability `json:"capability"`
	// Enabled is the operator's switch.
	Enabled bool `json:"enabled"`
	// State is what the reconciler last did about it.
	State proxy.CrowdSecState `json:"state"`
	// LocalAPI is what the last look at the CrowdSec container found.
	LocalAPI crowdsec.Health `json:"local_api"`
	// YourIP is the address this request came from, as Caddy saw it.
	YourIP string `json:"your_ip"`
	// YourIPProtection is how that address stands against being banned: private
	// and loopback addresses can never be named by a ban, a public one is only
	// as safe as its allowlist entry (which expires), and when the local API
	// cannot be asked nothing is claimed. The page words what it says about the
	// address from this, and not from the switch: the switch being on does not
	// mean the address was allowlisted.
	YourIPProtection crowdsec.Protection     `json:"your_ip_protection"`
	Autoban          crowdsecAutobanResponse `json:"autoban"`
}

type crowdsecAutobanResponse struct {
	Policy crowdsec.Policy        `json:"policy"`
	Status crowdsec.AutobanStatus `json:"status"`
	// Categories are the WAF rule categories a policy may choose from.
	Categories []string `json:"categories"`
}

type updateCrowdSecRequest struct {
	Enabled *bool `json:"enabled"`
}

type crowdsecDecisionResponse struct {
	ID    int64  `json:"id"`
	Value string `json:"value"`
	// Scope is "ip" or "range".
	Scope string `json:"scope"`
	// Source is what made the ban: dashboard, waf, cli (cscli by hand) or crowdsec.
	Source    string    `json:"source"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type crowdsecDecisionsResponse struct {
	Decisions []crowdsecDecisionResponse `json:"decisions"`
}

type banRequest struct {
	Target   string `json:"target"`
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
}

type crowdsecAllowItemResponse struct {
	Value       string    `json:"value"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
}

type crowdsecAllowlistResponse struct {
	Items []crowdsecAllowItemResponse `json:"items"`
}

type allowRequest struct {
	Target   string `json:"target"`
	Duration string `json:"duration"`
	Note     string `json:"note"`
}

// GetCrowdSecStatus reports CrowdSec: whether Caddy has the module, whether it is
// on, whether the local API is answering, and what the automatic bans are doing.
// GET /api/maintenance/proxy/crowdsec (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get CrowdSec Status
//apidoc:order 14
func (h *Handler) GetCrowdSecStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	st, err := h.crowdsecStatus(r, false)
	if err != nil {
		writeServerError(w, r, "failed to read the CrowdSec status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// UpdateCrowdSec turns the CrowdSec bouncer on or off for the whole install.
// PUT /api/maintenance/proxy/crowdsec (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Update CrowdSec
//apidoc:order 15
func (h *Handler) UpdateCrowdSec(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	var req updateCrowdSecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled (true or false) is required")
		return
	}

	// Only a Caddy known to lack the module refuses. When Belune could not ask, the
	// switch is stored and takes effect once it can.
	if *req.Enabled && featureState(h.capabilitySet(r), capability.ModuleCrowdSec) == featureUnavailable {
		writeError(w, http.StatusConflict,
			"this Caddy does not include the CrowdSec module ("+capability.ModuleCrowdSec+"); use the Belune Caddy image")
		return
	}

	if _, err := h.crowdsec.SetEnabled(r.Context(), *req.Enabled, middleware.ClientIP(r)); err != nil {
		writeServerError(w, r, "failed to save the CrowdSec setting", err)
		return
	}

	action := "disable_crowdsec"
	if *req.Enabled {
		action = "enable_crowdsec"
	}
	h.audit(r, action, "proxy", "", nil)

	// Applied now, so the answer below is the state the proxy is in. Bounded below
	// the route's ceiling, like the WAF's: a reconcile that outran it would turn a
	// saved switch into a 503.
	if h.reconciler != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := h.reconciler.ReconcileNow(ctx); err != nil {
			slog.Warn("proxy reconcile after a CrowdSec change reported errors", "error", redact.Error(err.Error()))
		}
	}

	st, err := h.crowdsecStatus(r, true)
	if err != nil {
		writeServerError(w, r, "failed to read the CrowdSec status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ListCrowdSecDecisions lists the bans in force.
// GET /api/maintenance/proxy/crowdsec/decisions (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List CrowdSec Bans
//apidoc:order 16
func (h *Handler) ListCrowdSecDecisions(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	ds, err := h.crowdsec.Decisions(r.Context())
	if err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	resp := crowdsecDecisionsResponse{Decisions: make([]crowdsecDecisionResponse, 0, len(ds))}
	for _, d := range ds {
		resp.Decisions = append(resp.Decisions, crowdsecDecisionResponse{
			ID: d.ID, Value: d.Value, Scope: d.Scope,
			Source: crowdsec.ReasonSource(d.Reason, d.Origin), Reason: d.Reason,
			CreatedAt: d.CreatedAt, ExpiresAt: d.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateCrowdSecBan bans an address or range for a while.
// POST /api/maintenance/proxy/crowdsec/decisions (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Ban an Address
//apidoc:order 17
func (h *Handler) CreateCrowdSecBan(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	var req banRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A ban placed while the feature is off would be recorded and enforced by
	// nothing, which reads as a ban that does not work.
	if on, err := h.crowdsecEnabled(r.Context()); err != nil {
		writeServerError(w, r, "failed to read the CrowdSec setting", err)
		return
	} else if !on {
		writeError(w, http.StatusConflict, "CrowdSec is switched off; turn it on before banning")
		return
	}

	d, err := h.crowdsec.Ban(r.Context(), req.Target, req.Duration, req.Reason, middleware.ClientIP(r))
	if err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	h.audit(r, "crowdsec_ban", "crowdsec", d.Value, map[string]any{
		"duration": req.Duration, "reason": d.Reason,
	})
	writeJSON(w, http.StatusCreated, crowdsecDecisionResponse{
		Value: d.Value, Source: "dashboard", Reason: d.Reason, ExpiresAt: d.ExpiresAt,
	})
}

// DeleteCrowdSecBan lifts every ban on an address or range. Never refused for the
// feature being off: undoing a ban must always be possible.
// DELETE /api/maintenance/proxy/crowdsec/decisions?target=… (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Lift a Ban
//apidoc:order 18
func (h *Handler) DeleteCrowdSecBan(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	target := r.URL.Query().Get("target")
	if err := h.crowdsec.Unban(r.Context(), target); err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	h.audit(r, "crowdsec_unban", "crowdsec", target, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ListCrowdSecAllowlist lists the addresses CrowdSec will never ban.
// GET /api/maintenance/proxy/crowdsec/allowlist (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List CrowdSec Allowlist
//apidoc:order 19
func (h *Handler) ListCrowdSecAllowlist(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	items, err := h.crowdsec.Allowlist(r.Context())
	if err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	resp := crowdsecAllowlistResponse{Items: make([]crowdsecAllowItemResponse, 0, len(items))}
	for _, i := range items {
		resp.Items = append(resp.Items, crowdsecAllowItemResponse{
			Value: i.Value, Description: i.Description, CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateCrowdSecAllow adds an address or range to the allowlist. Allowlisting an
// address also lifts a ban already on it.
// POST /api/maintenance/proxy/crowdsec/allowlist (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Allowlist an Address
//apidoc:order 20
func (h *Handler) CreateCrowdSecAllow(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	var req allowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.crowdsec.AllowTarget(r.Context(), req.Target, req.Duration, req.Note); err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	h.audit(r, "crowdsec_allow", "crowdsec", req.Target, map[string]any{"duration": req.Duration})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteCrowdSecAllow removes an entry from the allowlist.
// DELETE /api/maintenance/proxy/crowdsec/allowlist?target=… (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Remove from the Allowlist
//apidoc:order 21
func (h *Handler) DeleteCrowdSecAllow(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	target := r.URL.Query().Get("target")
	if err := h.crowdsec.UnallowTarget(r.Context(), target); err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	h.audit(r, "crowdsec_unallow", "crowdsec", target, nil)
	w.WriteHeader(http.StatusNoContent)
}

// UpdateCrowdSecAutoban sets the policy for turning WAF detections into bans.
// PUT /api/maintenance/proxy/crowdsec/autoban (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Update Automatic Bans
//apidoc:order 22
func (h *Handler) UpdateCrowdSecAutoban(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	var p crowdsec.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cfg, err := h.crowdsec.SetAutoban(r.Context(), p)
	if err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	h.audit(r, "crowdsec_autoban_policy", "crowdsec", "", map[string]any{
		"enabled": cfg.Autoban.Enabled, "threshold": cfg.Autoban.Threshold,
		"window_minutes": cfg.Autoban.WindowMinutes, "duration_minutes": cfg.Autoban.DurationMinutes,
		"max_active": cfg.Autoban.MaxActive, "categories": cfg.Autoban.Categories,
	})

	st, err := h.crowdsecStatus(r, false)
	if err != nil {
		writeServerError(w, r, "failed to read the CrowdSec status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// RotateCrowdSecKey replaces the bouncer's key, for a bouncer that has stopped
// pulling the ban list.
// POST /api/maintenance/proxy/crowdsec/rotate-key (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Rotate the CrowdSec Bouncer Key
//apidoc:order 23
func (h *Handler) RotateCrowdSecKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireCrowdSec(w) {
		return
	}
	if err := h.crowdsec.RotateKey(r.Context()); err != nil {
		h.writeCrowdSecError(w, r, err)
		return
	}
	h.audit(r, "crowdsec_rotate_key", "crowdsec", "", nil)

	if h.reconciler != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := h.reconciler.ReconcileNow(ctx); err != nil {
			slog.Warn("proxy reconcile after a CrowdSec key rotation reported errors", "error", redact.Error(err.Error()))
		}
	}
	st, err := h.crowdsecStatus(r, true)
	if err != nil {
		writeServerError(w, r, "failed to read the CrowdSec status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// requireCrowdSec answers 503 when the service was never wired (a test server, or
// a build without it), instead of dereferencing nil.
func (h *Handler) requireCrowdSec(w http.ResponseWriter) bool {
	if h.crowdsec == nil {
		writeError(w, http.StatusServiceUnavailable, "CrowdSec is not available on this server")
		return false
	}
	return true
}

func (h *Handler) crowdsecEnabled(ctx context.Context) (bool, error) {
	cfg, err := h.crowdsec.Config(ctx)
	return cfg.Enabled, err
}

// crowdsecStatus gathers the status response. fresh forces a new look at the local
// API instead of reusing one a few seconds old.
func (h *Handler) crowdsecStatus(r *http.Request, fresh bool) (crowdsecStatusResponse, error) {
	ctx := r.Context()
	cfg, err := h.crowdsec.Config(ctx)
	if err != nil {
		return crowdsecStatusResponse{}, err
	}
	resp := crowdsecStatusResponse{
		Capability: featureCapability{
			Module: capability.ModuleCrowdSec,
			State:  featureState(h.capabilitySet(r), capability.ModuleCrowdSec),
		},
		Enabled: cfg.Enabled,
		YourIP:  middleware.ClientIP(r),
		Autoban: crowdsecAutobanResponse{
			Policy:     cfg.Autoban,
			Status:     h.crowdsec.Autoban(),
			Categories: crowdsec.BanCategories,
		},
	}
	if h.reconciler != nil {
		resp.State = h.reconciler.CrowdSec()
	}
	if fresh {
		resp.LocalAPI = h.crowdsec.Refresh(ctx)
	} else {
		resp.LocalAPI = h.crowdsec.Look(ctx)
	}
	// After the look above, which it reuses: a private address is answered without
	// asking the local API at all.
	resp.YourIPProtection = h.crowdsec.Protection(ctx, resp.YourIP)
	return resp, nil
}

// writeCrowdSecError maps what the CrowdSec side can say to an HTTP answer. What it
// does not recognise is logged with its detail and answered without it: cscli's own
// error text is not for an API client.
func (h *Handler) writeCrowdSecError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *crowdsec.InvalidError
	var notBannable *crowdsec.ErrNotBannable
	var cmd *crowdsec.CommandError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Error())
	case errors.As(err, &notBannable):
		writeError(w, http.StatusBadRequest, notBannable.Error())
	case errors.Is(err, crowdsec.ErrAllowlisted):
		writeError(w, http.StatusConflict,
			"that address is on the allowlist, so it cannot be banned. Remove it from the allowlist first if you really mean to")
	case errors.Is(err, crowdsec.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable,
			"the CrowdSec container is not running. After updating Belune, run the host update (docker compose up -d) so the crowdsec service exists")
	case errors.As(err, &cmd):
		slog.Warn("crowdsec: command failed", "command", cmd.Command, "exit", cmd.ExitCode, "stderr", redact.Error(cmd.Stderr))
		writeError(w, http.StatusBadGateway, "CrowdSec refused the request; see the server log")
	default:
		writeServerError(w, r, "CrowdSec request failed", err)
	}
}
