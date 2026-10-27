package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
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

	// wafDefaultWindow is the period the totals and the top lists cover when the
	// caller does not ask for another. It is also what a PUT reports back, and it
	// is what every client got before the window was selectable, so it must stay
	// 24h: widening the default would silently change an existing caller's numbers.
	wafDefaultWindow = "24h"
	wafTopN          = 5

	wafEventsDefaultLimit = 50
	wafEventsMaxLimit     = 200

	// wafClientsDefaultLimit caps the grouped list. It is a summary, not an audit
	// trail: under a distributed scan the distinct-client count runs to thousands,
	// and the loudest 50 are the actionable set. The response says when more
	// matched rather than paginating an aggregate.
	wafClientsDefaultLimit = 50
	wafClientsMaxLimit     = 200
)

// wafWindows are the periods the summary can cover, in the order the dashboard
// offers them. A zero duration means every retained event rather than a period.
//
// "all" is not unbounded in practice: the sweep drops events past the retention
// and trims the table to MaxEvents, so it means "everything still kept", which is
// what the dashboard says.
var wafWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"all": 0,
}

// wafSummarySince is the timestamp the aggregates filter on. For "all" it is a
// floor no event can predate rather than a NULL, because the queries take `since`
// as a plain argument.
func wafSummarySince(window string, now time.Time) time.Time {
	if d := wafWindows[window]; d > 0 {
		return now.Add(-d)
	}
	return time.Unix(0, 0).UTC()
}

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
	// Window is the period these numbers cover: 1h, 24h, 7d or all.
	Window string `json:"window"`
	// WindowHours is the same period in hours, 0 for "all". Kept because it shipped
	// in v0.2.4, before the window could be chosen.
	WindowHours int   `json:"window_hours"`
	Events      int64 `json:"events"`
	Requests    int64 `json:"requests"`
	Clients     int64 `json:"clients"`
	// Total is every event still kept, whatever the window. Without it a dashboard
	// showing an empty window cannot tell "nothing has ever been detected" from
	// "nothing in the last day", and the second looks like the first.
	Total      int64          `json:"total"`
	TopRules   []wafTopRule   `json:"top_rules"`
	TopClients []wafTopClient `json:"top_clients"`
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
	// MatchedTarget is the request variable the rule matched in, as a name —
	// "REQUEST_COOKIES:session", "ARGS:q" — and never the value found in it.
	// Null for a rule that matches on the request as a whole, and for every
	// detection recorded before Belune kept it (000074). ⚠️ A hint, not
	// evidence: the text it is read from can be influenced by the request, so
	// the name can be forged — see waf.matchedTarget.
	MatchedTarget *string `json:"matched_target"`
	// Request is what the request looked like, joined from the request log by
	// TxID. Null is the normal case for an older detection: detections outlive
	// request logs, and an event that never carried a tx_id has nothing to join.
	Request *wafRequestDetail `json:"request"`
}

// wafRequestDetail is the request behind a detection, as far as Belune keeps it.
// Not the matched payload and not the headers at large — the URI is already
// redacted at Caddy's encoder and Content-Type is an allowlisted header, which is
// the whole of what is stored (000073).
type wafRequestDetail struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Status is 0 when the access entry carried one outside 100-599.
	Status      int     `json:"status"`
	ContentType *string `json:"content_type"`
	UserAgent   *string `json:"user_agent"`
}

// wafClientGroup is one client's detections in the summary window, collapsed.
//
// Events and Requests are deliberately both here: one request can trip a dozen
// rules, so "12 detections" alone reads as twelve attacks when it was one.
type wafClientGroup struct {
	ClientIP string `json:"client_ip"`
	// Events is rule matches; Requests is how many requests they came from.
	Events   int64 `json:"events"`
	Requests int64 `json:"requests"`
	Rules    int64 `json:"rules"`
	// URLs is distinct paths, counted through the request log. It is 0 — not
	// wrong — for detections whose request logs have been swept, since the URI is
	// never stored on the detection itself.
	URLs     int64     `json:"urls"`
	LastSeen time.Time `json:"last_seen"`
	// WouldBlock is whether any of these requests tripped the anomaly-score rule,
	// which is the only signal that separates a real attack from rule noise.
	WouldBlock bool `json:"would_block"`
}

