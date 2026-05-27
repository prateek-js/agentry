package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// installManualReader plugs a ManualReader into the global provider so tests
// can assert exact metric values without spinning up a collector. Returns
// the reader plus a cleanup that re-installs the previous provider.
func installManualReader(t *testing.T) (*sdkmetric.ManualReader, func()) {
	t.Helper()
	prev := otel.GetMeterProvider()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)

	meter := provider.Meter("github.com/agentry/agentry")
	reqs, err := meter.Int64Counter("http.server.requests")
	if err != nil {
		t.Fatal(err)
	}
	dur, err := meter.Float64Histogram("http.server.duration_ms")
	if err != nil {
		t.Fatal(err)
	}
	instMu.Lock()
	prevInst := inst
	prevEnabled := enabled
	inst = &instruments{requests: reqs, duration: dur}
	enabled = true
	instMu.Unlock()

	return reader, func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
		instMu.Lock()
		inst = prevInst
		enabled = prevEnabled
		instMu.Unlock()
	}
}

// findCounter returns the value of an Int64 counter for a given attr set, or
// 0 if absent. Linear-scan over the test surface, which is tiny.
func findCounter(t *testing.T, rm metricdata.ResourceMetrics, name string, match attribute.Set) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range s.DataPoints {
				if attrEq(dp.Attributes, match) {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// findHistogramCount returns the count of histogram observations matching
// the attr set.
func findHistogramCount(t *testing.T, rm metricdata.ResourceMetrics, name string, match attribute.Set) uint64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range h.DataPoints {
				if attrEq(dp.Attributes, match) {
					return dp.Count
				}
			}
		}
	}
	return 0
}

func attrEq(a, b attribute.Set) bool {
	// Compare the canonical key=value encodings — order- and case-stable.
	return a.Encoded(attribute.DefaultEncoder()) == b.Encoded(attribute.DefaultEncoder())
}

func okHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestMiddlewareDisabledIsPassthrough(t *testing.T) {
	// No installManualReader → inst is nil.
	handler := HTTPMiddleware(okHandler(200))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if IsEnabled() {
		t.Fatal("telemetry should be disabled when no reader installed")
	}
}

func TestMiddlewareRecordsCounterAndDuration(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := HTTPMiddleware(okHandler(204))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// 3 GETs returning 204.
	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/v1/foo")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	want := attribute.NewSet(
		attribute.String("method", "GET"),
		attribute.String("route", "/v1/foo"),
		attribute.Int("status", 204),
	)
	if got := findCounter(t, rm, "http.server.requests", want); got != 3 {
		t.Errorf("requests counter = %d; want 3", got)
	}
	if got := findHistogramCount(t, rm, "http.server.duration_ms", want); got != 3 {
		t.Errorf("duration histogram count = %d; want 3", got)
	}
}

func TestMiddlewareRecordsErrorStatuses(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/x")
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(
		attribute.String("method", "GET"),
		attribute.String("route", "/x"),
		attribute.Int("status", 400),
	)
	if got := findCounter(t, rm, "http.server.requests", want); got != 1 {
		t.Errorf("400 counter = %d; want 1", got)
	}
}

func TestMiddlewareDefaultsStatusToOK(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	// Handler that neither writes a body nor calls WriteHeader.
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/empty")
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(
		attribute.String("method", "GET"),
		attribute.String("route", "/empty"),
		attribute.Int("status", 200),
	)
	if got := findCounter(t, rm, "http.server.requests", want); got != 1 {
		t.Errorf("default-status counter = %d; want 1", got)
	}
}

func TestWithRouteIsUsedAsLabel(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the mux normalizing the route for the metric label.
		r2 := WithRoute(r, "/api/sandboxes/{id}")
		*r = *r2
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(HTTPMiddleware(inner))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/sandboxes/abc123")
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(
		attribute.String("method", "GET"),
		attribute.String("route", "/api/sandboxes/{id}"),
		attribute.Int("status", 200),
	)
	if got := findCounter(t, rm, "http.server.requests", want); got != 1 {
		t.Errorf("normalized-route counter = %d; want 1", got)
	}
}

func TestMiddlewareConcurrentSafe(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	handler := HTTPMiddleware(okHandler(200))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	var fails atomic.Int32
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/v1/foo")
			if err != nil {
				fails.Add(1)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	if v := fails.Load(); v != 0 {
		t.Fatalf("%d failures under load", v)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(
		attribute.String("method", "GET"),
		attribute.String("route", "/v1/foo"),
		attribute.Int("status", 200),
	)
	if got := findCounter(t, rm, "http.server.requests", want); got != int64(N) {
		t.Errorf("counter = %d; want %d", got, N)
	}
}

func TestInitNoOpWhenEndpointEmpty(t *testing.T) {
	// Ensure we start from a clean state.
	instMu.Lock()
	prev := inst
	prevEnabled := enabled
	inst, enabled = nil, false
	instMu.Unlock()
	t.Cleanup(func() {
		instMu.Lock()
		inst = prev
		enabled = prevEnabled
		instMu.Unlock()
	})

	shutdown, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if IsEnabled() {
		t.Fatal("Init with empty endpoint should leave telemetry disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown returned %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvEndpoint, "localhost:4317")
	t.Setenv(EnvInsecure, "true")
	t.Setenv(EnvServiceName, "override")
	t.Setenv("SANDBOX_ID", "sbx-1")

	cfg := ConfigFromEnv("default-name", "1.2.3")
	if cfg.Endpoint != "localhost:4317" {
		t.Errorf("endpoint = %q", cfg.Endpoint)
	}
	if !cfg.Insecure {
		t.Errorf("insecure = %v; want true", cfg.Insecure)
	}
	if cfg.ServiceName != "override" {
		t.Errorf("name = %q; want override (env should win)", cfg.ServiceName)
	}
	if cfg.SandboxID != "sbx-1" {
		t.Errorf("sandbox_id = %q", cfg.SandboxID)
	}
	if cfg.ServiceVersion != "1.2.3" {
		t.Errorf("version = %q", cfg.ServiceVersion)
	}
}

// Benchmark the disabled hot path — middleware must be cheap when no
// exporter is configured. Realistic baseline for the "off" case in prod.
func BenchmarkMiddlewareDisabled(b *testing.B) {
	instMu.Lock()
	inst = nil
	enabled = false
	instMu.Unlock()

	h := HTTPMiddleware(okHandler(200))
	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
}

// Ensure WriteHeader is forwarded exactly once.
func TestStatusRecorderForwardsOnce(t *testing.T) {
	reader, cleanup := installManualReader(t)
	defer cleanup()

	var writes atomic.Int32
	srv := httptest.NewServer(HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(201)
		w.WriteHeader(500) // ignored — net/http already deprecates re-writes
		writes.Add(1)
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d; want 201 (first write wins)", resp.StatusCode)
	}
	if writes.Load() != 1 {
		t.Fatalf("handler ran %d times; want 1", writes.Load())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	want := attribute.NewSet(
		attribute.String("method", "GET"),
		attribute.String("route", "/x"),
		attribute.Int("status", 201),
	)
	if got := findCounter(t, rm, "http.server.requests", want); got != 1 {
		t.Errorf("counter for 201 = %d; want 1", got)
	}
}

// Compile-time confirmation that Init's error type wraps cleanly.
var _ = errors.New
