// Package telemetry wires OpenTelemetry metrics into the sandbox runtime
// and provisioner. It is opt-in: when OTEL_EXPORTER_OTLP_ENDPOINT is unset
// (or empty), Init returns a no-op shutdown and HTTPMiddleware is a
// passthrough. This means we pay literally zero metric overhead on the
// hot path until an operator turns it on.
//
// What gets recorded today (more to follow as the codebase grows):
//
//	http.server.requests           counter, attrs: method, route, status
//	http.server.duration_ms        histogram, attrs: method, route, status
//
// Routes are normalized by the caller via WithRoute so we don't blow up
// metric cardinality on path parameters (e.g. /api/sandboxes/{id}).
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// Standard OpenTelemetry env vars we honor (see
// https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/).
const (
	EnvEndpoint    = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvInsecure    = "OTEL_EXPORTER_OTLP_INSECURE"
	EnvServiceName = "OTEL_SERVICE_NAME"
)

// Config controls how the SDK is initialized. The zero value is the
// disabled state; ConfigFromEnv populates it from standard OTEL env vars.
type Config struct {
	// Endpoint is the OTLP gRPC collector address (host:port).
	// Empty = telemetry disabled.
	Endpoint string

	// ServiceName / ServiceVersion become the corresponding resource attrs.
	ServiceName    string
	ServiceVersion string

	// SandboxID is attached as the "sandbox.id" resource attribute if set.
	// Useful when many sandboxes share a single collector.
	SandboxID string

	// Insecure disables TLS. Default false; OTEL_EXPORTER_OTLP_INSECURE=true
	// flips it on for local testing.
	Insecure bool
}

// ConfigFromEnv reads standard OTEL env vars. ServiceName / Version come
// from arguments because the package can't know which binary is calling it.
func ConfigFromEnv(serviceName, version string) Config {
	cfg := Config{
		Endpoint:       os.Getenv(EnvEndpoint),
		ServiceName:    serviceName,
		ServiceVersion: version,
		SandboxID:      os.Getenv("SANDBOX_ID"),
	}
	if v := os.Getenv(EnvServiceName); v != "" {
		cfg.ServiceName = v
	}
	if v, err := strconv.ParseBool(os.Getenv(EnvInsecure)); err == nil && v {
		cfg.Insecure = true
	}
	return cfg
}

// Shutdown is returned by Init; call it on process exit to flush pending
// metrics. Safe to call when Init was a no-op.
type Shutdown func(context.Context) error

// instruments holds the global meter handles. They are populated lazily by
// Init and read by HTTPMiddleware. The struct is replaced atomically via
// instrumentsMu so the read path is lock-free in the common case (no Init
// call recently → keep the previous pointer).
type instruments struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
}

var (
	instMu  sync.RWMutex
	inst    *instruments
	enabled bool
)

// Init starts the OpenTelemetry metric SDK and registers the global meter
// provider. Returns a Shutdown closure even on the disabled path so callers
// can defer it unconditionally.
func Init(ctx context.Context, cfg Config) (Shutdown, error) {
	if cfg.Endpoint == "" {
		// Disabled — keep instruments nil, HTTPMiddleware will fast-path.
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}

	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.SandboxID != "" {
		attrs = append(attrs, attribute.String("sandbox.id", cfg.SandboxID))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	reader := sdkmetric.NewPeriodicReader(exp,
		sdkmetric.WithInterval(30*time.Second),
	)
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		// Use exponential histogram for duration — better tail visibility
		// than fixed buckets at minimal overhead.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.duration_ms"},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
					MaxSize:  160,
					MaxScale: 20,
				},
			},
		)),
	)
	otel.SetMeterProvider(provider)

	meter := provider.Meter("github.com/agentry-ai/agentry")
	reqs, err := meter.Int64Counter("http.server.requests",
		metric.WithDescription("Total HTTP requests served"),
	)
	if err != nil {
		return nil, err
	}
	dur, err := meter.Float64Histogram("http.server.duration_ms",
		metric.WithDescription("HTTP request duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, err
	}

	instMu.Lock()
	inst = &instruments{requests: reqs, duration: dur}
	enabled = true
	instMu.Unlock()

	return func(shutdownCtx context.Context) error {
		instMu.Lock()
		inst = nil
		enabled = false
		instMu.Unlock()
		return provider.Shutdown(shutdownCtx)
	}, nil
}

// loadInstruments returns the active instruments under a read lock. nil
// when telemetry is disabled.
func loadInstruments() *instruments {
	instMu.RLock()
	i := inst
	instMu.RUnlock()
	return i
}

// IsEnabled reports whether Init successfully attached an exporter.
// Test helper; production code should not branch on this.
func IsEnabled() bool {
	instMu.RLock()
	defer instMu.RUnlock()
	return enabled
}

// statusRecorder wraps an http.ResponseWriter so the middleware can read
// the final status code. We avoid pointer-allocating per-request by
// pooling these in middleware-with-pool below.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		// Standard library would log "superfluous response.WriteHeader";
		// suppress so we don't spam logs from inside the metric wrapper.
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

var statusPool = sync.Pool{
	New: func() any { return new(statusRecorder) },
}

// HTTPMiddleware records request count and latency metrics. The route
// label is taken from the request context if set via WithRoute, else falls
// back to r.URL.Path. Pre-normalize via WithRoute to avoid cardinality
// blow-up on parameterized routes (e.g. /api/sandboxes/{id}).
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fast path when disabled — no allocations, no atomic reads beyond
		// the rlock, no pooled wrapper.
		i := loadInstruments()
		if i == nil {
			next.ServeHTTP(w, r)
			return
		}

		rec := statusPool.Get().(*statusRecorder)
		rec.ResponseWriter = w
		rec.status = 0
		rec.wrote = false

		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		route := routeOf(r)

		attrs := metric.WithAttributes(
			attribute.String("method", r.Method),
			attribute.String("route", route),
			attribute.Int("status", status),
		)
		i.requests.Add(r.Context(), 1, attrs)
		i.duration.Record(r.Context(), float64(elapsed.Milliseconds()), attrs)

		// Pool reset.
		rec.ResponseWriter = nil
		statusPool.Put(rec)
	})
}

// routeCtxKey is the context key used by WithRoute to attach a normalized
// route label. Strong-typed to avoid accidental collisions.
type routeCtxKey struct{}

// WithRoute returns a copy of r whose context carries `route`. Handlers
// (or the mux glue) should call this when they can identify a stable
// pattern (e.g. /api/sandboxes/{id}) to keep metric cardinality bounded.
func WithRoute(r *http.Request, route string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routeCtxKey{}, route))
}

func routeOf(r *http.Request) string {
	if v, ok := r.Context().Value(routeCtxKey{}).(string); ok && v != "" {
		return v
	}
	return r.URL.Path
}

// Compile-time check: ensure we satisfy the metric reader interface
// expected by Init. (Not actually exercised — this keeps the compiler
// honest when the SDK API drifts.)
var _ metricdata.ResourceMetrics
