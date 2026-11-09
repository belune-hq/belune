package instances

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// MetricsInterval is how often live application metrics are sampled.
//
// ⚠️ A one-shot Docker stats call costs about TWO SECONDS no matter what: Docker samples
// twice, about a second apart, to compute the CPU delta. Measured 2026-11-09 on Docker
// 29.8.2 (OrbStack): 1.04s, 2.00s, 2.00s for three consecutive calls on an idle
// container. This is the third place that cost has had to be rediscovered (the project
// overview blew its 15s timeout from 8 apps; this broadcaster was permanently behind).
// So a 2s tick can never be met even with a perfect fan-out -- a new pass would start
// the moment the last one ended -- and the interval is deliberately longer than the
// floor. Fan-out (FetchStats) keeps one pass at about one call's time however many
// instances there are.
//
// A variable only so a test can shorten it; nothing else assigns it.
var MetricsInterval = 4 * time.Second

// MaxConcurrentStats bounds in-flight Docker stats calls in one pass.
const MaxConcurrentStats = 16

// StatsQuerier is the read the sampler needs.
type StatsQuerier interface {
	ListInstancesWithProcessTypeByApplication(ctx context.Context, applicationID pgtype.UUID) ([]generated.ListInstancesWithProcessTypeByApplicationRow, error)
}

// StatsRuntime is the slice of the container runtime the sampler calls.
type StatsRuntime interface {
	ContainerStats(ctx context.Context, containerID string) (*runtime.ContainerResourceStats, error)
}

// LiveInstances returns the instances worth sampling: those whose row says they are
// up. Docker answers a stats request for a STOPPED container with its last cgroup
// reading rather than zeroes -- a stopped application once reported a live,
// two-second-refreshing 117 MB it was not using -- so a replica that is not running
// must publish nothing, not a stale number. Per instance, not per application: a
// stopped replica must not blank its running siblings, nor borrow their liveness.
func LiveInstances(rows []generated.ListInstancesWithProcessTypeByApplicationRow) []generated.ListInstancesWithProcessTypeByApplicationRow {
	var out []generated.ListInstancesWithProcessTypeByApplicationRow
	for _, r := range rows {
		if r.Status == status.ApplicationRunning || r.Status == status.ApplicationUnhealthy {
			out = append(out, r)
		}
	}
	return out
}

// FetchStats samples every named container concurrently and returns the ones that
// answered, keyed by name. A failure drops only its own container.
//
// No errgroup context: one unreachable container must not cancel the rest and blank
// every other replica's numbers. Wall time is roughly ONE call (see MetricsInterval),
// not N.
func FetchStats(ctx context.Context, rt StatsRuntime, names []string) map[string]*runtime.ContainerResourceStats {
	out := make(map[string]*runtime.ContainerResourceStats, len(names))
	results := make([]*runtime.ContainerResourceStats, len(names))
	var g errgroup.Group
	g.SetLimit(MaxConcurrentStats)
	for i, name := range names {
		g.Go(func() error {
			s, err := rt.ContainerStats(ctx, name)
			if err != nil {
				slog.Debug("instance stats unavailable", "container", name, "error", err)
				return nil
			}
			results[i] = s // each goroutine owns its own slot
			return nil
		})
	}
	_ = g.Wait()
	for i, name := range names {
		if results[i] != nil {
			out[name] = results[i]
		}
	}
	return out
}

// InstancePoint is one instance's sample.
type InstancePoint struct {
	InstanceID     string  `json:"instance_id"`
	ProcessType    string  `json:"process_type"`
	Idx            int32   `json:"idx"`
	Routable       bool    `json:"routable"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryUsage    int64   `json:"memory_usage"`
	MemoryLimit    int64   `json:"memory_limit"`
	NetworkRxBytes int64   `json:"network_rx_bytes"`
	NetworkTxBytes int64   `json:"network_tx_bytes"`
}

// Point is one application sample: the totals the existing view has always shown, plus
// a point per instance for the question replicas create -- which one is hot.
//
// The totals are the SUM over the ROUTABLE type's instances that answered, i.e. what the
// view showed before replicas (one web container), extended to its replicas. Workers
// are in Instances, flagged, but not folded into the totals: an application with a
// queue worker must not suddenly report the worker's CPU as the web's. A total is null
// when no routable instance could be sampled -- "could not be determined", never zero.
type Point struct {
	CPUPercent     *float64        `json:"cpu_percent"`
	MemoryUsage    *int64          `json:"memory_usage"`
	MemoryLimit    *int64          `json:"memory_limit"`
	NetworkRxBytes *int64          `json:"network_rx_bytes"`
	NetworkTxBytes *int64          `json:"network_tx_bytes"`
	RecordedAt     string          `json:"recorded_at"`
	Instances      []InstancePoint `json:"instances"`
}

// BuildPoint assembles a Point from the live instances and the stats that came back.
// ok is false when nothing was sampled, in which case nothing should be published.
func BuildPoint(live []generated.ListInstancesWithProcessTypeByApplicationRow, stats map[string]*runtime.ContainerResourceStats, now time.Time) (Point, bool) {
	p := Point{RecordedAt: now.Format(time.RFC3339), Instances: []InstancePoint{}}
	var cpu float64
	var mem, memLimit, rx, tx int64
	routableSampled := false
	for _, r := range live { // already ordered: routable first, then type, then idx
		s, ok := stats[r.ContainerName]
		if !ok {
			continue
		}
		p.Instances = append(p.Instances, InstancePoint{
			InstanceID: uuidString(r.ID), ProcessType: r.ProcessTypeName, Idx: r.Idx, Routable: r.ProcessTypeRoutable,
			CPUPercent: s.CPUPercent, MemoryUsage: s.MemoryUsage, MemoryLimit: s.MemoryLimit,
			NetworkRxBytes: s.NetworkRxBytes, NetworkTxBytes: s.NetworkTxBytes,
		})
		if r.ProcessTypeRoutable {
			routableSampled = true
			cpu += s.CPUPercent
			mem += s.MemoryUsage
			memLimit += s.MemoryLimit
			rx += s.NetworkRxBytes
			tx += s.NetworkTxBytes
		}
	}
	if len(p.Instances) == 0 {
		return Point{}, false
	}
	if routableSampled {
		p.CPUPercent, p.MemoryUsage, p.MemoryLimit, p.NetworkRxBytes, p.NetworkTxBytes = &cpu, &mem, &memLimit, &rx, &tx
	}
	return p, true
}
