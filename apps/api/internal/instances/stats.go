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

// EffectiveLimits is the ONE rule for the CPU and memory limit a container is created
// with: the process type's own when set (NULL on the type means inherit), otherwise the
// application's. 0 is unlimited, as on the application. The deploy path and the metrics
// ceiling both go through it -- a second copy would let "what we created it with" and
// "what we report as its ceiling" drift apart.
func EffectiveLimits(appCPU float64, appMemory int64, ptCPU pgtype.Float8, ptMemory pgtype.Int8) (cpu float64, memory int64) {
	cpu, memory = appCPU, appMemory
	if ptCPU.Valid {
		cpu = ptCPU.Float64
	}
	if ptMemory.Valid {
		memory = ptMemory.Int64
	}
	return cpu, memory
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
//
// COVERAGE. A total is a sum of the instances that answered, so with 3 replicas of which
// 2 answered it is a confident-looking sum of 2. The counts say so, one pair per scope
// because the scopes differ and a single pair mixing them would be its own small lie:
//
//	RoutableSampled / RoutableExpected -- what the totals sum over
//	Sampled / Expected                 -- every type, i.e. what Instances lists
//
// ⚠️ "Expected" is the set the CALLER passed in, not the set that exists. The stream
// passes LiveInstances, which filters on the ROW's status, and the row is eventually
// consistent: an instance that is genuinely up while its row still says "pending" is in
// NEITHER count -- invisible, not under-reported. The counts describe coverage of the
// intended set. "4 of 4" does not mean "we saw everything that exists".
//
// MemoryLimit is the application's CEILING, and is CONFIGURATION, not the sample: the
// sum of each routable instance's effective limit (EffectiveLimits), or null when any of
// them is unlimited. Docker reports the HOST's total memory as the cgroup limit of a
// container with no limit, indistinguishable in the stats from "limited to exactly host
// RAM", so a sampled sum would read 24 GB for three unlimited replicas on an 8 GB host.
// It spans the whole expected routable set, not just the replicas that answered: a
// replica that missed a sample still has a ceiling, and it is reported even when nothing
// answered. Null therefore means ONLY "a routable replica is unlimited".
//
// ⚠️ Usage sums the SAMPLED routable set while the ceiling spans the EXPECTED one, so a
// usage/ceiling ratio mixes denominators and understates utilisation under partial
// coverage (two replicas' usage against three replicas' ceiling). It is only safe when
// RoutableSampled == RoutableExpected; check the counts before drawing a percentage.
type Point struct {
	CPUPercent       *float64        `json:"cpu_percent"`
	MemoryUsage      *int64          `json:"memory_usage"`
	MemoryLimit      *int64          `json:"memory_limit"`
	NetworkRxBytes   *int64          `json:"network_rx_bytes"`
	NetworkTxBytes   *int64          `json:"network_tx_bytes"`
	RoutableSampled  int             `json:"routable_sampled"`
	RoutableExpected int             `json:"routable_expected"`
	Sampled          int             `json:"sampled"`
	Expected         int             `json:"expected"`
	RecordedAt       string          `json:"recorded_at"`
	Instances        []InstancePoint `json:"instances"`
}

// BuildPoint assembles a Point from the live instances and the stats that came back.
// ok is false when nothing was sampled, in which case nothing should be published; the
// returned Point still carries the counts (zero sampled, the expected sizes).
func BuildPoint(live []generated.ListInstancesWithProcessTypeByApplicationRow, stats map[string]*runtime.ContainerResourceStats, now time.Time) (Point, bool) {
	p := Point{RecordedAt: now.Format(time.RFC3339), Instances: []InstancePoint{}, Expected: len(live)}
	var cpu float64
	var mem, rx, tx int64
	var ceiling int64
	ceilingKnown := true
	for _, r := range live {
		if r.ProcessTypeRoutable {
			p.RoutableExpected++
			if _, m := EffectiveLimits(r.ApplicationCpuLimit, r.ApplicationMemoryLimit, r.ProcessTypeCpuLimit, r.ProcessTypeMemoryLimit); m > 0 {
				ceiling += m
			} else {
				ceilingKnown = false // one unlimited replica makes the ceiling unknown, not the sum of the rest
			}
		}
	}
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
			p.RoutableSampled++
			cpu += s.CPUPercent
			mem += s.MemoryUsage
			rx += s.NetworkRxBytes
			tx += s.NetworkTxBytes
		}
	}
	p.Sampled = len(p.Instances)
	if p.Sampled == 0 {
		return p, false
	}
	if p.RoutableSampled > 0 {
		p.CPUPercent, p.MemoryUsage, p.NetworkRxBytes, p.NetworkTxBytes = &cpu, &mem, &rx, &tx
	}
	// The ceiling is configuration, not a measurement, so it does not wait on sampling:
	// null means exactly "at least one routable replica is unlimited". The
	// RoutableExpected guard is load-bearing -- with no routable instance the loop never
	// ran, ceiling is 0 and ceilingKnown is still true, and publishing &0 would read as
	// "unlimited", which is what 0 means everywhere else.
	if ceilingKnown && p.RoutableExpected > 0 {
		p.MemoryLimit = &ceiling
	}
	return p, true
}