type wafClientsResponse struct {
	// Window is the period these groups cover: 1h, 24h, 7d or all.
	Window  string           `json:"window"`
	Clients []wafClientGroup `json:"clients"`
	// HasMore is whether more clients matched than are listed. There is no page
	// after this one: narrowing the window or the filters is the way through.
	HasMore bool `json:"has_more"`
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
	window, ok := wafWindowParam(r.URL.Query())
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be one of 1h, 24h, 7d, all")
		return
	}

	st, err := h.wafStatus(r, window)
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

	st, err := h.wafStatus(r, wafDefaultWindow)
	if err != nil {
		writeServerError(w, r, "failed to read the WAF status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// wafWindowParam reads the summary period. An absent window is the default one,
// and the second return says whether what was asked for is a period at all.
func wafWindowParam(q url.Values) (string, bool) {
	v := q.Get("window")
	if v == "" {
		return wafDefaultWindow, true
	}
	_, ok := wafWindows[v]
	return v, ok
}

func wafLimitParam(q url.Values, def, max int) (int, bool) {
	v := q.Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// wafClientIPParam unmaps the address, so ::ffff:203.0.113.9 and 203.0.113.9 are
// the one client they are.
func wafClientIPParam(q url.Values) (*netip.Addr, bool) {
	v := q.Get("client_ip")
	if v == "" {
		return nil, true
	}
	ip, err := netip.ParseAddr(v)
	if err != nil {
		return nil, false
	}
	ip = ip.Unmap()
	return &ip, true
}

func wafRuleIDParam(q url.Values) (pgtype.Int4, bool) {
	v := q.Get("rule_id")
	if v == "" {
		return pgtype.Int4{}, true
	}
	id, err := strconv.ParseInt(v, 10, 32)
	if err != nil || id < 0 {
		return pgtype.Int4{}, false
	}
	return pgtype.Int4{Int32: int32(id), Valid: true}, true
}

// ListWAFClients groups a window's detections by the client that caused them, so
// one scanner's thousand rule matches read as one line rather than twenty pages.
// GET /api/maintenance/proxy/waf/clients (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List WAF Clients
//apidoc:order 14
func (h *Handler) ListWAFClients(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	window, ok := wafWindowParam(q)
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be one of 1h, 24h, 7d, all")
		return
	}
	limit, ok := wafLimitParam(q, wafClientsDefaultLimit, wafClientsMaxLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(wafClientsMaxLimit))
		return
	}
	clientIP, ok := wafClientIPParam(q)
	if !ok {
		writeError(w, http.StatusBadRequest, "client_ip must be an IP address")
		return
	}
	ruleID, ok := wafRuleIDParam(q)
	if !ok {
		writeError(w, http.StatusBadRequest, "rule_id must be a rule number")
		return
	}

	rows, err := h.queries.WAFClientSummary(r.Context(), generated.WAFClientSummaryParams{
		AnomalyRule: waf.AnomalySummaryRule,
		Since:       pgtype.Timestamptz{Time: wafSummarySince(window, time.Now()), Valid: true},
		ClientIp:    clientIP,
		RuleID:      ruleID,
		// One more than asked for, to learn whether more clients matched without
		// counting them all.
		RowLimit: int32(limit + 1),
	})
	if err != nil {
		writeServerError(w, r, "failed to group WAF detections by client", err)
		return
	}

	resp := wafClientsResponse{Window: window, Clients: make([]wafClientGroup, 0, len(rows))}
	if len(rows) > limit {
		rows = rows[:limit]
		resp.HasMore = true
	}
	for _, c := range rows {
		resp.Clients = append(resp.Clients, wafClientGroup{
			ClientIP:   c.ClientIp.String(),
			Events:     c.Events,
			Requests:   c.Requests,
			Rules:      c.Rules,
			URLs:       c.Urls,
			LastSeen:   c.LastSeen.Time,
			WouldBlock: c.WouldBlock,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// ListWAFEvents pages through detection events, newest first.
// GET /api/maintenance/proxy/waf/events (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List WAF Events
//apidoc:order 13
func (h *Handler) ListWAFEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, ok := wafLimitParam(q, wafEventsDefaultLimit, wafEventsMaxLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(wafEventsMaxLimit))
		return
	}
	clientIP, ok := wafClientIPParam(q)
	if !ok {
		writeError(w, http.StatusBadRequest, "client_ip must be an IP address")
		return
	}
	ruleID, ok := wafRuleIDParam(q)
	if !ok {
		writeError(w, http.StatusBadRequest, "rule_id must be a rule number")
		return
	}

	// One more than asked for, to learn whether there is another page without a
	// second query.
	params := generated.ListWAFEventsParams{RowLimit: int32(limit + 1), ClientIp: clientIP, RuleID: ruleID}
	if v := q.Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			writeError(w, http.StatusBadRequest, "before must be an event id")
			return
		}
		params.BeforeID = pgtype.Int8{Int64: id, Valid: true}
	}
	// Unscoped unless a window is asked for: that is what callers got before this
	// list could be scoped, and widening it later would be the easy half of a
	// breaking change. Scoped, it agrees with the grouped counts it sits under.
	if q.Get("window") != "" {
		window, ok := wafWindowParam(q)
		if !ok {
			writeError(w, http.StatusBadRequest, "window must be one of 1h, 24h, 7d, all")
			return
		}
		params.Since = pgtype.Timestamptz{Time: wafSummarySince(window, time.Now()), Valid: true}
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

	details := h.wafRequestDetails(r, rows)
	for _, e := range rows {
		dto := wafEventDTO(e)
		if e.TxID.Valid {
			if d, ok := details[e.TxID.String]; ok {
				dto.Request = d
			}
		}
		resp.Events = append(resp.Events, dto)
	}
	writeJSON(w, http.StatusOK, resp)
}

// wafRequestDetails fetches the request behind each detection on this page, keyed
// by transaction id.
//
// Missing detail is not an error: it is what a detection older than the request
// log looks like, and the operator is better served by the detection without its
// request than by a 500. So a failed lookup is logged and returns nothing, and
// every event on the page renders with request: null.
func (h *Handler) wafRequestDetails(r *http.Request, rows []generated.WafEvent) map[string]*wafRequestDetail {
	ids := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, e := range rows {
		// The rules of one request share a tx_id, so a page of 50 events can be far
		// fewer requests — ask for each one once.
		if !e.TxID.Valid {
			continue
		}
		if _, dup := seen[e.TxID.String]; dup {
			continue
		}
		seen[e.TxID.String] = struct{}{}
		ids = append(ids, e.TxID.String)
	}
	if len(ids) == 0 {
		return nil
	}

	details, err := h.queries.ListRequestDetailsByUUID(r.Context(), ids)
	if err != nil {
		slog.WarnContext(r.Context(), "could not load request detail for WAF events", "error", err)
		return nil
	}

	out := make(map[string]*wafRequestDetail, len(details))
	for _, d := range details {
		if !d.RequestUuid.Valid {
			continue
		}
		out[d.RequestUuid.String] = &wafRequestDetail{
			Method:      d.Method,
			Path:        d.Path,
			Status:      int(d.StatusCode),
			ContentType: textPtr(d.ContentType),
			UserAgent:   textPtr(d.UserAgent),
		}
	}
	return out
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
		MatchedTarget: textPtr(e.MatchedTarget),
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

// wafStatus gathers the status response. The window selects the period the
// summary covers; the rest of the response does not depend on it.
func (h *Handler) wafStatus(r *http.Request, window string) (wafStatusResponse, error) {
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

	since := pgtype.Timestamptz{Time: wafSummarySince(window, time.Now()), Valid: true}
	totals, err := h.queries.WAFEventTotals(ctx, since)
	if err != nil {
		return resp, err
	}
	total, err := h.queries.CountWAFEvents(ctx)
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
		Window:      window,
		WindowHours: int(wafWindows[window] / time.Hour),
		Events:      totals.Events,
		Requests:    totals.Requests,
		Clients:     totals.Clients,
		Total:       total,
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
