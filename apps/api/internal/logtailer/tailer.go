// Package logtailer watches Caddy's JSON access log file, parses new lines,
// resolves the hostname to an application_id, batch-inserts into request_logs,
// and publishes each entry to Redis pub/sub for live SSE consumers.
package logtailer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/belune-hq/belune/internal/store/generated"
)

// caddyLogEntry is the subset of Caddy's JSON access log that we care about.
type caddyLogEntry struct {
	TS      float64         `json:"ts"`
	Request caddyLogRequest `json:"request"`
	Status  int             `json:"status"`
	// duration is in seconds (float)
	Duration float64 `json:"duration"`
	// size is response bytes
	Size      int64 `json:"size"`
	BytesRead int64 `json:"bytes_read"`
}

type caddyLogRequest struct {
	Method   string              `json:"method"`
	Host     string              `json:"host"`
	URI      string              `json:"uri"`
	ClientIP string              `json:"client_ip"`
	Headers  map[string][]string `json:"headers"`
}

// Tailer watches a Caddy access log file and stores parsed entries.
type Tailer struct {
	logPath     string
	queries     *generated.Queries
	rdb         *redis.Client
	domainCache map[string]pgtype.UUID // hostname → application_id
	cacheMu     sync.RWMutex
	batchSize   int
	flushEvery  time.Duration

	// stats feeds Status. A mutex rather than atomics because the fields are
	// read together: a snapshot where lines_read and rows_inserted come from
	// different moments would misclassify the state.
	statsMu sync.Mutex
	stats   counters
}

// New creates a new log tailer.
func New(logPath string, queries *generated.Queries, rdb *redis.Client) *Tailer {
	return &Tailer{
		logPath:     logPath,
		queries:     queries,
		rdb:         rdb,
		domainCache: make(map[string]pgtype.UUID),
		batchSize:   100,
		flushEvery:  2 * time.Second,
		stats:       counters{startedAt: time.Now()},
	}
}

// missingLogGrace is how long the access log may be absent before we say so.
// Caddy creates it on the first request, so a little patience avoids a spurious
// warning on a cold start; beyond this, the path is wrong and someone needs to know.
const missingLogGrace = 60 * time.Second

// Run starts the tailer loop, blocking until ctx is cancelled.
func (t *Tailer) Run(ctx context.Context) {
	slog.Info("access log tailer starting", "path", t.logPath)

	for {
		if err := t.tail(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("access log tailer error, retrying in 5s", "error", err)
			t.recordError(err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (t *Tailer) tail(ctx context.Context) error {
	// Wait for the log file to exist. Caddy may not have created it yet, so being
	// patient is right — but this was a Debug line in an unbounded loop, so when the
	// path was simply wrong (the default is a repo-relative dev path that does not
	// exist inside the container) the tailer waited forever and said nothing at all.
	// Request logging was dead in production for months and the only evidence was an
	// empty table. Stay patient; stop being quiet.
	waitStart := time.Now()
	warned := false
	for {
		if _, err := os.Stat(t.logPath); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
			if !warned && time.Since(waitStart) > missingLogGrace {
				slog.Warn("access log file still does not exist — request logging is recording nothing. Check ACCESS_LOG_PATH, and that Caddy's log volume is mounted into this container.",
					"path", t.logPath, "waited", time.Since(waitStart).Round(time.Second))
				warned = true
			} else {
				slog.Debug("access log file not found, waiting", "path", t.logPath)
			}
		}
	}

	f, err := os.Open(t.logPath)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()

	t.setTailing(true)
	defer t.setTailing(false)

	// Seek to end so we only process new entries
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek to end: %w", err)
	}

	var batch []generated.InsertRequestLogParams
	flushTicker := time.NewTicker(t.flushEvery)
	defer flushTicker.Stop()

	reader := bufio.NewReader(f)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read line: %w", err)
			}
			// No new data — flush pending batch and wait
			if len(batch) > 0 {
				t.flush(ctx, batch)
				batch = batch[:0]
			}
			select {
			case <-ctx.Done():
				return nil
			case <-flushTicker.C:
				// Re-try reading in case new data arrived
				continue
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		t.countLine()

		params, ok := t.parseLine(ctx, line)
		if !ok {
			continue
		}

		batch = append(batch, params)
		if len(batch) >= t.batchSize {
			t.flush(ctx, batch)
			batch = batch[:0]
		}
	}
}

func (t *Tailer) parseLine(ctx context.Context, line string) (generated.InsertRequestLogParams, bool) {
	var entry caddyLogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		slog.Debug("failed to parse access log line", "error", err)
		return generated.InsertRequestLogParams{}, false
	}

	host := entry.Request.Host
	if host == "" {
		return generated.InsertRequestLogParams{}, false
	}

	// Strip port from host if present
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}

	appID, ok := t.resolveHostname(ctx, host)
	if !ok {
		// Not a known app domain — skip (e.g. requests to the PaaS app itself)
		return generated.InsertRequestLogParams{}, false
	}

	latencyMs := int32(math.Round(entry.Duration * 1000))
	if latencyMs < 0 {
		latencyMs = 0
	}

	var requestSize pgtype.Int4
	if entry.BytesRead > 0 {
		requestSize = pgtype.Int4{Int32: int32(entry.BytesRead), Valid: true}
	} else if cls := entry.Request.Headers["Content-Length"]; len(cls) > 0 {
		if n, err := strconv.ParseInt(cls[0], 10, 32); err == nil {
			requestSize = pgtype.Int4{Int32: int32(n), Valid: true}
		}
	}

	var responseSize pgtype.Int4
	if entry.Size > 0 {
		responseSize = pgtype.Int4{Int32: int32(entry.Size), Valid: true}
	}

	var clientIP pgtype.Text
	if entry.Request.ClientIP != "" {
		clientIP = pgtype.Text{String: entry.Request.ClientIP, Valid: true}
	}

	var userAgent pgtype.Text
	if uas := entry.Request.Headers["User-Agent"]; len(uas) > 0 {
		userAgent = pgtype.Text{String: uas[0], Valid: true}
	}

	// Clamp status code to int16 range
	statusCode := int16(entry.Status)
	if entry.Status < 100 || entry.Status > 599 {
		statusCode = 0
	}

	return generated.InsertRequestLogParams{
		ApplicationID: appID,
		Method:        entry.Request.Method,
		Path:          entry.Request.URI,
		StatusCode:    statusCode,
		LatencyMs:     latencyMs,
		Hostname:      host,
		RequestSize:   requestSize,
		ResponseSize:  responseSize,
		ClientIp:      clientIP,
		UserAgent:     userAgent,
	}, true
}

