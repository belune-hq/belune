package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// maxPinnedProjects bounds a single create request's project_ids. Installs
// typically have a handful of projects (the picker only ever lists ones the
// caller can reach), so this is generous headroom, not a real ceiling — its
// job is to cap the per-id canAccessProject/GetProject round trips below,
// not to bound anything a legitimate pin would ever approach.
const maxPinnedProjects = 100

// validTokenExpiryDays mirrors the UI's expiry picker (1/7/14/30/60/90 days).
// Enforced here too, so a client cannot mint an expiry the UI never offers.
// Absent entirely means "never expires".
var validTokenExpiryDays = map[int]bool{1: true, 7: true, 14: true, 30: true, 60: true, 90: true}

// apiTokenDTO is what a token looks like everywhere except the moment it is
// created — never the hash, never the plaintext.
type apiTokenDTO struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Scopes      []string `json:"scopes"`
	RoleAtIssue string   `json:"role_at_issue"`
	// Pinned is the token's own recorded pin state — never inferred from
	// len(ProjectIDs) == 0, since a pinned token can legitimately end up with
	// no reachable projects left (see migration 000067) and that must read as
	// "pinned to nothing," not "unpinned."
	Pinned bool `json:"pinned"`
	// ProjectIDs is every project this token is pinned to — always present
	// (never null), empty when Pinned is false.
	ProjectIDs []string           `json:"project_ids"`
	ExpiresAt  pgtype.Timestamptz `json:"expires_at"`
	LastUsedAt pgtype.Timestamptz `json:"last_used_at"`
	CreatedAt  pgtype.Timestamptz `json:"created_at"`
}

// uuidsToStrings converts a query result's project id array to its DTO form.
// emit_empty_slices keeps this non-nil even for a token with no pins, so the
// output here is too — the frontend never has to distinguish "null" from "[]".
func uuidsToStrings(ids []pgtype.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = uuidToString(id)
	}
	return out
}

func tokenDTOFromRow(row generated.ListAPITokensByUserRow) apiTokenDTO {
	return apiTokenDTO{
		ID:          uuidToString(row.ID),
		Name:        row.Name,
		Scopes:      row.Scopes,
		RoleAtIssue: row.RoleAtIssue,
		Pinned:      row.Pinned,
		ProjectIDs:  uuidsToStrings(row.ProjectIds),
		ExpiresAt:   row.ExpiresAt,
		LastUsedAt:  row.LastUsedAt,
		CreatedAt:   row.CreatedAt,
	}
}

// ListAPITokens returns the current user's own tokens — never another user's.
// There is no cross-user token oversight view in v1, admin or not.
// GET /api/tokens
//
//apidoc:tag tokens
func (h *Handler) ListAPITokens(w http.ResponseWriter, r *http.Request) {
	userUUID, ok := currentUserUUID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	rows, err := h.queries.ListAPITokensByUser(r.Context(), userUUID)
	if err != nil {
		writeServerError(w, r, "failed to list tokens", err)
		return
	}

	dtos := make([]apiTokenDTO, len(rows))
	for i, row := range rows {
		dtos[i] = tokenDTOFromRow(row)
	}
	writeJSON(w, http.StatusOK, dtos)
}

type createTokenRequest struct {
	Name          string   `json:"name"`
	ExpiresInDays *int     `json:"expires_in_days"`
	Scopes        []string `json:"scopes"`
	// ProjectIDs pins the token to these projects; empty or omitted means
	// unpinned (every project the owner can reach, evaluated at use time).
	// Each id is validated below with canAccessProject — a Member can pin to
	// a project shared with them, not just one they own.
	ProjectIDs []string `json:"project_ids"`
}

// validScopes indexes service.AllScopes for membership checks below.
var validScopes = func() map[string]bool {
	m := make(map[string]bool, len(service.AllScopes))
	for _, s := range service.AllScopes {
		m[s] = true
	}
	return m
}()

