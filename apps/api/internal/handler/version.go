package handler

import (
	"net/http"

	"github.com/belune-hq/belune/internal/version"
)

type versionResponse struct {
	Version string `json:"version"`
	// Updating is true while a self-update is in progress. See updateState for
	// how it is derived and why the target version is not exposed.
	Updating bool `json:"updating"`
	// UpdatingFor is whole seconds since the update began, so the client shows
	// elapsed time without trusting its own clock to agree with the server's.
	// Zero when not updating.
	UpdatingFor int64 `json:"updating_for"`
}

// GetVersion reports the running build's version and whether an update is in
// progress.
//
// Unauthenticated by design. The value is already inferable from the served
// assets and response behaviour, and both the UI's identity block and the
// update checker need it before a session exists. A build that was not stamped
// at link time reports "dev" rather than guessing at a release.
//
// `updating` is public for the same reason: a restart is observable to anyone
// as a refused connection, so "mid-update" adds nothing an observer lacks. It
// answers only "is something happening right now" — the admin-only
// GET /api/maintenance/update/status keeps "did the last attempt succeed".
func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	updating, elapsed := h.updateState(r.Context())
	writeJSON(w, http.StatusOK, versionResponse{
		Version:     version.Version,
		Updating:    updating,
		UpdatingFor: int64(elapsed.Seconds()),
	})
}
