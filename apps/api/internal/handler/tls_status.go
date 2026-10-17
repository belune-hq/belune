package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/proxy"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/tlsstatus"
	"github.com/weiliang79/belune/internal/worker"
)

// domainTLSStatus is one row of the central TLS view: what the server last
// observed for a domain, not what its configuration claims should happen.
type domainTLSStatus struct {
	ID               string     `json:"id"`
	Hostname         string     `json:"hostname"`
	SSLMode          string     `json:"ssl_mode"`
	TLSStatus        string     `json:"tls_status"`
	TLSIssuer        string     `json:"tls_issuer,omitempty"`
	TLSNotAfter      *time.Time `json:"tls_not_after"`
	TLSLastCheckedAt *time.Time `json:"tls_last_checked_at"`
	TLSError         string     `json:"tls_error,omitempty"`
	// TLSAdvisory is a suspicion, not a verdict — it explains a domain that is
	// still pending without asserting that anything is wrong. Kept apart from
	// TLSError, which is authoritative and decides the status.
	TLSAdvisory     string `json:"tls_advisory,omitempty"`
	CertificateName string `json:"certificate_name,omitempty"`
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	ProjectID       string `json:"project_id"`
}

// ListDomainTLSStatus returns every domain's TLS state in one place — the view
// that makes a stuck certificate obvious instead of requiring the operator to
// click through each application to find it.
//
//apidoc:tag platform
//apidoc:title Get Domain TLS Status
//apidoc:description Every domain's observed TLS state, with the name of the certificate it serves. An admin sees every domain on the install; a member sees only domains in their own projects and any shared with them.
//apidoc:order 3
func (h *Handler) ListDomainTLSStatus(w http.ResponseWriter, r *http.Request) {
	// A NULL user_id asks for every domain. Admins get that; everyone else is
	// narrowed to what they can already reach, which is what lets this route
	// serve both audiences instead of staying admin-only — a member has no
	// other way to resolve their domain's certificate_id to a name.
	var scope pgtype.UUID
	if middleware.RoleFromContext(r.Context()) != "admin" {
		if err := scope.Scan(middleware.UserIDFromContext(r.Context())); err != nil {
			writeError(w, http.StatusUnauthorized, "invalid user id")
			return
		}
	}

	// This route has no {projectId} param, so middleware.RequirePinnedProjectMatch
	// never sees one — enforce the token's pin here or a pinned token would read
	// every domain its owner can reach, which is the escape the pin exists to
	// prevent. Same treatment GetGlobalDeployments gives its own query filter.
	// nil means unpinned; non-nil (even empty) means "only these ids."
	pinnedIDs, err := pinnedProjectUUIDs(r.Context())
	if err != nil {
		writeServerError(w, r, "invalid pinned project", err)
		return
	}

	rows, err := h.queries.ListDomainsWithTLSStatus(r.Context(), generated.ListDomainsWithTLSStatusParams{
		UserID:     scope,
		ProjectIds: pinnedIDs,
	})
	if err != nil {
		writeServerError(w, r, "failed to list domain TLS status", err)
		return
	}

	out := make([]domainTLSStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, domainTLSStatus{
			ID:               uuidToString(row.ID),
			Hostname:         row.Hostname,
			SSLMode:          row.SslMode,
			TLSStatus:        row.TlsStatus,
			TLSIssuer:        row.TlsIssuer.String,
			TLSNotAfter:      timestampPtr(row.TlsNotAfter),
			TLSLastCheckedAt: timestampPtr(row.TlsLastCheckedAt),
			TLSError:         row.TlsError.String,
			TLSAdvisory:      row.TlsAdvisory.String,
			CertificateName:  row.CertificateName.String,
			ApplicationID:    uuidToString(row.ApplicationID),
			ApplicationName:  row.ApplicationName,
			ProjectID:        uuidToString(row.ProjectID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// RecheckDomainTLS enqueues an immediate probe for one domain, so the user does
// not have to wait out the sweep interval after fixing their DNS.
//
//apidoc:tag applications/domains
//apidoc:title Recheck Domain TLS
//apidoc:order 5
func (h *Handler) RecheckDomainTLS(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}
	if !h.canAccessDomain(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	h.enqueueTLSProbe(domainID)
	w.WriteHeader(http.StatusAccepted)
}

// enqueueTLSProbe asks the worker to re-probe one domain now. Called after a
// domain is created or updated so the badge settles within seconds rather than
// showing a stale state until the next sweep. Best-effort: the periodic sweep
// picks the domain up regardless.
func (h *Handler) enqueueTLSProbe(domainID string) {
	payload, err := json.Marshal(worker.TLSProbePayload{DomainID: domainID})
	if err != nil {
		return
	}
	task := asynq.NewTask(worker.TypeTLSProbe, payload)
	if _, err := h.asynq.Enqueue(task, asynq.Queue("low")); err != nil {
		slog.Warn("failed to enqueue tls probe", "domain_id", domainID, "error", err)
	}
}

func timestampPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// dashboardTLSResponse reports the dashboard's own certificate. The dashboard has
// no row in the domains table, so its status is probed on demand rather than by
// the sweep — the operator is watching the page while Let's Encrypt works.
type dashboardTLSResponse struct {
	Domain      string     `json:"domain"`
	SSLMode     string     `json:"ssl_mode"`
	TLSStatus   string     `json:"tls_status"`
	TLSIssuer   string     `json:"tls_issuer,omitempty"`
	TLSNotAfter *time.Time `json:"tls_not_after"`
	TLSError    string     `json:"tls_error,omitempty"`
}

// GetDashboardTLS probes the certificate the proxy currently serves for the
// configured dashboard domain.
//
//apidoc:tag platform/maintenance
//apidoc:title Get LTS Status
//apidoc:order 2
func (h *Handler) GetDashboardTLS(w http.ResponseWriter, r *http.Request) {
	setting, err := h.queries.GetSetting(r.Context(), proxy.SettingDashboardDomain)
	domain := ""
	if err == nil {
		domain = strings.TrimSpace(setting.Value)
	}
	if domain == "" {
		writeJSON(w, http.StatusOK, dashboardTLSResponse{TLSStatus: tlsstatus.StatusUnknown})
		return
	}

	// Derive on the mode the operator actually chose, not a hardcoded "automatic".
	// An off dashboard serves no certificate by design, and probing it as though
	// it should have one would report a healthy configuration as failed.
	sslMode := proxy.SSLModeAutomatic
	if s, err := h.queries.GetSetting(r.Context(), proxy.SettingDashboardSSLMode); err == nil {
		if m := strings.TrimSpace(s.Value); proxy.ValidSSLMode(m) && m != "" {
			sslMode = m
		}
	}

	internalIssuer := false
	if h.proxy != nil {
		if v, err := h.proxy.UsesInternalIssuer(r.Context()); err == nil {
			internalIssuer = v
		}
	}

	leaf, dialErr := tlsstatus.Probe(r.Context(), h.cfg.CaddyTLSProbeAddr, domain)
	res := tlsstatus.Derive(sslMode, leaf, domain, dialErr, "", time.Now(), internalIssuer)

	out := dashboardTLSResponse{
		Domain:    domain,
		SSLMode:   sslMode,
		TLSStatus: res.Status,
		TLSIssuer: res.Issuer,
		TLSError:  res.Error,
	}
	if !res.NotAfter.IsZero() {
		t := res.NotAfter
		out.TLSNotAfter = &t
	}
	writeJSON(w, http.StatusOK, out)
}
