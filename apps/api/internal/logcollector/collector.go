// Package logcollector attaches to running container log streams, batch-inserts
// log lines into the container_logs table (tagged with a heuristic severity
// level), and publishes them to Redis pub/sub so live WebSocket consumers can
// receive them. It watches both application and database containers.
package logcollector

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/belune-hq/belune/internal/pkg/loglevel"
	"github.com/belune-hq/belune/internal/pkg/tracing"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	labelApplicationID = runtime.LabelApplicationID
	labelDatabaseID    = runtime.LabelDatabaseID
	// labelDeploymentID ties an application container to the deployment (image
	// generation) that created it, so its logs can be grouped into a session
	// distinct from the previous redeploy/rebuild/rollback. Only application
	// containers carry it; databases and system containers have no deployment.
	labelDeploymentID = "deployment-id"
	// labelSystem marks Belune's own infrastructure containers (currently only
	// Caddy). Collecting the proxy's logs is what lets the TLS status pipeline
	// tell a user *why* a certificate failed, instead of leaving the reason in a
	// stdout nobody reads.
	labelSystem = "belune-system"

	sourceApplication = "application"
	sourceDatabase    = "database"
	sourceSystem      = "system"

	// SystemCaddy is the value of the belune-system label on the proxy.
	SystemCaddy = "caddy"
)

// systemSourceIDs maps a system component to a fixed source_id. container_logs
// keys every row by a UUID (applications and databases have one naturally), so
// infrastructure containers need a stable synthetic one rather than a new column.
var systemSourceIDs = map[string]string{
	SystemCaddy: "00000000-0000-0000-0000-0000000000ca",
}

// CaddySourceID is the container_logs.source_id under which Caddy's own logs are
// stored. Exported so the API can serve them without re-deriving it.
var CaddySourceID = systemSourceIDs[SystemCaddy]

// containerSource identifies which resource a container belongs to, derived
// from its labels.
type containerSource struct {
	typ        string // sourceApplication | sourceDatabase | sourceSystem
	id         string // resource UUID (synthetic for system containers)
	name       string // system component name, empty for applications and databases
	deployment string // deployment UUID for application containers, empty otherwise
}

// sourceOf returns the log source for a container, or false if the container
// carries none of our resource labels.
func sourceOf(labels map[string]string) (containerSource, bool) {
	if id := labels[labelApplicationID]; id != "" {
		return containerSource{typ: sourceApplication, id: id, deployment: labels[labelDeploymentID]}, true
	}
	if id := labels[labelDatabaseID]; id != "" {
		return containerSource{typ: sourceDatabase, id: id}, true
	}
	if name := labels[labelSystem]; name != "" {
		id, ok := systemSourceIDs[name]
		if !ok {
			return containerSource{}, false
		}
		return containerSource{typ: sourceSystem, id: id, name: name}, true
	}
	return containerSource{}, false
}

// maxConcurrentWatchers caps how many container log streams the collector
// will track concurrently. Each watcher holds a Docker log stream + a
// goroutine, so an unbounded set could exhaust file descriptors on a host
// running hundreds of containers. The cap is intentionally generous: typical
// single-host PaaS instances run dozens of apps, not hundreds.
const maxConcurrentWatchers = 200

// LineHook observes a single collected log line. It runs inline on the
// collector's read path, so it must be cheap and must not block.
type LineHook func(ctx context.Context, src string, name string, message string)

// Collector watches running containers and persists their logs.
type Collector struct {
	runtime    runtime.ContainerRuntime
	queries    *generated.Queries
	rdb        *redis.Client
	mu         sync.Mutex
	watchers   map[string]context.CancelFunc // containerID → cancel
	maxWatch   int
	batchSize  int
	flushEvery time.Duration
	lineHook   LineHook
}

// SetLineHook installs an observer called for every collected line. It is how
// the TLS status pipeline sees Caddy's certificate errors as they are emitted,
// without the collector needing to know anything about TLS.
func (c *Collector) SetLineHook(hook LineHook) {
	c.lineHook = hook
}

