package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/service"
)

// dnsProviderResponse is the secret-free view of a provider. The credentials are
// write-only: they are accepted on create and rotate and never returned, so there
// is no path by which a read can leak one.
type dnsProviderResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	DomainCount int64     `json:"domain_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type dnsProviderRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Credentials is keyed by the provider's own field names (api_token,
	// access_key_id, …). On update, absent keeps the stored secret; present
	// replaces it wholesale.
	Credentials map[string]string `json:"credentials"`
}

func (h *Handler) dnsProviderIDFromPath(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "providerId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid dns provider id")
		return id, false
	}
	return id, true
}

// credentialKeyNames is what audit records about a credential: which fields were
// set, never what they were set to.
func credentialKeyNames(creds map[string]string) []string {
	keys := make([]string, 0, len(creds))
	for k := range creds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ListDNSProviders lists the install's DNS providers, without credentials.
//
//apidoc:tag platform/dns-providers
//apidoc:title List Providers
//apidoc:order 1
func (h *Handler) ListDNSProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.dnsProviderSvc.List(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list dns providers", err)
		return
	}
	resp := make([]dnsProviderResponse, 0, len(rows))
	for _, p := range rows {
		resp = append(resp, dnsProviderResponse{
			ID: uuidToString(p.ID), Name: p.Name, Provider: p.Provider,
			DomainCount: p.DomainCount,
			CreatedAt:   p.CreatedAt.Time, UpdatedAt: p.UpdatedAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateDNSProvider stores a DNS provider's credentials for DNS-01 issuance.
//
//apidoc:tag platform/dns-providers
//apidoc:title Create Provider
//apidoc:order 2
func (h *Handler) CreateDNSProvider(w http.ResponseWriter, r *http.Request) {
	var req dnsProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	spec, ok := proxy.DNSProviders[req.Provider]
	if !ok {
		writeError(w, http.StatusBadRequest, "provider must be one of cloudflare, route53, digitalocean")
		return
	}
	if _, err := spec.Normalize(req.Credentials); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.dnsProviderSvc.Create(r.Context(), req.Name, req.Provider, req.Credentials)
	if err != nil {
		writeDNSProviderError(w, r, err)
		return
	}
	h.audit(r, "create_dns_provider", "dns_provider", uuidToString(p.ID), map[string]any{
		"name": p.Name, "provider": p.Provider, "fields_set": credentialKeyNames(req.Credentials),
	})
	writeJSON(w, http.StatusCreated, dnsProviderResponse{
		ID: uuidToString(p.ID), Name: p.Name, Provider: p.Provider,
		CreatedAt: p.CreatedAt.Time, UpdatedAt: p.UpdatedAt.Time,
	})
}

// UpdateDNSProvider renames a provider and/or rotates its credentials. The
// provider type is fixed at creation.
//
//apidoc:tag platform/dns-providers
//apidoc:title Update Provider
//apidoc:order 3
func (h *Handler) UpdateDNSProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := h.dnsProviderIDFromPath(w, r)
	if !ok {
		return
	}
	var req dnsProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	current, err := h.dnsProviderSvc.Get(r.Context(), id)
	if err != nil {
		writeDNSProviderError(w, r, err)
		return
	}
	if req.Provider != "" && req.Provider != current.Provider {
		writeError(w, http.StatusBadRequest, "the provider type cannot be changed; create a new provider instead")
		return
	}
	if req.Credentials != nil {
		if _, err := proxy.DNSProviders[current.Provider].Normalize(req.Credentials); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	p, err := h.dnsProviderSvc.Update(r.Context(), id, req.Name, req.Credentials)
	if err != nil {
		writeDNSProviderError(w, r, err)
		return
	}
	h.audit(r, "update_dns_provider", "dns_provider", uuidToString(p.ID), map[string]any{
		"name": p.Name, "fields_set": credentialKeyNames(req.Credentials), "rotated": req.Credentials != nil,
	})
	// The reconciler re-asserts policies every 30s; nudging it means a rotated
	// token reaches Caddy now, not half a minute from now.
	h.syncDNSPolicies(r.Context())
	writeJSON(w, http.StatusOK, dnsProviderResponse{
		ID: uuidToString(p.ID), Name: p.Name, Provider: p.Provider,
		CreatedAt: p.CreatedAt.Time, UpdatedAt: p.UpdatedAt.Time,
	})
}

// DeleteDNSProvider removes a provider, refused while any domain uses it.
//
//apidoc:tag platform/dns-providers
//apidoc:title Delete Provider
//apidoc:order 4
func (h *Handler) DeleteDNSProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := h.dnsProviderIDFromPath(w, r)
	if !ok {
		return
	}
	if err := h.dnsProviderSvc.Delete(r.Context(), id); err != nil {
		writeDNSProviderError(w, r, err)
		return
	}
	h.audit(r, "delete_dns_provider", "dns_provider", uuidToString(id), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func writeDNSProviderError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "dns provider not found")
		return
	}
	var inUse service.ErrDNSProviderInUse
	if errors.As(err, &inUse) {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"%d domain(s) use this DNS provider. Switch them to another SSL mode or provider before removing it.", inUse.DomainCount))
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "a DNS provider with that name already exists")
		return
	}
	writeServerError(w, r, "failed to save dns provider", err)
}

// syncDNSPolicies pushes the current DNS-01 policies to Caddy ahead of the route
// that needs them. Without it a new dns_challenge domain's route lands first and
// Caddy tries HTTP-01 for a name that can never pass it (a wildcard, or a host
// behind a CDN), then backs off. Best-effort: the reconciler re-asserts the same
// plan every pass, so a failure here costs latency, not correctness.
func (h *Handler) syncDNSPolicies(ctx context.Context) {
	policies, _, err := proxy.PlanDNSPolicies(ctx, h.queries, h.cfg.Keyring, h.capabilities)
	if err != nil {
		slog.Warn("dns policies: could not plan", "error", err)
	}
	if err := h.proxy.SyncDNSPolicies(ctx, policies); err != nil {
		slog.Warn("dns policies: could not sync to the proxy", "error", err)
	}
}
