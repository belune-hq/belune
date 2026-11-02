package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// otlptracehttp exposes no accessor for a built config, so these assert on the
// option set: which options were chosen is the decision under test, and the
// count distinguishes the two paths unambiguously (URL contributes exactly one
// option; host:port contributes one plus WithInsecure when the flag is set).
func optionCount(cfg Config) int { return len(exporterOptions(cfg)) }

// The bug this fixes: OTEL_EXPORTER_OTLP_ENDPOINT is a standard variable whose
// value the spec defines as a URL, but it was passed to WithEndpoint, which
// wants a bare host:port. Anyone following a vendor's documentation had their
// URL treated as a hostname.
func TestExporterOptions_AcceptsAUrl(t *testing.T) {
	opts := exporterOptions(Config{Endpoint: "https://api.honeycomb.io", Insecure: true})
	require.Len(t, opts, 1,
		"a URL should produce WithEndpointURL alone — Insecure must not be applied on top")
}

// Existing configuration must keep working: this project has always used the
// bare form, and the devcontainer and observability profile both write it.
func TestExporterOptions_StillAcceptsHostPort(t *testing.T) {
	assert.Equal(t, 2, optionCount(Config{Endpoint: "jaeger:4318", Insecure: true}),
		"host:port with Insecure should add WithInsecure")
	assert.Equal(t, 1, optionCount(Config{Endpoint: "jaeger:4318", Insecure: false}),
		"host:port without Insecure should not")
}

// The scheme is authoritative. OTEL_EXPORTER_OTLP_INSECURE defaults to true, so
// if the flag could still apply, every hosted endpoint an operator wrote as
// https:// would be silently downgraded to plaintext.
func TestExporterOptions_InsecureCannotOverrideAnExplicitScheme(t *testing.T) {
	withFlag := exporterOptions(Config{Endpoint: "https://api.honeycomb.io", Insecure: true})
	without := exporterOptions(Config{Endpoint: "https://api.honeycomb.io", Insecure: false})
	assert.Len(t, withFlag, len(without),
		"the Insecure flag must make no difference once a scheme is given")
}

func TestExporterOptions_PlainHttpUrlIsAlsoAUrl(t *testing.T) {
	assert.Len(t, exporterOptions(Config{Endpoint: "http://jaeger:4318", Insecure: false}), 1,
		"an http:// URL carries its own transport and needs no extra option")
}

var _ = otlptracehttp.WithInsecure // keep the import meaningful to readers

// Counting options shows which branch was taken but not that the result works.
// This drives a real exporter at a real URL and asserts the spans arrive, which
// is the behaviour the bug denied: with WithEndpoint, "http://127.0.0.1:PORT"
// was treated as a hostname and nothing was ever delivered.
func TestExporterOptions_UrlEndpointActuallyDelivers(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	shutdown, err := Init(context.Background(), Config{
		Endpoint:    srv.URL, // a full URL, the form the spec defines
		ServiceName: "test", ServiceVersion: "test",
	})
	require.NoError(t, err)

	_, span := Tracer().Start(context.Background(), "unit")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx)) // flushes the batch

	select {
	case path := <-got:
		assert.Equal(t, "/v1/traces", path,
			"the exporter should POST to the collector's traces path")
	case <-time.After(5 * time.Second):
		t.Fatal("no span reached the collector — the URL endpoint was not honoured")
	}
}

// deliveredPath points a real exporter at the collector's base URL plus
// pathSuffix and returns the path the collector was actually POSTed to. What
// arrives is the contract: otlptracehttp v1.45.0 changed what a path-less URL
// turns into, and no count of options would have shown it.
func deliveredPath(t *testing.T, pathSuffix string) string {
	t.Helper()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	shutdown, err := Init(context.Background(), Config{
		Endpoint:    srv.URL + pathSuffix,
		ServiceName: "test", ServiceVersion: "test",
	})
	require.NoError(t, err)

	_, span := Tracer().Start(context.Background(), "unit")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx)) // flushes the batch

	select {
	case path := <-got:
		return path
	case <-time.After(5 * time.Second):
		t.Fatalf("no span reached the collector for endpoint %q", srv.URL+pathSuffix)
		return ""
	}
}

// A trailing slash is the same collector as no slash, but before v1.45.0 it
// posted to "/" because the default path applied only to an empty path.
func TestExporterOptions_UrlEndpointWithTrailingSlashDelivers(t *testing.T) {
	assert.Equal(t, "/v1/traces", deliveredPath(t, "/"),
		"https://collector/ and https://collector are one endpoint and must reach the same path")
}

// withTracesPath must complete only a missing path. Appending to one the
// operator spelled out would turn the signal-specific form into /v1/traces/v1/traces.
func TestExporterOptions_ExplicitTracesPathIsNotDoubled(t *testing.T) {
	assert.Equal(t, "/v1/traces", deliveredPath(t, "/v1/traces"))
}

func TestWithTracesPath(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"no path":             {"https://api.honeycomb.io", "https://api.honeycomb.io/v1/traces"},
		"trailing slash":      {"https://api.honeycomb.io/", "https://api.honeycomb.io/v1/traces"},
		"plain http and port": {"http://jaeger:4318", "http://jaeger:4318/v1/traces"},
		"query is kept":       {"https://collector.example?x=1", "https://collector.example/v1/traces?x=1"},
		"explicit path":       {"https://collector.example/v1/traces", "https://collector.example/v1/traces"},
		"unparseable":         {"http://[::1", "http://[::1"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withTracesPath(tc.in))
		})
	}
}