// New creates a new log collector.
func New(rt runtime.ContainerRuntime, queries *generated.Queries, rdb *redis.Client) *Collector {
	return &Collector{
		runtime:    rt,
		queries:    queries,
		rdb:        rdb,
		watchers:   make(map[string]context.CancelFunc),
		maxWatch:   maxConcurrentWatchers,
		batchSize:  50,
		flushEvery: 2 * time.Second,
	}
}

// Run starts the collector, blocking until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	slog.Info("application log collector starting")
	c.sync(ctx)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			for _, cancel := range c.watchers {
				cancel()
			}
			c.mu.Unlock()
			return
		case <-ticker.C:
			c.sync(ctx)
		}
	}
}

// sync lists running containers and starts/stops watchers as needed.
// For newly-attached containers, it picks up log collection from the last
// recorded_at for that application (or from the beginning if none).
func (c *Collector) sync(ctx context.Context) {
	containers, err := c.runtime.ListContainers(ctx)
	if err != nil {
		slog.Warn("log collector: failed to list containers", "error", err)
		return
	}

	// The proxy is not managed-by=belune — that label marks containers the cleanup
	// worker may reap — so ListContainers filters it out at the Docker API and it
	// was never watched. Caddy's log is where ACME failure reasons live, so without
	// this a domain whose certificate failed showed no reason at all.
	system, err := c.runtime.ListSystemContainers(ctx)
	if err != nil {
		slog.Warn("log collector: failed to list system containers", "error", err)
	} else {
		containers = append(containers, system...)
	}

	// Build set of running container IDs that belong to an application or
	// database (i.e. carry one of our resource labels).
	active := make(map[string]struct{})
	for _, ctr := range containers {
		if ctr.Status != "running" {
			continue
		}
		if _, ok := sourceOf(ctr.Labels); !ok {
			continue
		}
		active[ctr.ID] = struct{}{}

		c.mu.Lock()
		_, watching := c.watchers[ctr.ID]
		atCap := !watching && len(c.watchers) >= c.maxWatch
		var watchCtx context.Context
		var cancel context.CancelFunc
		if !watching && !atCap {
			watchCtx, cancel = context.WithCancel(ctx)
			c.watchers[ctr.ID] = cancel
		}
		c.mu.Unlock()

		if atCap {
			slog.Warn("log collector: watcher cap reached, skipping container",
				"container", ctr.Name, "cap", c.maxWatch)
			continue
		}
		if !watching {
			ctrCopy := ctr
			go func() {
				defer func() {
					c.mu.Lock()
					delete(c.watchers, ctrCopy.ID)
					c.mu.Unlock()
				}()
				c.watchContainer(watchCtx, ctrCopy)
			}()
		}
	}

	// Cancel watchers for containers that are no longer running.
	c.mu.Lock()
	for id, cancel := range c.watchers {
		if _, ok := active[id]; !ok {
			cancel()
			delete(c.watchers, id)
		}
	}
	c.mu.Unlock()
}

type logLine struct {
	stream  string
	message string
	level   string
	ts      time.Time // event time from the Docker log stream (zero if unparsed)
}

// chanWriter demultiplexes a Docker log stream into lines and sends them to ch.
type chanWriter struct {
	stream string
	ch     chan<- logLine
	buf    []byte
}

func (w *chanWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		raw := strings.TrimRight(string(w.buf[:idx]), "\r")
		ts, msg := splitTimestamp(raw)
		if msg != "" {
			select {
			case w.ch <- logLine{
				stream:  w.stream,
				message: msg,
				level:   string(loglevel.Detect(msg, w.stream)),
				ts:      ts,
			}:
			default: // drop if channel full rather than blocking
			}
		}
		w.buf = w.buf[idx+1:]
	}
	return len(p), nil
}

