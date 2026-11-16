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
//
// CPUPercent and MemoryUsed are the ROUTABLE type's sum, as they always were: workers are
// never folded in. Instances lists EVERY container of the application, so a view that
// wants the per-replica picture (or to say a worker exists) can have it; each carries its
// own usage, null when that container was not sampled.
//
// The routable pair uses instances.Point's vocabulary: RoutableExpected is the routable
// containers Docker reports running, RoutableSampled the ones whose stats call answered,
// and together they say what the top-level sums cover. Sampled < Expected means a sum or
// a dash is a coverage fact, not an idle container.
//
// ⛔ There is deliberately NO all-types pair here. This endpoint never samples workers, so
// "sampled 2 of expected 3" would read as partial coverage on every healthy application
// that has one, and describeCoverage (which consumes those exact names) would render it
// amber. That a worker exists and was not sampled is what Instances already says.
type serviceMetrics struct {
	CPUPercent       *float64          `json:"cpu_percent"`
	MemoryUsed       *int64            `json:"memory_used"`
	MemoryLimit      int64             `json:"memory_limit"`
	UptimeSeconds    int64             `json:"uptime_seconds"`
	Status           string            `json:"status"`
	Domain           string            `json:"domain,omitempty"`
	Port             int32             `json:"port,omitempty"`
	Instances        []serviceInstance `json:"instances"`
	RoutableSampled  int               `json:"routable_sampled"`
	RoutableExpected int               `json:"routable_expected"`
}

// serviceInstance is one container of an application.
//
// Status is Docker's, from the one container listing this handler already makes (so it
// costs no stats call); when that listing is unavailable it falls back to the instance
// row's stored status, and a container the listing does not contain reads "missing".
// CPUPercent and MemoryUsed are null when the container was not sampled: not running, its
// stats call failed, it is not of the routable type (workers are deliberately not
// sampled here, see maxProjectStats), or the project was too large to sample it.
type serviceInstance struct {
	ProcessType string   `json:"process_type"`
	Idx         int32    `json:"idx"`
	Routable    bool     `json:"routable"`
	Status      string   `json:"status"`
	CPUPercent  *float64 `json:"cpu_percent"`
	MemoryUsed  *int64   `json:"memory_used"`
}

// maxProjectStats bounds how many containers one overview request samples.
//
// A one-shot Docker stats call costs about two seconds, structurally, and FetchStats runs
// at most instances.MaxConcurrentStats at once, so wall time is about
// 2s x ceil(C / MaxConcurrentStats) against a 15s handler timeout. Four waves (~8s)
// leaves the margin the daemon's own variance needs. Past it the overview does not
// time out (a 503 with nothing) and does not pretend (a 200 whose sums are really a
// truncation): an application whose live replicas do not ALL fit is left unsampled, with
// null usage and RoutableSampled < RoutableExpected, so a partial sum is never presented as the total.
const maxProjectStats = instances.MaxConcurrentStats * 4

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
	// Whether the listing SUCCEEDED, not whether it found anything: a project whose
	// containers have all been removed lists nothing, and that is exactly the case where
	// "missing" must replace the row's stale stored status.
	listed := false
	if rt != nil {
		if containers, cerr := rt.ListContainers(ctx); cerr == nil {
			listed = true
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
		m := serviceMetrics{Status: app.Status, MemoryLimit: app.MemoryLimit, Instances: []serviceInstance{}}

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
		for _, in := range instancesByApp[appID] {
			status := in.Status
			if c, ok := containerByName[in.ContainerName]; ok {
				status = c.Status
			} else if listed {
				status = "missing"
			}
			m.Instances = append(m.Instances, serviceInstance{
				ProcessType: in.ProcessTypeName, Idx: in.Idx, Routable: in.ProcessTypeRoutable, Status: status,
			})
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
	// instances.MaxConcurrentStats. That bound is NOT what keeps the handler inside the 15s
	// timeout: wall time is ~2s per wave of 16, so it is the NUMBER of sampled containers
	// that has to be bounded (maxProjectStats), however many replicas or apps make it up.
	// ⛔ Do not nest a per-app fan-out inside another
	// capped one -- that multiplies the bound (16 x 16 calls on one daemon).
	// A failed call drops only its own container, never cancels the rest.
	//
	// ⛔ Do not add sampling here (workers, in particular): the arithmetic above is the
	// budget, and it was last written when an application meant one container. Replicas
	// already bent it; maxProjectStats is what stops the next addition from becoming the
	// next handler timeout.
	var names []string
	var sampledApps []int
	for _, i := range running {
		// All-or-nothing per application, spent in application order: a partial sum in
		// one cell would be the confident wrong number, so an over-budget project leaves
		// the SAME later applications unsampled on every poll. Chosen, not overlooked;
		// the log line is the only place that says why.
		if len(names)+len(liveByApp[i]) > maxProjectStats {
			slog.Warn("project metrics: not sampling an application, the project is over the stats budget",
				"application", apps[i].Slug, "budget", maxProjectStats)
			continue
		}
		sampledApps = append(sampledApps, i)
		for _, in := range liveByApp[i] {
			names = append(names, in.ContainerName)
		}
	}
	stats := instances.FetchStats(ctx, rt, names)
	now := time.Now()
	// Counts for every running application, including one left unsampled above: its
	// RoutableSampled is 0 against its RoutableExpected, which is what makes the dash
	// honest.
	for _, i := range running {
		p, _ := instances.BuildPoint(liveByApp[i], stats, now)
		metrics[i].RoutableSampled, metrics[i].RoutableExpected = p.RoutableSampled, p.RoutableExpected
	}
	// Per-container usage, for the containers that were sampled. Instances is in the same
	// order as instancesByApp, so the index lines up.
	for _, i := range sampledApps {
		for k, in := range instancesByApp[uuidToString(apps[i].ID)] {
			if st, ok := stats[in.ContainerName]; ok {
				cpu, mem := st.CPUPercent, st.MemoryUsage
				metrics[i].Instances[k].CPUPercent, metrics[i].Instances[k].MemoryUsed = &cpu, &mem
			}
		}
	}
	for _, i := range sampledApps {
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
