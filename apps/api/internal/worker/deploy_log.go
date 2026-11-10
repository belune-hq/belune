package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/buildlog"
	"github.com/belune-hq/belune/internal/store/generated"
)

// deployLog is the ONE operator-visible log of a deploy, from the pull or build through
// the last container. It exists because deployments.build_logs is written with
// UPDATE ... SET build_logs = $2: a REPLACE, not an append. A second sink for the
// container phase would overwrite the build output with just its own lines, so every
// phase writes to this one sink and every persist writes everything it holds.
//
// ⛔ Do not give a stage its own sink, and do not read-modify-write build_logs.
//
// Lifecycle: opened once in HandleDeployTask, persisted at the points where the UI
// switches from the live stream to the stored blob (after the pull/build, before a
// terminal status), flushed periodically meanwhile so a page refresh shows progress, and
// closed by a defer so EVERY exit path -- including a panic -- stores the log and tells
// live viewers it ended. A failed deploy keeping its build output is the property this
// shape must not lose.
//
// All methods are safe on a nil *deployLog, so a deployContext built without one (tests,
// other tasks) simply logs nothing.
type deployLog struct {
	q            *generated.Queries
	deploymentID pgtype.UUID
	pub          *buildlog.Publisher
	sink         *buildlog.LogSink

	stdout io.Writer
	stderr io.Writer

	// storeMu serialises writes to build_logs and makes each one read the sink INSIDE the
	// lock. The sink is append-only, so every write then carries at least what the one
	// before it did: the periodic flusher can never land an older snapshot on top of a
	// newer final one.
	storeMu sync.Mutex

	stopFlush chan struct{}
	flushDone chan struct{}
	closeOnce sync.Once
}

// deployLogFlushInterval is how often the partial log is persisted while a deploy runs.
const deployLogFlushInterval = 2 * time.Second

// openDeployLog creates the deploy's log and starts its periodic flusher. The sink is
// bound to the TASK's context, not a phase's: a build's timeout context ends with the
// build, and the container phase still has lines to publish.
func (h *TaskHandler) openDeployLog(ctx context.Context, dc *deployContext) {
	pub := buildlog.NewPublisher(h.RedisClient, dc.payload.DeploymentID)
	sink := buildlog.NewLogSink(pub, ctx)
	l := &deployLog{
		q:            h.Queries,
		deploymentID: dc.deploymentID,
		pub:          pub,
		sink:         sink,
		stdout:       sink.Writer("stdout"),
		stderr:       sink.Writer("stderr"),
		stopFlush:    make(chan struct{}),
		flushDone:    make(chan struct{}),
	}
	dc.log = l
	go func() {
		defer close(l.flushDone)
		t := time.NewTicker(deployLogFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-l.stopFlush:
				return
			case <-t.C:
				l.store(ctx)
			}
		}
	}()
}

// Logf writes one line to the deploy log (live and stored).
func (l *deployLog) Logf(format string, args ...any) {
	if l == nil {
		return
	}
	// The sink never returns an error: a failed live publish must not fail a deploy.
	_, _ = fmt.Fprintf(l.stdout, format+"\n", args...)
}

// Stdout and Stderr are the writers a build's output streams into.
func (l *deployLog) Stdout() io.Writer {
	if l == nil {
		return io.Discard
	}
	return l.stdout
}

func (l *deployLog) Stderr() io.Writer {
	if l == nil {
		return io.Discard
	}
	return l.stderr
}

// store writes everything the sink holds to build_logs. Detached from ctx's
// cancellation: the failure paths that matter most run when the task's context may
// already be done.
func (l *deployLog) store(ctx context.Context) {
	if l == nil {
		return
	}
	l.storeMu.Lock()
	defer l.storeMu.Unlock()
	b := l.sink.NDJSON()
	if b == "" {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := l.q.UpdateDeploymentBuildLogs(wctx, generated.UpdateDeploymentBuildLogsParams{
		ID:        l.deploymentID,
		BuildLogs: pgtype.Text{String: b, Valid: true},
	}); err != nil {
		slog.Error("could not store the build logs", "error", err)
	}
}

// persist emits any partial line and stores the log. Call it BEFORE a status change the
// UI reacts to: when the status leaves "building" the UI stops reading the live stream
// and shows the stored blob, so the blob must already be complete.
func (l *deployLog) persist(ctx context.Context) {
	if l == nil {
		return
	}
	l.sink.Flush()
	l.store(ctx)
}

// close stops the flusher, stores the final log and tells live viewers the stream is
// over. Idempotent.
func (l *deployLog) close(ctx context.Context) {
	if l == nil {
		return
	}
	l.closeOnce.Do(func() {
		close(l.stopFlush)
		<-l.flushDone
		l.persist(ctx)
		_ = l.pub.Close(context.WithoutCancel(ctx))
	})
}

func (h *TaskHandler) closeDeployLog(ctx context.Context, dc *deployContext) { dc.log.close(ctx) }

// targetLabel is the stable name of one container in operator-facing text:
// {process_type}-{idx}. Container names churn on every roll and are not used here.
func targetLabel(t *deployTarget) string { return fmt.Sprintf("%s-%d", t.typeName, t.instanceIdx) }

func targetLabels(ts []*deployTarget) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, targetLabel(t))
	}
	return strings.Join(out, ", ")
}

// processSummary reads "web ×2, queue ×1": the routable type first, then the rest, each
// with its instance count. It is what the deploy is about to create.
func (dc *deployContext) processSummary() string {
	var order []string
	count := map[string]int{}
	for _, t := range append(dc.routables(), dc.workers...) {
		if t == nil {
			continue
		}
		if count[t.typeName] == 0 {
			order = append(order, t.typeName)
		}
		count[t.typeName]++
	}
	parts := make([]string, 0, len(order))
	for _, n := range order {
		parts = append(parts, fmt.Sprintf("%s ×%d", n, count[n]))
	}
	return strings.Join(parts, ", ")
}
