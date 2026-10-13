package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/weiliang79/belune/internal/runtime"
)

// maxConcurrentStats bounds in-flight Docker stats calls per overview request.
// Each is a ~2s two-sample collection rather than CPU work, so the cap protects
// the daemon, not this process; 16 keeps a 50-app project well inside the
// handler timeout.
const maxConcurrentStats = 16

// serviceMetrics is a per-service runtime snapshot for the project overview.
type serviceMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsed    int64   `json:"memory_used"`
	MemoryLimit   int64   `json:"memory_limit"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	Status        string  `json:"status"`
	Domain        string  `json:"domain,omitempty"`
	Port          int32   `json:"port,omitempty"`
}

// GetProjectMetrics returns a current runtime snapshot (CPU%, memory, uptime,
// status, primary domain/port) for each application in a project, keyed by
// application id. Container stats are a best-effort single poll — apps without a
// running container simply omit live usage. GET /api/projects/{projectId}/metrics
//
//apidoc:tag projects
//apidoc:title Get Metrics
//apidoc:order 5
func (h *Handler) GetProjectMetrics(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	ctx := r.Context()

	apps, err := h.queries.ListApplicationsByProject(ctx, projectUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list applications")
		return
	}

	// Primary domain/port per app (best-effort; ignore on error).
	domainByApp := map[string]struct {
		hostname string
		port     int32
	}{}
	if rows, derr := h.queries.ListProjectAppPrimaryDomain(ctx, projectUUID); derr == nil {
		for _, row := range rows {
			domainByApp[uuidToString(row.ApplicationID)] = struct {
				hostname string
				port     int32
			}{row.Hostname.String, row.ContainerPort.Int32}
		}
	}

	// Everything in a project shares the project's host, so one resolve covers
	// the listing and every per-container stats call below.
	// Live usage is enrichment on top of the DB-derived rows, so an unreachable
	// host degrades to those rows rather than failing the whole overview.
	rt, rtErr := h.runtimeForProject(ctx, projectUUID)
	if rtErr != nil {
		slog.Warn("project metrics: could not reach the project's server", "project_id", projectID, "error", rtErr)
	}

	// One container listing, indexed by name (== app slug).
	containerByName := map[string]runtime.ContainerInfo{}
	if rt != nil {
		if containers, cerr := rt.ListContainers(ctx); cerr == nil {
			for _, c := range containers {
				containerByName[c.Name] = c
			}
		}
	}

	// Indexed by position so the stats goroutines below each write their own
	// element and need no lock.
	metrics := make([]serviceMetrics, len(apps))
	var running []int
	for i, app := range apps {
		appID := uuidToString(app.ID)
		m := serviceMetrics{Status: app.Status, MemoryLimit: app.MemoryLimit}

		if c, ok := containerByName[app.Slug]; ok {
			m.Status = c.Status
			if !c.CreatedAt.IsZero() {
				m.UptimeSeconds = int64(time.Since(c.CreatedAt).Seconds())
			}
			// Usage is read from the container, not the application row, so it
			// is only populated when the container is genuinely up. A row whose
			// stored status claims "running" while its container is not will
			// show no usage at all — which is how a stale status used to
			// present itself here, as a service reading "Running" with an empty
			// CPU and memory column.
			if c.Status == "running" {
				running = append(running, i)
			}
		}

		if d, ok := domainByApp[appID]; ok {
			m.Domain = d.hostname
			m.Port = d.port
		}
		metrics[i] = m
	}

	// A one-shot stats call costs about two seconds (Docker samples twice, ~1s
	// apart, to compute the CPU delta), so a serial loop needs 2s x N and blew
	// the 15s handler timeout from 8 apps up. Fan out with a cap so the wall
	// time stays roughly one call however many apps run. No errgroup context:
	// a failed stats call must degrade only its own row, never cancel the rest.
	var g errgroup.Group
	g.SetLimit(maxConcurrentStats)
	for _, i := range running {
		g.Go(func() error {
			slug := apps[i].Slug
			s, serr := rt.ContainerStats(ctx, slug)
			if serr != nil {
				// Left at zero, which the UI renders the same as a genuinely
				// idle container — so log it, otherwise a failing stats call
				// is indistinguishable from real zeros.
				slog.Debug("project metrics: container stats unavailable",
					"application", slug, "error", serr)
				return nil
			}
			metrics[i].CPUPercent = s.CPUPercent
			metrics[i].MemoryUsed = s.MemoryUsage
			if s.MemoryLimit > 0 {
				metrics[i].MemoryLimit = s.MemoryLimit
			}
			return nil
		})
	}
	_ = g.Wait()

	result := make(map[string]serviceMetrics, len(apps))
	for i, app := range apps {
		result[uuidToString(app.ID)] = metrics[i]
	}

	writeJSON(w, http.StatusOK, result)
}