func (t *Tailer) resolveHostname(ctx context.Context, hostname string) (pgtype.UUID, bool) {
	t.cacheMu.RLock()
	id, ok := t.domainCache[hostname]
	t.cacheMu.RUnlock()
	if ok {
		return id, id.Valid
	}

	// Cache miss — query DB
	domain, err := t.queries.GetDomainByHostname(ctx, hostname)
	if err != nil {
		// Unknown hostname — cache as invalid so we don't query again
		t.cacheMu.Lock()
		t.domainCache[hostname] = pgtype.UUID{}
		t.cacheMu.Unlock()
		return pgtype.UUID{}, false
	}

	t.cacheMu.Lock()
	t.domainCache[hostname] = domain.ApplicationID
	t.cacheMu.Unlock()
	return domain.ApplicationID, true
}

// InvalidateHostname removes a hostname from the cache (call when domains change).
func (t *Tailer) InvalidateHostname(hostname string) {
	t.cacheMu.Lock()
	delete(t.domainCache, hostname)
	t.cacheMu.Unlock()
}

func (t *Tailer) flush(ctx context.Context, batch []generated.InsertRequestLogParams) {
	for _, p := range batch {
		if err := t.queries.InsertRequestLog(ctx, p); err != nil {
			slog.Warn("failed to insert request log", "error", err)
			t.recordError(fmt.Errorf("insert request log: %w", err))
			continue
		}
		t.countInsert()

		// Publish to Redis pub/sub for live SSE consumers
		appIDStr := fmt.Sprintf("%x-%x-%x-%x-%x",
			p.ApplicationID.Bytes[0:4], p.ApplicationID.Bytes[4:6],
			p.ApplicationID.Bytes[6:8], p.ApplicationID.Bytes[8:10],
			p.ApplicationID.Bytes[10:16])

		payload, _ := json.Marshal(map[string]any{
			"id":          appIDStr + "-" + fmt.Sprint(time.Now().UnixNano()),
			"method":      p.Method,
			"path":        p.Path,
			"status_code": p.StatusCode,
			"latency_ms":  p.LatencyMs,
			"hostname":    p.Hostname,
			"client_ip":   p.ClientIp.String,
			"recorded_at": time.Now().UTC().Format(time.RFC3339),
		})
		t.rdb.Publish(ctx, "requests:live:"+appIDStr, string(payload))
	}
}
