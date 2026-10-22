// Package caddymetrics turns Caddy's Prometheus exposition into the handful of
// numbers the Proxy page shows: how much traffic Caddy is proxying to apps, how
// fast, and how much of it fails.
//
// Only the reverse_proxy handler is read. Caddy instruments each handler in a
// route separately, so a request that passes through basic auth AND the reverse
// proxy appears in both series; summing handlers would count it twice and there
// is no series that counts each request once. reverse_proxy is the one that
// means "reached an app", and its 5xx are the gateway failures an operator
// cares about.
package caddymetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const proxyHandler = "reverse_proxy"

const (
	famInFlight = "caddy_http_requests_in_flight"
	famDuration = "caddy_http_request_duration_seconds"
)

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	UpperBound float64
	Cumulative float64
}

// Snapshot is the reverse_proxy handler's counters at one instant. All counters
// are cumulative since Caddy started, so a single snapshot has no rate in it.
type Snapshot struct {
	At time.Time
	// Seen is false when Caddy has recorded nothing for the handler yet.
	Seen     bool
	InFlight float64
	Count    float64
	Sum      float64
	Buckets  []Bucket
	// Class counts responses by status class ("2xx" … "5xx").
	Class map[string]float64
}

// Parse reads the exposition text.
func Parse(text []byte, at time.Time) (Snapshot, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(strings.NewReader(string(text)))
	if err != nil {
		return Snapshot{}, fmt.Errorf("parsing caddy metrics: %w", err)
	}

	s := Snapshot{At: at, Class: map[string]float64{}}
	if f := fams[famInFlight]; f != nil {
		for _, m := range f.Metric {
			if label(m, "handler") == proxyHandler && m.Gauge != nil {
				s.InFlight += m.Gauge.GetValue()
				s.Seen = true
			}
		}
	}

	byBound := map[float64]float64{}
	if f := fams[famDuration]; f != nil {
		for _, m := range f.Metric {
			h := m.Histogram
			if label(m, "handler") != proxyHandler || h == nil {
				continue
			}
			s.Seen = true
			n := float64(h.GetSampleCount())
			s.Count += n
			s.Sum += h.GetSampleSum()
			if code := label(m, "code"); code != "" {
				s.Class[code[:1]+"xx"] += n
			}
			for _, b := range h.Bucket {
				byBound[b.GetUpperBound()] += float64(b.GetCumulativeCount())
			}
		}
	}
	for bound, c := range byBound {
		s.Buckets = append(s.Buckets, Bucket{UpperBound: bound, Cumulative: c})
	}
	sort.Slice(s.Buckets, func(i, j int) bool { return s.Buckets[i].UpperBound < s.Buckets[j].UpperBound })
	return s, nil
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// Rates is the change between two snapshots.
type Rates struct {
	Window                time.Duration
	PerSecond             float64
	ServerErrorsPerSecond float64
	// Mean, P50 and P95 are nil when no request completed in the window: there is
	// no latency to report, which is not the same as zero latency.
	Mean, P50, P95 *float64
}

// ErrCounterReset means a counter went backwards — Caddy restarted — so the
// pair describes nothing.
var ErrCounterReset = errors.New("counters reset")

// Between computes the rates from old to new.
func Between(old, cur Snapshot) (Rates, error) {
	span := cur.At.Sub(old.At)
	if span <= 0 {
		return Rates{}, errors.New("snapshots are not in order")
	}
	if cur.Count < old.Count {
		return Rates{}, ErrCounterReset
	}
	secs := span.Seconds()
	n := cur.Count - old.Count
	r := Rates{
		Window:                span,
		PerSecond:             n / secs,
		ServerErrorsPerSecond: (cur.Class["5xx"] - old.Class["5xx"]) / secs,
	}
	if r.ServerErrorsPerSecond < 0 {
		return Rates{}, ErrCounterReset
	}
	if n > 0 {
		mean := (cur.Sum - old.Sum) / n
		r.Mean = &mean
		delta := bucketDelta(old.Buckets, cur.Buckets)
		r.P50 = quantile(0.50, delta)
		r.P95 = quantile(0.95, delta)
	}
	return r, nil
}

// bucketDelta subtracts old from cur bound by bound. A bound present only in cur
// (a bucket that first received an observation) counts from zero.
func bucketDelta(old, cur []Bucket) []Bucket {
	prev := make(map[float64]float64, len(old))
	for _, b := range old {
		prev[b.UpperBound] = b.Cumulative
	}
	out := make([]Bucket, len(cur))
	for i, b := range cur {
		out[i] = Bucket{UpperBound: b.UpperBound, Cumulative: b.Cumulative - prev[b.UpperBound]}
	}
	return out
}

// quantile estimates q from cumulative buckets by linear interpolation inside the
// bucket that holds it — the same estimate Prometheus's histogram_quantile makes,
// and as coarse as the bucket layout. A result in the +Inf bucket is reported as
// the largest finite bound: "at least this", the only honest finite number.
func quantile(q float64, buckets []Bucket) *float64 {
	if len(buckets) == 0 {
		return nil
	}
	total := buckets[len(buckets)-1].Cumulative
	if total <= 0 {
		return nil
	}
	rank := q * total
	lowerBound, lowerCount := 0.0, 0.0
	for _, b := range buckets {
		if b.Cumulative >= rank {
			if math.IsInf(b.UpperBound, 1) {
				return &lowerBound
			}
			inBucket := b.Cumulative - lowerCount
			v := b.UpperBound
			if inBucket > 0 {
				v = lowerBound + (b.UpperBound-lowerBound)*(rank-lowerCount)/inBucket
			}
			return &v
		}
		lowerBound, lowerCount = b.UpperBound, b.Cumulative
	}
	return &lowerBound
}

// Source is the slice of the Caddy client the observer needs.
type Source interface {
	Metrics(ctx context.Context) ([]byte, error)
	MetricsEnabled(ctx context.Context) (bool, error)
}

// States of a ProxyTraffic.
const (
	StateOK          = "ok"          // rates computed
	StateCollecting  = "collecting"  // data seen, too soon after the first sample for a rate
	StateNoData      = "no_data"     // metrics are on, nothing proxied since Caddy started
	StateDisabled    = "disabled"    // Caddy is not recording HTTP metrics
	StateUnreachable = "unreachable" // the admin API did not answer
	StateUnknown     = "unknown"     // could not tell (no source, or the config check failed)
)

const (
	// minSpan is the shortest window a rate is quoted over; shorter and a single
	// request swings it wildly.
	minSpan = 5 * time.Second
	// maxSpan is how far back the baseline reaches, so the rate means "lately".
	maxSpan = 60 * time.Second
)

// ProxyTraffic is what the endpoint returns. Pointers are nil when the value could not
// be determined, which the page must show as unknown, never as zero.
type ProxyTraffic struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// InFlight is requests being proxied right now.
	InFlight *float64 `json:"in_flight"`
	// Proxied is the lifetime count since Caddy last started.
	Proxied       *float64 `json:"proxied_total"`
	WindowSeconds *float64 `json:"window_seconds"`
	PerSecond     *float64 `json:"requests_per_second"`
	// ServerErrorsPerSecond counts 5xx responses from the proxy handler: gateway
	// failures and app errors alike.
	ServerErrorsPerSecond *float64           `json:"server_errors_per_second"`
	MeanSeconds           *float64           `json:"mean_seconds"`
	P50Seconds            *float64           `json:"p50_seconds"`
	P95Seconds            *float64           `json:"p95_seconds"`
	StatusClasses         map[string]float64 `json:"status_classes"`
}

