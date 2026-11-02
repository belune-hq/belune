// Package tracing wires OpenTelemetry tracing into the server. When
// OTEL_EXPORTER_OTLP_ENDPOINT is unset the package installs a no-op provider —
// tracing calls remain cheap and the program runs without an exporter.
//
// When the endpoint is set, spans are sent via OTLP/HTTP. This pairs with the
// Phase 4 observability stack (Prometheus + Grafana + optional Jaeger/Tempo).
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation scope used for every span produced by
// first-party server code.
const TracerName = "belune/api"

// Config controls exporter wiring. Zero value is safe — tracing remains no-op.
type Config struct {
	// Endpoint is an OTLP/HTTP endpoint. Accepts either a bare host:port
	// ("otel-collector:4318") or a full URL ("https://api.honeycomb.io"), which
	// is the form the OpenTelemetry specification defines. Empty = no-op.
	Endpoint string
	// Insecure skips TLS when talking to the collector. Applies only to the bare
	// host:port form — with a URL the scheme decides, and this is ignored.
	Insecure bool
	// ServiceName is reported as the resource service.name attribute.
	ServiceName string
	// ServiceVersion is reported as the resource service.version attribute.
	ServiceVersion string
}

// exporterOptions turns the configured endpoint into exporter options,
// accepting both the form this project has always used and the form the
// OpenTelemetry specification defines.
//
// OTEL_EXPORTER_OTLP_ENDPOINT is a standard variable and the spec says its value
// is a URL with a scheme. We passed it to WithEndpoint, which takes a bare
// "host:port" — so anyone following a vendor's own documentation and setting
// "https://api.honeycomb.io" had the string treated as a hostname. Every hosted
// collector was unreachable as a result.
//
// Both forms now work, so nothing configured against the old behaviour breaks:
//
//   - "jaeger:4318"              -> WithEndpoint, TLS decided by Insecure
//   - "https://api.honeycomb.io" -> WithEndpointURL, TLS decided by the scheme
//
// With a URL the scheme is authoritative and Insecure is not applied at all.
// Otherwise an explicit "https://" plus the default
// OTEL_EXPORTER_OTLP_INSECURE=true would silently downgrade a hosted endpoint to
// plaintext. That flag exists for bare host:port collectors on the loopback; it
// should not be able to override a scheme the operator wrote out.
func exporterOptions(cfg Config) []otlptracehttp.Option {
	if strings.Contains(cfg.Endpoint, "://") {
		return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(withTracesPath(cfg.Endpoint))}
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts
}

// withTracesPath completes an endpoint URL that names a host but no path.
//
// OTEL_EXPORTER_OTLP_ENDPOINT is the general variable: the spec has the SDK
// append /v1/traces to it, where the signal-specific ..._TRACES_ENDPOINT is
// used exactly as written. WithEndpointURL did the former until otlptracehttp
// v1.45.0 (opentelemetry-go #8538) changed it to the latter, so
// "https://api.honeycomb.io" started posting to "/" and every span was lost.
//
// "/" is completed too, though it was never right: before v1.45.0 the default
// path applied only to an empty path, so a trailing slash already posted to "/".
//
// Any other path is the operator's own and is passed through untouched. A URL
// that does not parse is also passed through, so WithEndpointURL rejects it
// exactly as it did before.
func withTracesPath(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return endpoint
	}
	return u.JoinPath("/v1/traces").String()
}

// ShutdownFunc flushes buffered spans and releases exporter resources.
type ShutdownFunc func(context.Context) error

// Init installs a global tracer provider and W3C propagator. Returns a
// shutdown function that must be called before process exit.
//
// When cfg.Endpoint is empty, a no-op provider is installed and the returned
// shutdown function is a cheap no-op. Callers do not need to branch on this.
func Init(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	// Route OTel's own errors through slog before anything can fail. Installed
	// unconditionally: it costs nothing with no exporter, and forgetting it in
	// the configured branch is how export failures end up at Info.
	SetErrorHandler()

	// Always install the W3C propagator so callers can inject/extract even
	// when no exporter is configured — the context still flows through.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		slog.Info("tracing: no OTLP endpoint configured, using no-op provider")
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(exporterOptions(cfg)...))
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// Pass an empty schema URL on our resource so Merge adopts resource.Default()'s
	// SchemaURL. Hard-coding a specific semconv version here causes a merge
	// conflict whenever the OTel SDK bumps its default schema.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"",
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Report the transport actually in use rather than the flag. With a URL the
	// scheme decides, so echoing insecure=true beside an https:// endpoint would
	// state the opposite of what the exporter is doing.
	if strings.Contains(cfg.Endpoint, "://") {
		slog.Info("tracing: OTLP exporter configured", "endpoint", cfg.Endpoint)
	} else {
		slog.Info("tracing: OTLP exporter configured", "endpoint", cfg.Endpoint, "insecure", cfg.Insecure)
	}

	return func(shutdownCtx context.Context) error {
		return tp.Shutdown(shutdownCtx)
	}, nil
}

// Tracer returns the package's named tracer. Safe to call before Init — the
// default no-op provider satisfies the interface.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// InjectContext serializes the active trace context from ctx into a flat
// string map suitable for storing in an asynq task payload.
//
// Returns an empty map if no span is active.
func InjectContext(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

// ExtractContext deserializes a carrier produced by InjectContext back into a
// context that can be used to start child spans.
func ExtractContext(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}
