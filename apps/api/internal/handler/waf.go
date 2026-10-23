package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/waf"
)

// WAFStatsProvider is the slice of the WAF recorder the status endpoint reads.
type WAFStatsProvider interface {
	Stats() waf.Stats
}

// SetWAF wires the WAF recorder's counters into the status endpoint.
func (h *Handler) SetWAF(p WAFStatsProvider) {
	h.waf = p
}

const (
	// wafModeDetectionOnly is the only mode there is: nothing is blocked.
	wafModeDetectionOnly = "detection_only"

	// wafSummaryWindow is the period the totals and the top lists cover.
	wafSummaryWindow = 24 * time.Hour
	wafTopN          = 5

	wafEventsDefaultLimit = 50
	wafEventsMaxLimit     = 200
)

type wafStatusResponse struct {
	// Capability is whether Caddy has the module: available, unavailable, or
	// unknown when Belune could not list its modules. The three are different
	// answers and the page must not collapse them.
	Capability featureCapability `json:"capability"`
	// Enabled is the operator's switch.
	Enabled bool `json:"enabled"`
	// Mode is always detection_only for now.
	Mode string `json:"mode"`
	// State is what the reconciler last did about it: whether the route is in Caddy,
	// how many hosts it covers, and why not when it is switched on but inactive.
	State proxy.WAFState `json:"state"`
	// Recorder is what the recorder has done since Belune started, including the two
	// ways an event is lost without a row: dropped under load, and unparseable.
	Recorder      waf.Stats  `json:"recorder"`
	RetentionDays int        `json:"retention_days"`
	MaxEvents     int        `json:"max_events"`
	Summary       wafSummary `json:"summary"`
}

type wafSummary struct {
	WindowHours int            `json:"window_hours"`
	Events      int64          `json:"events"`
	Requests    int64          `json:"requests"`
	Clients     int64          `json:"clients"`
	TopRules    []wafTopRule   `json:"top_rules"`
	TopClients  []wafTopClient `json:"top_clients"`
}

type wafTopRule struct {
	RuleID   int    `json:"rule_id"`
	Message  string `json:"message"`
	Category string `json:"category"`
	Events   int64  `json:"events"`
}

type wafTopClient struct {
	ClientIP string    `json:"client_ip"`
	Events   int64     `json:"events"`
	LastSeen time.Time `json:"last_seen"`
}

type wafEventResponse struct {
	ID         int64     `json:"id"`
	RecordedAt time.Time `json:"recorded_at"`
	ClientIP   string    `json:"client_ip"`
	RuleID     int       `json:"rule_id"`
	Severity   string    `json:"severity"`
	Category   *string   `json:"category"`
	Message    string    `json:"message"`
	// Host and ApplicationID are null when attribution failed for this event.
	Host          *string `json:"host"`
	ApplicationID *string `json:"application_id"`
	// TxID is shared by the rules one request tripped.
	TxID *string `json:"tx_id"`
}

type wafEventsResponse struct {
	Events []wafEventResponse `json:"events"`
	// NextBefore is the id to pass as `before` for the next page; null on the last.
	NextBefore *int64 `json:"next_before"`
}

type updateWAFRequest struct {
	Enabled *bool `json:"enabled"`
}