// Observer keeps a short history of snapshots so rates can be computed across
// requests. In memory by design: a rate only needs the last minute, and losing
// it on restart costs one "collecting" reading.
type Observer struct {
	src Source
	now func() time.Time

	mu   sync.Mutex
	ring []Snapshot
}

func NewObserver(src Source) *Observer {
	return &Observer{src: src, now: time.Now}
}

// Observe scrapes Caddy and reports.
func (o *Observer) Observe(ctx context.Context) ProxyTraffic {
	if o == nil || o.src == nil {
		return ProxyTraffic{State: StateUnknown}
	}
	text, err := o.src.Metrics(ctx)
	if err != nil {
		return ProxyTraffic{State: StateUnreachable, Error: err.Error()}
	}
	snap, err := Parse(text, o.now())
	if err != nil {
		return ProxyTraffic{State: StateUnknown, Error: err.Error()}
	}

	if !snap.Seen {
		// Either metrics are off or nothing has been proxied yet; only the config
		// can say which, and claiming "disabled" on a quiet install would send
		// someone to edit a Caddyfile that was fine.
		on, err := o.src.MetricsEnabled(ctx)
		switch {
		case err != nil:
			return ProxyTraffic{State: StateUnknown, Error: err.Error()}
		case !on:
			return ProxyTraffic{State: StateDisabled}
		}
		return ProxyTraffic{State: StateNoData}
	}

	view := ProxyTraffic{
		State:         StateCollecting,
		InFlight:      &snap.InFlight,
		Proxied:       &snap.Count,
		StatusClasses: snap.Class,
	}

	base, ok := o.advance(snap)
	if !ok {
		return view
	}
	rates, err := Between(base, snap)
	if err != nil {
		if errors.Is(err, ErrCounterReset) {
			o.reset(snap)
		}
		return view
	}
	view.State = StateOK
	w := rates.Window.Seconds()
	view.WindowSeconds = &w
	view.PerSecond = &rates.PerSecond
	view.ServerErrorsPerSecond = &rates.ServerErrorsPerSecond
	view.MeanSeconds, view.P50Seconds, view.P95Seconds = rates.Mean, rates.P50, rates.P95
	return view
}

// advance records snap and returns the baseline to measure it against: the oldest
// snapshot still inside maxSpan, provided it is at least minSpan old.
func (o *Observer) advance(snap Snapshot) (Snapshot, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	kept := o.ring[:0]
	for _, s := range o.ring {
		if snap.At.Sub(s.At) <= maxSpan {
			kept = append(kept, s)
		}
	}
	o.ring = append(kept, snap)

	for _, s := range o.ring {
		if snap.At.Sub(s.At) >= minSpan {
			return s, true
		}
	}
	return Snapshot{}, false
}

func (o *Observer) reset(snap Snapshot) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ring = []Snapshot{snap}
}