// splitTimestamp separates the Docker-prepended RFC3339Nano timestamp prefix
// from the log message, returning the parsed event time and the remaining text.
// Format: "2025-01-01T00:00:00.000000000Z message". If the prefix can't be
// parsed, a zero time and the original line are returned (recorded_at then
// falls back to the DB's NOW()).
func splitTimestamp(line string) (time.Time, string) {
	idx := strings.IndexByte(line, ' ')
	if idx > 0 && idx <= 36 {
		if ts, err := time.Parse(time.RFC3339Nano, line[:idx]); err == nil {
			return ts, line[idx+1:]
		}
	}
	return time.Time{}, line
}

func (c *Collector) watchContainer(ctx context.Context, ctr runtime.ContainerInfo) {
	ctx, span := tracing.Tracer().Start(ctx, "logcollector.watch",
		trace.WithAttributes(
			attribute.String("container.name", ctr.Name),
			attribute.String("container.id", ctr.ID),
		),
	)
	defer span.End()

	src, ok := sourceOf(ctr.Labels)
	if !ok {
		return
	}
	var srcUUID pgtype.UUID
	if err := srcUUID.Scan(src.id); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.Warn("log collector: invalid source id label", "container", ctr.Name, "source_type", src.typ, "source_id", src.id)
		return
	}

	// The deployment id groups this container's lines into a session. It is
	// optional (databases/system containers have none, and an app container
	// predating this feature carries no label), so an unset or unparseable value
	// leaves deployment_id NULL rather than dropping the container.
	var deploymentUUID pgtype.UUID
	if src.deployment != "" {
		if err := deploymentUUID.Scan(src.deployment); err != nil {
			slog.Warn("log collector: invalid deployment id label", "container", ctr.Name, "deployment_id", src.deployment)
			deploymentUUID = pgtype.UUID{}
		}
	}
	span.SetAttributes(
		attribute.String("log.source_type", src.typ),
		attribute.String("log.source_id", src.id),
	)

	// Resume from the last recorded log for this source so a restarted API
	// server (or a brand-new container) doesn't drop early log lines.
	// Zero time = fetch from the beginning of the container's log buffer.
	var since time.Time
	if src.typ == sourceSystem {
		// System containers are not stored, so there is no recorded position to
		// resume from and the lookup below would eventually return nothing and
		// replay the whole buffer. Their lines exist only to feed the line hook,
		// which records per-domain TLS status — replaying historical ACME
		// failures would re-record them against domains that have since issued
		// successfully. Take live lines only; Caddy re-emits on its next attempt.
		since = time.Now()
	} else {
		latest, err := c.queries.GetLatestContainerLogTime(ctx, generated.GetLatestContainerLogTimeParams{
			SourceType: src.typ,
			SourceID:   srcUUID,
		})
		if err == nil && latest.Valid {
			// Add 1ns so we don't re-ingest the exact line we already stored.
			since = latest.Time.Add(time.Nanosecond)
		}
	}

	slog.Info("log collector: attaching to container",
		"container", ctr.Name,
		"source_type", src.typ,
		"source_id", src.id,
		"since", since.Format(time.RFC3339Nano))

	rc, err := c.runtime.ContainerLogsSince(ctx, ctr.ID, since)
	if err != nil {
		slog.Warn("log collector: failed to attach to container logs", "container", ctr.Name, "error", err)
		return
	}
	defer rc.Close()

	lines := make(chan logLine, 256)
	go func() {
		defer close(lines)
		stdout := &chanWriter{stream: "stdout", ch: lines}
		stderr := &chanWriter{stream: "stderr", ch: lines}
		stdcopy.StdCopy(stdout, stderr, rc) //nolint:errcheck
	}()

	var batch []generated.BulkInsertContainerLogsParams
	flushTicker := time.NewTicker(c.flushEvery)
	defer flushTicker.Stop()

	appendLine := func(line logLine) {
		if c.lineHook != nil {
			c.lineHook(ctx, src.typ, src.name, line.message)
		}
		// System containers are watched but not stored. Caddy alone accounted
		// for 94% of container_logs and nothing ever read it back — the rows
		// existed for no consumer. What Caddy's log is actually needed for is
		// the TLS status pipeline, and that reads the line above, before this
		// point, so it is unaffected. Anything that needs to *display* proxy
		// logs reads them live from Docker, as the Maintenance panel does.
		if src.typ == sourceSystem {
			return
		}
		// The bulk insert takes plain values, so the timestamp is resolved here
		// rather than by a COALESCE at insert time: Docker's own timestamp for
		// the line when it parsed, otherwise now — which is when the line was
		// read, and closer to the truth than the moment the batch happens to be
		// flushed.
		ts := line.ts
		if ts.IsZero() {
			ts = time.Now()
		}
		batch = append(batch, generated.BulkInsertContainerLogsParams{
			SourceType: src.typ,
			SourceID:   srcUUID,
			Level:      line.level,
			Stream:     line.stream,
			Message:    line.message,
			RecordedAt: pgtype.Timestamptz{Time: ts, Valid: true},
			// The container generation is the session key: it exists for every
			// source type, including databases, which have no deployment but are
			// still replaced by a new container on a version upgrade.
			ContainerID:  pgtype.Text{String: ctr.ID, Valid: ctr.ID != ""},
			DeploymentID: deploymentUUID,
		})
	}

	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		c.flush(flushCtx, src, batch)
		batch = batch[:0]
	}

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// Container log stream ended (container stopped).
				flush(ctx)
				slog.Info("log collector: container log stream ended", "container", ctr.Name)
				return
			}
			appendLine(line)
			if len(batch) >= c.batchSize {
				flush(ctx)
			}
		case <-flushTicker.C:
			flush(ctx)
		case <-ctx.Done():
			// rc.Close() via defer will unblock the stdcopy goroutine.
			// Drain remaining buffered lines before returning.
			drainTimeout := time.After(1 * time.Second)
		drainLoop:
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						break drainLoop
					}
					appendLine(line)
				case <-drainTimeout:
					break drainLoop
				}
			}
			// Use a bounded shutdown context so the final flush cannot hang
			// indefinitely on a slow DB while the collector is exiting.
			flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			flush(flushCtx)
			cancel()
			return
		}
	}
}