// GetWAFStatus reports the WAF: whether Caddy has the module, whether it is on,
// what it is doing, and a summary of what it has seen.
// GET /api/maintenance/proxy/waf (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get WAF Status
//apidoc:order 11
func (h *Handler) GetWAFStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.wafStatus(r)
	if err != nil {
		writeServerError(w, r, "failed to read the WAF status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// UpdateWAF turns the WAF on or off for the whole install. It is detection only:
// turning it on records what the rules would have blocked and blocks nothing.
// PUT /api/maintenance/proxy/waf (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Update WAF
//apidoc:order 12
func (h *Handler) UpdateWAF(w http.ResponseWriter, r *http.Request) {
	var req updateWAFRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled (true or false) is required")
		return
	}

	// Only a Caddy known to lack the module refuses. When Belune could not ask, the
	// switch is stored and takes effect once it can: refusing on "unknown" would
	// tell someone their image lacks a module it may well have.
	if *req.Enabled && featureState(h.capabilitySet(r), capability.ModuleWAF) == featureUnavailable {
		writeError(w, http.StatusConflict,
			"this Caddy does not include the WAF module ("+capability.ModuleWAF+"); use the Belune Caddy image")
		return
	}

	if _, err := h.queries.UpsertSetting(r.Context(), generated.UpsertSettingParams{
		Key: proxy.SettingWAFEnabled, Value: strconv.FormatBool(*req.Enabled),
	}); err != nil {
		writeServerError(w, r, "failed to save the WAF setting", err)
		return
	}

	action := "disable_waf"
	if *req.Enabled {
		action = "enable_waf"
	}
	h.audit(r, action, "proxy", "", map[string]any{"mode": wafModeDetectionOnly})

	// Apply it now rather than on the next 30-second pass, so the answer below is
	// the state the proxy is actually in. A failed pass is not a failed request:
	// the switch is saved, and the status says what the proxy did about it.
	//
	// Bounded below the route's 15-second ceiling, which this request also has to
	// fit the status queries into: a reconcile that outran it would turn a saved
	// switch into a 503.
	if h.reconciler != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := h.reconciler.ReconcileNow(ctx); err != nil {
			slog.Warn("proxy reconcile after a WAF change reported errors", "error", err)
		}
	}

	st, err := h.wafStatus(r)
	if err != nil {
		writeServerError(w, r, "failed to read the WAF status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ListWAFEvents pages through detection events, newest first.
// GET /api/maintenance/proxy/waf/events (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List WAF Events
//apidoc:order 13
func (h *Handler) ListWAFEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := wafEventsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > wafEventsMaxLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(wafEventsMaxLimit))
			return
		}
		limit = n
	}

	// One more than asked for, to learn whether there is another page without a
	// second query.
	params := generated.ListWAFEventsParams{RowLimit: int32(limit + 1)}
	if v := q.Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "before must be an event id")
			return
		}
		params.BeforeID = pgtype.Int8{Int64: id, Valid: true}
	}
	if v := q.Get("client_ip"); v != "" {
		ip, err := netip.ParseAddr(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "client_ip must be an IP address")
			return
		}
		ip = ip.Unmap()
		params.ClientIp = &ip
	}
	if v := q.Get("rule_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 32)
		if err != nil || id < 0 {
			writeError(w, http.StatusBadRequest, "rule_id must be a rule number")
			return
		}
		params.RuleID = pgtype.Int4{Int32: int32(id), Valid: true}
	}

	rows, err := h.queries.ListWAFEvents(r.Context(), params)
	if err != nil {
		writeServerError(w, r, "failed to list WAF events", err)
		return
	}

	resp := wafEventsResponse{Events: make([]wafEventResponse, 0, len(rows))}
	if len(rows) > limit {
		rows = rows[:limit]
		next := rows[len(rows)-1].ID
		resp.NextBefore = &next
	}
	for _, e := range rows {
		resp.Events = append(resp.Events, wafEventDTO(e))
	}
	writeJSON(w, http.StatusOK, resp)
}

func wafEventDTO(e generated.WafEvent) wafEventResponse {
	return wafEventResponse{
		ID:            e.ID,
		RecordedAt:    e.RecordedAt.Time,
		ClientIP:      e.ClientIp.String(),
		RuleID:        int(e.RuleID),
		Severity:      e.Severity,
		Category:      textPtr(e.Category),
		Message:       e.Message,
		Host:          textPtr(e.Host),
		ApplicationID: uuidPtr(e.ApplicationID),
		TxID:          textPtr(e.TxID),
	}
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func uuidPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuidToString(u)
	return &s
}

// wafStatus gathers the status response.
func (h *Handler) wafStatus(r *http.Request) (wafStatusResponse, error) {
	ctx := r.Context()
	resp := wafStatusResponse{
		Capability: featureCapability{
			Module: capability.ModuleWAF,
			State:  featureState(h.capabilitySet(r), capability.ModuleWAF),
		},
		Mode:          wafModeDetectionOnly,
		RetentionDays: waf.RetentionDays(ctx, h.queries),
		MaxEvents:     waf.MaxEvents,
	}

	s, err := h.queries.GetSetting(ctx, proxy.SettingWAFEnabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return resp, err
	default:
		resp.Enabled = s.Value == "true"
	}

	if h.reconciler != nil {
		resp.State = h.reconciler.WAF()
	}
	if h.waf != nil {
		resp.Recorder = h.waf.Stats()
	}

	since := pgtype.Timestamptz{Time: time.Now().Add(-wafSummaryWindow), Valid: true}
	totals, err := h.queries.WAFEventTotals(ctx, since)
	if err != nil {
		return resp, err
	}
	rules, err := h.queries.TopWAFRules(ctx, generated.TopWAFRulesParams{Since: since, RowLimit: wafTopN})
	if err != nil {
		return resp, err
	}
	clients, err := h.queries.TopWAFClients(ctx, generated.TopWAFClientsParams{Since: since, RowLimit: wafTopN})
	if err != nil {
		return resp, err
	}

	resp.Summary = wafSummary{
		WindowHours: int(wafSummaryWindow / time.Hour),
		Events:      totals.Events,
		Requests:    totals.Requests,
		Clients:     totals.Clients,
		TopRules:    make([]wafTopRule, 0, len(rules)),
		TopClients:  make([]wafTopClient, 0, len(clients)),
	}
	for _, rule := range rules {
		resp.Summary.TopRules = append(resp.Summary.TopRules, wafTopRule{
			RuleID: int(rule.RuleID), Message: rule.Message, Category: rule.Category, Events: rule.Events,
		})
	}
	for _, c := range clients {
		resp.Summary.TopClients = append(resp.Summary.TopClients, wafTopClient{
			ClientIP: c.ClientIp.String(), Events: c.Events, LastSeen: c.LastSeen.Time,
		})
	}
	return resp, nil
}
