package caddymetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// exposition builds the slice of Caddy's /metrics this package reads. The counts
// are per status code on the reverse_proxy handler; the authentication handler is
// always present with big numbers to prove it is ignored.
func exposition(inFlight int, ok200, bad502 int, sumSeconds float64) string {
	return fmt.Sprintf(`# TYPE caddy_http_requests_in_flight gauge
caddy_http_requests_in_flight{handler="authentication",server="srv0"} 99
caddy_http_requests_in_flight{handler="reverse_proxy",server="srv0"} %d
# TYPE caddy_http_request_duration_seconds histogram
caddy_http_request_duration_seconds_bucket{code="200",handler="authentication",method="GET",server="srv0",le="+Inf"} 5000
caddy_http_request_duration_seconds_sum{code="200",handler="authentication",method="GET",server="srv0"} 9999
caddy_http_request_duration_seconds_count{code="200",handler="authentication",method="GET",server="srv0"} 5000
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",method="GET",server="srv0",le="0.1"} %d
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",method="GET",server="srv0",le="1"} %d
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",method="GET",server="srv0",le="+Inf"} %d
caddy_http_request_duration_seconds_sum{code="200",handler="reverse_proxy",method="GET",server="srv0"} %g
caddy_http_request_duration_seconds_count{code="200",handler="reverse_proxy",method="GET",server="srv0"} %d
caddy_http_request_duration_seconds_bucket{code="502",handler="reverse_proxy",method="GET",server="srv0",le="0.1"} 0
caddy_http_request_duration_seconds_bucket{code="502",handler="reverse_proxy",method="GET",server="srv0",le="1"} %d
caddy_http_request_duration_seconds_bucket{code="502",handler="reverse_proxy",method="GET",server="srv0",le="+Inf"} %d
caddy_http_request_duration_seconds_sum{code="502",handler="reverse_proxy",method="GET",server="srv0"} 0
caddy_http_request_duration_seconds_count{code="502",handler="reverse_proxy",method="GET",server="srv0"} %d
`, inFlight, ok200, ok200, ok200, sumSeconds, ok200, bad502, bad502, bad502)
}

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func TestParseReadsOnlyTheProxyHandler(t *testing.T) {
	s, err := Parse([]byte(exposition(3, 90, 10, 9)), t0)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Seen || s.InFlight != 3 || s.Count != 100 || s.Sum != 9 {
		t.Fatalf("snapshot = %+v (authentication's 99/5000 must not leak in)", s)
	}
	if s.Class["2xx"] != 90 || s.Class["5xx"] != 10 {
		t.Errorf("classes = %v", s.Class)
	}
}

