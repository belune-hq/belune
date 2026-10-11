package handler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/store/generated"
)

// wsAuthTimeout bounds one subscribe-time lookup. ReadPump is the socket's only
// reader — it also services pings — so a query stuck behind an exhausted pool
// would otherwise freeze the whole connection, not just the one subscription.
const wsAuthTimeout = 5 * time.Second

// wsAdminChannels are the channels with no owning resource. Each mirrors a REST
// twin that is admin-only, so the socket must not be the softer way in.
var wsAdminChannels = map[string]struct{}{
	"metrics:host": {}, // GET /api/metrics/host
	"requests:all": {}, // GET /api/requests
}

// wsOpenChannels are heard by every authenticated subscriber — Members and
// project-pinned tokens included — because what they carry is already public.
// "platform" announces that an update is underway, which is exactly what the
// unauthenticated GET /api/version reports in its `updating` flag; the socket
// only delivers it sooner. It must never carry more than that endpoint does
// (the target version above all), and it is not a place to park other
// platform-wide events: those need their own decision about who may hear them.
var wsOpenChannels = map[string]struct{}{
	"platform": {},
}

// wsResourceChannels maps a channel prefix to the check on the resource id that
// follows it. A channel matching none of this, wsOpenChannels or wsAdminChannels is refused, so
// a channel added to the server but not listed here is denied until someone
// decides who may hear it — never open by default.
var wsResourceChannels = []struct {
	prefix string
	check  func(a *wsChannelAuthorizer, ctx context.Context, id pgtype.UUID) bool
}{
	{"requests:", (*wsChannelAuthorizer).application}, // requests:{appID}
	{"metrics:app:", (*wsChannelAuthorizer).application},
	{"container-status:", (*wsChannelAuthorizer).application},
	// The log collector publishes application AND database container logs to
	// this one prefix, so the id can be either kind.
	//
	// ⚠️ It also publishes SYSTEM container logs here, under a synthetic source
	// id (logcollector.CaddySourceID, "00000000-0000-0000-0000-0000000000ca").
	// Those are deliberately absent: a synthetic id resolves to neither an
	// application nor a database, so applicationOrDatabase refuses it. Nothing
	// subscribes to them today — the log viewer is mounted only on an
	// application and a database, and platform logs are served over REST
	// (GET /api/maintenance/logs, admin) — so the refusal costs nothing. But a
	// live platform-log viewer would be denied and look like a broken socket;
	// it needs an explicit admin-only entry here, not a loosened id check.
	{"container-logs:", (*wsChannelAuthorizer).applicationOrDatabase},
	{"build-logs:", (*wsChannelAuthorizer).deployment}, // build-logs:{deploymentID}
	{"database-status:", (*wsChannelAuthorizer).database},
}

// wsChannelAuthorizer is the ws.ChannelAuthorizer for the dashboard socket. It
// reuses the REST authorization rule (ownerMayAccess, the core of canAccessOwned)
// and adds the token-pin gate that REST enforces in RequirePinnedProjectMatch by URL
// param — a subscribe has no {projectId} in its path, so like the MCP tools it
// has to ask the pin itself, once per subscription.
type wsChannelAuthorizer struct {
	queries *generated.Queries
}

// AuthorizeChannel reports whether the caller in ctx may subscribe to channel.
// The two axes — the token's project pin and the owner's ownership/sharing —
// are separate on purpose and both must pass; a false is uniform, whether the
// resource is forbidden or does not exist, so a refusal cannot be used to probe
// for what exists.
func (a *wsChannelAuthorizer) AuthorizeChannel(ctx context.Context, channel string) bool {
	if _, ok := wsAdminChannels[channel]; ok {
		return wsAdminChannelAllowed(ctx, channel)
	}
	if _, ok := wsOpenChannels[channel]; ok {
		return true
	}
	for _, rule := range wsResourceChannels {
		rest, ok := strings.CutPrefix(channel, rule.prefix)
		if !ok {
			continue
		}
		id, ok := parseCanonicalUUID(rest)
		if !ok {
			return false
		}
		ctx, cancel := context.WithTimeout(ctx, wsAuthTimeout)
		defer cancel()
		return rule.check(a, ctx, id)
	}
	return false
}