// normalizeScopes validates that every requested scope is a known one and
// returns the deduplicated set. An empty or all-invalid request is rejected
// outright — a token with zero scopes is a footgun (it authenticates but can
// do literally nothing), not a narrower valid choice.
func normalizeScopes(requested []string) ([]string, bool) {
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		if !validScopes[s] || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// CreateAPIToken mints a token for the current user with exactly the scopes
// it requests — validated against service.AllScopes, so a client cannot smuggle
// in a value PR4's enforcement doesn't know about. Optionally pinned to one or
// more projects the caller can already reach; omitted or empty, it is
// unpinned (every project the owner can reach, evaluated at use time).
// POST /api/tokens
//
//apidoc:tag tokens
func (h *Handler) CreateAPIToken(w http.ResponseWriter, r *http.Request) {
	userUUID, ok := currentUserUUID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req createTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	scopes, ok := normalizeScopes(req.Scopes)
	if !ok {
		writeError(w, http.StatusBadRequest, "select at least one valid scope")
		return
	}

	var expiresAt pgtype.Timestamptz
	if req.ExpiresInDays != nil {
		if !validTokenExpiryDays[*req.ExpiresInDays] {
			writeError(w, http.StatusBadRequest, "invalid expires_in_days")
			return
		}
		expiresAt = pgtype.Timestamptz{
			Time:  time.Now().Add(time.Duration(*req.ExpiresInDays) * 24 * time.Hour),
			Valid: true,
		}
	}

	if len(req.ProjectIDs) > maxPinnedProjects {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot pin to more than %d projects", maxPinnedProjects))
		return
	}

	// canAccessProject, not isProjectOwner: a Member must be able to pin to a
	// project shared with them, or the feature is useless to exactly the
	// people who need it. This check is UX, not the security boundary — a
	// token's actual reach is owner's role ∩ project access ∩ scope,
	// evaluated at use time, so pinning can never grant access the owner
	// doesn't independently have. Deleting a pinned project no longer deletes
	// the token (see migration 000067) — it just removes that project from
	// the token's reach, narrowing rather than destroying.
	//
	// Deduplicated AFTER parsing, on the parsed pgtype.UUID rather than the
	// raw string: two differently-cased strings for the same UUID (e.g.
	// mixed-case input) parse to identical bytes, so deduping on the raw
	// string before parsing let both through — CreateAPITokenProjectPins
	// then inserted the same (token_id, project_id) pair twice in one
	// statement and violated its own primary key, turning a harmless repeat
	// into a 500.
	seen := make(map[pgtype.UUID]bool, len(req.ProjectIDs))
	pinnedIDs := make([]pgtype.UUID, 0, len(req.ProjectIDs))
	for _, raw := range req.ProjectIDs {
		if raw == "" {
			continue
		}

		var id pgtype.UUID
		if err := id.Scan(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid project id")
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true

		if !h.canAccessProject(r, id) {
			writeError(w, http.StatusForbidden, "access denied")
			return
		}
		// canAccessOwned short-circuits to true for an admin before it ever
		// looks the project up, so a well-formed but nonexistent id sails
		// past the check above and would otherwise surface as a 500 from
		// the api_token_projects foreign key at INSERT time. For a non-admin
		// this is a no-op: canAccessProject already required a successful
		// lookup to pass, so it never fails here — a nonexistent id was
		// already turned into "access denied" above, the same collapse that
		// avoids a cross-tenant existence oracle for that caller. Checked per
		// id, in this order, for the same reason: swapping it would let an
		// admin's existence check run first and leak "not found" vs. "403"
		// to a caller who never should have distinguished them.
		if _, err := h.queries.GetProject(r.Context(), id); err != nil {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		pinnedIDs = append(pinnedIDs, id)
	}

	created, err := h.tokenSvc.Create(r.Context(), service.CreateTokenParams{
		UserID:      userUUID,
		Name:        req.Name,
		RoleAtIssue: middleware.RoleFromContext(r.Context()),
		ExpiresAt:   expiresAt,
		Scopes:      scopes,
		Pinned:      len(pinnedIDs) > 0,
		ProjectIDs:  pinnedIDs,
	})
	if err != nil {
		writeServerError(w, r, "failed to create token", err)
		return
	}

	auditDetails := map[string]any{
		"name": created.Name,
	}
	if len(created.ProjectIDs) > 0 {
		auditDetails["project_ids"] = created.ProjectIDs
	}
	h.audit(r, "token_created", "api_token", uuidToString(created.ID), auditDetails)

	// The only response that ever carries the plaintext — shown once, never
	// stored or logged past this point.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":            uuidToString(created.ID),
		"name":          created.Name,
		"token":         created.Plain,
		"scopes":        created.Scopes,
		"role_at_issue": created.RoleAtIssue,
		"pinned":        created.Pinned,
		"project_ids":   created.ProjectIDs,
		"expires_at":    created.ExpiresAt,
		"created_at":    created.CreatedAt,
	})
}

// DeleteAPIToken revokes one of the current user's own tokens. The query
// itself is scoped by user_id (not just an authz check beforehand), so it can
// never delete another user's token even given that token's id.
// DELETE /api/tokens/{tokenId}
//
//apidoc:tag tokens
func (h *Handler) DeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	userUUID, ok := currentUserUUID(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var tokenUUID pgtype.UUID
	if err := tokenUUID.Scan(chi.URLParam(r, "tokenId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid token id")
		return
	}

	name, err := h.queries.DeleteAPIToken(r.Context(), generated.DeleteAPITokenParams{
		ID:     tokenUUID,
		UserID: userUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "token not found")
			return
		}
		writeServerError(w, r, "failed to delete token", err)
		return
	}

	h.audit(r, "token_deleted", "api_token", uuidToString(tokenUUID), map[string]any{
		"name": name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