func TestParseWithoutProxySeriesIsNotSeen(t *testing.T) {
	s, err := Parse([]byte("# TYPE caddy_admin_http_requests_total counter\ncaddy_admin_http_requests_total{code=\"200\"} 4\n"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen {
		t.Error("no reverse_proxy series must read as not seen")
	}
}

func TestBetweenComputesRatesAndQuantiles(t *testing.T) {
	a, _ := Parse([]byte(exposition(0, 100, 0, 5)), t0)
	// 10s later: 80 more OK requests (60 under 100ms, 20 between 100ms and 1s)
	// and 20 gateway failures.
	b, _ := Parse([]byte(exposition(2, 180, 20, 5+8)), t0.Add(10*time.Second))
	// Make the new OK requests' bucket split explicit: 100ms bucket gains 60.
	for i := range b.Buckets {
		if b.Buckets[i].UpperBound == 0.1 {
			b.Buckets[i].Cumulative = a.Buckets[0].Cumulative + 60
		}
	}

	r, err := Between(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if r.PerSecond != 10 || r.ServerErrorsPerSecond != 2 {
		t.Errorf("per second = %v, 5xx per second = %v", r.PerSecond, r.ServerErrorsPerSecond)
	}
	if r.Mean == nil || math.Abs(*r.Mean-0.08) > 1e-9 {
		t.Errorf("mean = %v, want 0.08", r.Mean)
	}
	if r.P95 == nil || *r.P95 <= 0.1 || *r.P95 > 1 {
		t.Errorf("p95 = %v, want within (0.1, 1]", r.P95)
	}
	if r.P50 == nil || *r.P50 > 0.1 {
		t.Errorf("p50 = %v, want <= 0.1", r.P50)
	}
}

// Idle windows have a rate (zero) but no latency: nil, not 0, or the page would
// claim the proxy answered in no time.
func TestBetweenWithNoNewRequestsHasNoLatency(t *testing.T) {
	a, _ := Parse([]byte(exposition(0, 100, 0, 5)), t0)
	b, _ := Parse([]byte(exposition(0, 100, 0, 5)), t0.Add(10*time.Second))
	r, err := Between(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if r.PerSecond != 0 || r.Mean != nil || r.P50 != nil || r.P95 != nil {
		t.Errorf("rates = %+v", r)
	}
}

func TestBetweenDetectsACaddyRestart(t *testing.T) {
	a, _ := Parse([]byte(exposition(0, 500, 0, 5)), t0)
	b, _ := Parse([]byte(exposition(0, 3, 0, 1)), t0.Add(10*time.Second))
	if _, err := Between(a, b); !errors.Is(err, ErrCounterReset) {
		t.Fatalf("err = %v, want ErrCounterReset", err)
	}
}

func TestQuantileInInfBucketIsALowerBound(t *testing.T) {
	got := quantile(0.99, []Bucket{{1, 1}, {math.Inf(1), 100}})
	if got == nil || *got != 1 {
		t.Fatalf("got %v, want 1 (largest finite bound)", got)
	}
}

type fakeSource struct {
	text    string
	err     error
	enabled bool
	enErr   error
}

func (f *fakeSource) Metrics(context.Context) ([]byte, error)      { return []byte(f.text), f.err }
func (f *fakeSource) MetricsEnabled(context.Context) (bool, error) { return f.enabled, f.enErr }

func TestObserverStates(t *testing.T) {
	ctx := context.Background()
	clock := t0
	newObs := func(src Source) *Observer {
		o := NewObserver(src)
		o.now = func() time.Time { return clock }
		return o
	}

	t.Run("nil source is unknown", func(t *testing.T) {
		if v := newObs(nil).Observe(ctx); v.State != StateUnknown {
			t.Errorf("state = %q", v.State)
		}
	})
	t.Run("admin API down is unreachable", func(t *testing.T) {
		v := newObs(&fakeSource{err: errors.New("refused")}).Observe(ctx)
		if v.State != StateUnreachable || v.Error == "" {
			t.Errorf("view = %+v", v)
		}
	})
	t.Run("metrics off is disabled, metrics on but idle is no_data", func(t *testing.T) {
		if v := newObs(&fakeSource{enabled: false}).Observe(ctx); v.State != StateDisabled {
			t.Errorf("disabled = %q", v.State)
		}
		if v := newObs(&fakeSource{enabled: true}).Observe(ctx); v.State != StateNoData {
			t.Errorf("idle = %q", v.State)
		}
	})
	t.Run("a failed config check is unknown, never disabled", func(t *testing.T) {
		v := newObs(&fakeSource{enErr: errors.New("boom")}).Observe(ctx)
		if v.State != StateUnknown {
			t.Errorf("state = %q, want unknown", v.State)
		}
	})
	t.Run("first sample collects, a later one reports a rate", func(t *testing.T) {
		src := &fakeSource{text: exposition(1, 100, 0, 5)}
		o := newObs(src)
		clock = t0
		if v := o.Observe(ctx); v.State != StateCollecting || v.PerSecond != nil || v.InFlight == nil {
			t.Fatalf("first = %+v", v)
		}
		src.text = exposition(1, 150, 0, 7)
		clock = t0.Add(10 * time.Second)
		v := o.Observe(ctx)
		if v.State != StateOK || v.PerSecond == nil || *v.PerSecond != 5 {
			t.Fatalf("second = %+v", v)
		}
	})
	t.Run("a restart drops back to collecting", func(t *testing.T) {
		src := &fakeSource{text: exposition(0, 500, 0, 5)}
		o := newObs(src)
		clock = t0
		o.Observe(ctx)
		src.text = exposition(0, 2, 0, 1)
		clock = t0.Add(10 * time.Second)
		if v := o.Observe(ctx); v.State != StateCollecting {
			t.Errorf("state = %q", v.State)
		}
	})
}