func (c *Collector) flush(ctx context.Context, src containerSource, batch []generated.BulkInsertContainerLogsParams) {
	ctx, span := tracing.Tracer().Start(ctx, "logcollector.flush",
		trace.WithAttributes(
			attribute.String("log.source_type", src.typ),
			attribute.String("log.source_id", src.id),
			attribute.Int("log.batch_size", len(batch)),
		),
	)
	defer span.End()

	// All lines in a batch share one source, so the Redis channel is constant.
	channel := "container-logs:" + src.id

	// One bulk copy for the whole batch rather than a statement per line.
	// container_logs is the highest-write table here — a line per container per
	// output line — and the per-row insert cost a round trip each time.
	//
	// The batch is all-or-nothing, unlike the previous loop which could persist
	// some lines and skip others. Losing a whole batch of best-effort log lines
	// on a database error is preferable to a silently partial one, and the
	// stream is resumed from the last stored timestamp on reattach either way.
	if _, err := c.queries.BulkInsertContainerLogs(ctx, batch); err != nil {
		span.RecordError(err)
		slog.Warn("log collector: failed to insert container logs",
			"source_type", src.typ, "source_id", src.id, "lines", len(batch), "error", err)
		return
	}

	// Publish to Redis for live WebSocket consumers. Done after the rows are
	// durable so a live viewer never sees a line that was not stored.
	for _, p := range batch {
		payload, _ := json.Marshal(map[string]any{
			"level":         p.Level,
			"stream":        p.Stream,
			"message":       p.Message,
			"recorded_at":   p.RecordedAt.Time.UTC().Format(time.RFC3339Nano),
			"container_id":  p.ContainerID.String,
			"deployment_id": src.deployment,
		})
		c.rdb.Publish(ctx, channel, string(payload))
	}
}