func wsAdminChannelAllowed(ctx context.Context, channel string) bool {
	if middleware.RoleFromContext(ctx) != "admin" {
		return false
	}
	// requests:all is one firehose across every project, and the hub sends every
	// subscriber the same frames — it cannot be narrowed to a pin, so a pinned
	// token gets none of it. metrics:host is host-level, not project data; the
	// pin has nothing to say about it (its REST twin ignores the pin the same way).
	if channel == "requests:all" && middleware.TokenPinnedFromContext(ctx) {
		return false
	}
	return true
}

func (a *wsChannelAuthorizer) application(ctx context.Context, id pgtype.UUID) bool {
	// GetApplicationLogAccess rather than GetApplicationOwnerUserID: it already
	// returns project_id alongside owner/shared, so the pin check and the
	// ownership check come from one round trip, and it selects only what an
	// authorization decision needs.
	row, err := a.queries.GetApplicationLogAccess(ctx, id)
	if err != nil {
		logWSLookupFailure(err)
		return false
	}
	return wsProjectAccess(ctx, row.ProjectID, row.ProjectUserID, row.ProjectShared)
}

func (a *wsChannelAuthorizer) database(ctx context.Context, id pgtype.UUID) bool {
	row, err := a.queries.GetDatabaseOwnerUserID(ctx, id)
	if err != nil {
		logWSLookupFailure(err)
		return false
	}
	return wsProjectAccess(ctx, row.ProjectID, row.UserID, row.Shared)
}

// applicationOrDatabase resolves an id that may name either. Neither resolving
// is a refusal: treating "unknown" as allowed would leave the hole open, and
// only trying one kind would break log viewing for the other.
func (a *wsChannelAuthorizer) applicationOrDatabase(ctx context.Context, id pgtype.UUID) bool {
	row, err := a.queries.GetApplicationLogAccess(ctx, id)
	switch {
	case err == nil:
		return wsProjectAccess(ctx, row.ProjectID, row.ProjectUserID, row.ProjectShared)
	case !errors.Is(err, pgx.ErrNoRows):
		logWSLookupFailure(err)
		return false
	}
	return a.database(ctx, id)
}

// deployment follows deployment → application → project. GetDeploymentAccess,
// not GetDeploymentLogAccess: the latter also loads the build log, which is
// unbounded and pointless to fetch just to decide a subscription.
func (a *wsChannelAuthorizer) deployment(ctx context.Context, id pgtype.UUID) bool {
	row, err := a.queries.GetDeploymentAccess(ctx, id)
	if err != nil {
		logWSLookupFailure(err)
		return false
	}
	return wsProjectAccess(ctx, row.ProjectID, row.ProjectUserID, row.ProjectShared)
}

// wsProjectAccess composes the two independent gates on a project-scoped
// resource: the token's project pin (unpinned and sessions pass; a pin that
// resolves to no projects allows nothing — see middleware.PinAllows) and the
// ownership/sharing rule REST applies through canAccessOwned.
func wsProjectAccess(ctx context.Context, projectID, ownerID pgtype.UUID, shared bool) bool {
	return middleware.PinAllows(ctx, uuidToString(projectID)) && ownerMayAccess(ctx, ownerID, shared)
}

// parseCanonicalUUID accepts only the lowercase hyphenated spelling the API
// itself emits. pgtype's Scan is more lenient (uppercase, no hyphens), and that
// leniency would let one resource be subscribed under several spellings: each
// counts as its own channel in ActiveChannelsWithPrefix, multiplying the Docker
// stats calls metrics:app:{id} drives, and none but the canonical one ever
// matches a broadcast — a subscription that looks live and never delivers.
func parseCanonicalUUID(s string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil || !id.Valid {
		return pgtype.UUID{}, false
	}
	return id, uuidToString(id) == s
}

// logWSLookupFailure logs a lookup that failed for a reason other than the row
// not existing. The subscription is refused either way; this only keeps a DB
// outage from looking like a wave of ordinary refusals.
func logWSLookupFailure(err error) {
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("ws: channel authorization lookup failed", "error", err)
	}
}
