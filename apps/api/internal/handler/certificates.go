package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/service"
)

// ListCertificates returns metadata for every stored certificate. Responses
// carry no key material — only what the leaf declares publicly.
//
//apidoc:tag platform/certificates
//apidoc:title Get Certificates
//apidoc:order 1
func (h *Handler) ListCertificates(w http.ResponseWriter, r *http.Request) {
	certs, err := h.certSvc.ListCertificates(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list certificates", err)
		return
	}
	writeJSON(w, http.StatusOK, certs)
}

// ListUsableCertificates answers "which certificate can serve this hostname",
// the one certificate question a non-admin needs. Adding a domain is
// project-scoped, and ssl_mode=custom requires a certificate_id, so a member
// could configure custom TLS only if an admin read them a uuid — the add-domain
// form rendered an empty picker and refused to submit.
//
// Not a filter on the admin listing but a separate route, deliberately.
// GET /api/certificates must stay behind RequireRole("admin") structurally: it
// returns every certificate with every SAN, and moving the role check into the
// handler so one path could serve both audiences is what makes x-belune-roles
// publish "no role required" for a route that is still admin-only in its
// unfiltered form. Two routes, each meaning exactly one thing.
//
// hostname is required. Without it this would be the admin listing with the
// SANs filed off, reachable by anyone.
//
// ⚠️ Filed under Domains & TLS, NOT platform/certificates, even though the path
// says /api/certificates. Every other certificate route is admin-gated and that
// domain's tag carries the " (Admin)" suffix, which the generator asserts is
// true of every route beneath it — this one is member-reachable, so tagging it
// there would make the docs overstate the restriction. Grouping is by the task
// a reader is doing, and the only reason to call this is while adding a domain.
//
//apidoc:tag applications/domains
//apidoc:title Find Certificates for a Hostname
//apidoc:description Certificates that can serve one hostname, including a wildcard covering it. Takes a required `hostname` query parameter and returns 400 without it. Available to any authenticated caller, unlike the full certificate list: the response names only the certificate and its expiry, never the other hostnames it covers.
//apidoc:order 6
func (h *Handler) ListUsableCertificates(w http.ResponseWriter, r *http.Request) {
	hostname := strings.TrimSpace(r.URL.Query().Get("hostname"))
	if hostname == "" {
		writeError(w, http.StatusBadRequest, "hostname is required")
		return
	}

	certs, err := h.certSvc.CertificatesForHostname(r.Context(), hostname)
	if err != nil {
		writeServerError(w, r, "failed to list certificates", err)
		return
	}
	writeJSON(w, http.StatusOK, certs)
}

type uploadCertificateRequest struct {
	Name    string `json:"name"`
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
}

// UploadCertificate stores a PEM pair after validating it. Validation errors are
// the operator's to fix, so they come back as 400 with the specific reason
// (mismatched key, missing SANs, not PEM at all) rather than a generic failure.
//
//apidoc:tag platform/certificates
//apidoc:title Upload Certificate
//apidoc:order 2
func (h *Handler) UploadCertificate(w http.ResponseWriter, r *http.Request) {
	var req uploadCertificateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var createdBy pgtype.UUID
	if err := createdBy.Scan(middleware.UserIDFromContext(r.Context())); err != nil {
		createdBy = pgtype.UUID{}
	}

	cert, err := h.certSvc.CreateCertificate(r.Context(), req.Name, req.CertPEM, req.KeyPEM, createdBy)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "a certificate with that name already exists")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.audit(r, "upload_certificate", "certificate", cert.ID, map[string]any{
		"name":     cert.Name,
		"subjects": cert.Subjects,
	})

	writeJSON(w, http.StatusCreated, cert)
}

// DeleteCertificate removes a certificate unless domains still serve it.
//
//apidoc:tag platform/certificates
//apidoc:title Delete Certificate
//apidoc:order 3
func (h *Handler) DeleteCertificate(w http.ResponseWriter, r *http.Request) {
	certificateID := chi.URLParam(r, "certificateId")
	var certUUID pgtype.UUID
	if err := certUUID.Scan(certificateID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid certificate id")
		return
	}

	if err := h.certSvc.DeleteCertificate(r.Context(), certUUID); err != nil {
		if errors.Is(err, service.ErrCertificateInUse) {
			// The message names the domains still referencing it, so the operator
			// knows exactly what to detach first.
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeServerError(w, r, "failed to delete certificate", err)
		return
	}

	h.audit(r, "delete_certificate", "certificate", certificateID, nil)

	w.WriteHeader(http.StatusNoContent)
}
