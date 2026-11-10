package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// serviceMetrics is a per-service runtime snapshot for the project overview.
//
// CPUPercent and MemoryUsed are pointers because zero is a claim about a running
// container and an unknown is not that: null means "not sampled" (no running
// container, or its stats call failed), and the UI renders it as a dash rather than
// a confident 0%. Same convention as instances.Point.
type serviceMetrics struct {
	CPUPercent    *float64 `json:"cpu_percent"`
	MemoryUsed    *int64   `json:"memory_used"`
	MemoryLimit   int64    `json:"memory_limit"`
	UptimeSeconds int64    `json:"uptime_seconds"`
	Status        string   `json:"status"`
	Domain        string   `json:"domain,omitempty"`
	Port          int32    `json:"port,omitempty"`
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
		writeServerError(w, r, "failed to list applications", err)
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

	// Instance rows are how an application reaches its containers: the name is a fact
	// stored on application_instances.container_name, and since 0.3.2 a rolled
	// container is renamed, so the slug names nothing. ⛔ Never look a container up by
	// slug here -- the handler used to, answered 200 with empty metrics for every
	// deployed app, and every test passed.
	instancesByApp := map[string][]generated.ListInstancesWithProcessTypeByApplicationRow{}
	if rows, ierr := h.queries.ListInstancesWithProcessTypeByProject(ctx, projectUUID); ierr != nil {
		slog.Warn("project metrics: could not list instances", "project_id", projectID, "error", ierr)
	} else {
		for _, row := range rows {
			// Identical columns; the per-application type is what the shared
			// sampling rules (instances.LiveInstances / BuildPoint) take.
			id := uuidToString(row.ApplicationID)
			instancesByApp[id] = append(instancesByApp[id], generated.ListInstancesWithProcessTypeByApplicationRow(row))
		}
	}

	// One container listing, indexed by name (== instance container_name).
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
	// liveByApp holds, per app position, the routable instances whose container
	// Docker reports running -- the ones worth a stats call.
	liveByApp := make([][]generated.ListInstancesWithProcessTypeByApplicationRow, len(apps))
	var running []int
	for i, app := range apps {
		appID := uuidToString(app.ID)
		m := serviceMetrics{Status: app.Status, MemoryLimit: app.MemoryLimit}

		// Status and uptime are single-valued, so they follow instances.ResolveOne:
		// the lowest idx among the ROUTABLE type's instances whose container is up,
		// else the lowest idx of the type. Docker decides "up" here, not the row --
		// instance status is eventually consistent, and this view exists to show
		// what Docker says. Rows arrive routable-first, then by idx.
		var routable []generated.ListInstancesWithProcessTypeByApplicationRow
		for _, in := range instancesByApp[appID] {
			if in.ProcessTypeRoutable {
				routable = append(routable, in)
			}
		}
		for _, in := range routable {
			if c, ok := containerByName[in.ContainerName]; ok && c.Status == "running" {
				liveByApp[i] = append(liveByApp[i], in)
			}
		}
		// A roll updates the instance row's container_name in place (one row per
		// type+idx), so this samples the old container or the new one, never both.
		var pick *runtime.ContainerInfo
		if len(liveByApp[i]) > 0 {
			c := containerByName[liveByApp[i][0].ContainerName]
			pick = &c
		} else if len(routable) > 0 {
			if c, ok := containerByName[routable[0].ContainerName]; ok {
				pick = &c
			}
		}
		if pick != nil {
			m.Status = pick.Status
			if !pick.CreatedAt.IsZero() {
				m.UptimeSeconds = int64(time.Since(pick.CreatedAt).Seconds())
			}
		}
		// Usage is read from the containers, not the application row, so it is only
		// populated when one is genuinely up. A row whose stored status claims
		// "running" while its container is not shows null usage -- which is how a
		// stale status presents itself here.
		if len(liveByApp[i]) > 0 {
			running = append(running, i)
		}

		if d, ok := domainByApp[appID]; ok {
			m.Domain = d.hostname
			m.Port = d.port
		}
		metrics[i] = m
	}

	// A one-shot stats call costs about two seconds (Docker samples twice, ~1s
	// apart, to compute the CPU delta), so a serial loop needs 2s x N and blew
	// the 15s handler timeout from 8 apps up. So every running container of every
	// app goes through ONE FetchStats call: a single errgroup capped at
	// instances.MaxConcurrentStats (16 keeps a 50-app project, four waves, ~8s
	// inside the 15s timeout). ⛔ Do not nest a per-app fan-out inside another
	// capped one -- that multiplies the bound (16 x 16 calls on one daemon).
	// A failed call drops only its own container, never cancels the rest.
	var names []string
	for _, i := range running {
		for _, in := range liveByApp[i] {
			names = append(names, in.ContainerName)
		}
	}
	stats := instances.FetchStats(ctx, rt, names)
	now := time.Now()
	for _, i := range running {
		// The routable type's SUM, per instances.BuildPoint: workers are never
		// folded in (a queue worker's CPU is not the web's). Null when no replica
		// answered -- logged, because a failing stats call must stay
		// distinguishable from a real idle container.
		p, ok := instances.BuildPoint(liveByApp[i], stats, now)
		if !ok || p.CPUPercent == nil {
			slog.Debug("project metrics: container stats unavailable", "application", apps[i].Slug)
			continue
		}
		metrics[i].CPUPercent = p.CPUPercent
		metrics[i].MemoryUsed = p.MemoryUsage
		// MemoryLimit stays the configured app.MemoryLimit. Not overwritten from
		// stats: an unlimited container reports the HOST's memory as its limit, so
		// summing replicas gives a number that means nothing, and the field would
		// change meaning (and scale by N) depending on whether Docker answered.
	}

	result := make(map[string]serviceMetrics, len(apps))
	for i, app := range apps {
		result[uuidToString(app.ID)] = metrics[i]
	}

	writeJSON(w, http.StatusOK, result)
}
