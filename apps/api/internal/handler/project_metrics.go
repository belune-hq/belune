package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// maxConcurrentStats bounds in-flight Docker stats calls per overview request.
// Each is a two-sample collection (~2.0s measured against a real daemon, of
// which transport is ~8ms) rather than CPU work, and dockerd serves them fully
// in parallel: 8 calls took 1.97s concurrently against 15.26s serially. So the
// cap protects the daemon, not this process; 16 keeps a 50-app project (four
// waves, ~8s) inside the 15s handler timeout.
const maxConcurrentStats = 16

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
	// the 15s handler timeout from 8 apps up. Fan out with a cap so the wall
	// time stays roughly one call however many apps run. No errgroup context:
	// a failed stats call must degrade only its own row, never cancel the rest.
	// Each app's replicas are sampled in one pass (FetchStats fans out within it);
	// the outer cap bounds apps, so a very wide project can exceed
	// maxConcurrentStats calls briefly -- bounded by instances.MaxConcurrentStats
	// per app, which is acceptable for the daemon.
	var g errgroup.Group
	g.SetLimit(maxConcurrentStats)
	for _, i := range running {
		g.Go(func() error {
			live := liveByApp[i]
			names := make([]string, len(live))
			for j, in := range live {
				names[j] = in.ContainerName
			}
			stats := instances.FetchStats(ctx, rt, names)
			// The routable type's SUM, per instances.BuildPoint: workers are never
			// folded in (a queue worker's CPU is not the web's). Null when no
			// replica answered -- logged, because a failing stats call must stay
			// distinguishable from a real idle container.
			p, ok := instances.BuildPoint(live, stats, time.Now())
			if !ok || p.CPUPercent == nil {
				slog.Debug("project metrics: container stats unavailable", "application", apps[i].Slug)
				return nil
			}
			metrics[i].CPUPercent = p.CPUPercent
			metrics[i].MemoryUsed = p.MemoryUsage
			if p.MemoryLimit != nil && *p.MemoryLimit > 0 {
				metrics[i].MemoryLimit = *p.MemoryLimit
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
