package provisioner

import (
	"context"
	"testing"
	"time"
)

// seed creates n sandboxes in the mock with the given TTL applied at startTime.
// Returns the list of pod names.
func seed(t *testing.T, m *MockBackend, n int, startTime time.Time, ttlSec int64) []string {
	t.Helper()
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := "s" + itoa(i)
		ann := ttlAnnotations(startTime, ttlSec)
		spec := SandboxSpec{SandboxID: id, NodeHost: "node", Annotations: ann}
		if err := m.CreatePod(context.Background(), "default", spec); err != nil {
			t.Fatal(err)
		}
		if err := m.CreateService(context.Background(), "default", spec); err != nil {
			t.Fatal(err)
		}
		names = append(names, "sandbox-"+id)
	}
	return names
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestReaperDeletesOnlyExpired(t *testing.T) {
	mock := NewMockBackend()

	// Three sandboxes: one already expired (ttl=10s, started 1h ago),
	// one with no TTL, one fresh.
	start := time.Now().Add(-time.Hour)
	_ = seed(t, mock, 1, start, 10) // expired
	noTTL := SandboxSpec{SandboxID: "noTTL", NodeHost: "node"}
	_ = mock.CreatePod(context.Background(), "default", noTTL)
	_ = mock.CreateService(context.Background(), "default", noTTL)
	fresh := SandboxSpec{SandboxID: "fresh", NodeHost: "node",
		Annotations: ttlAnnotations(time.Now(), 3600)}
	_ = mock.CreatePod(context.Background(), "default", fresh)
	_ = mock.CreateService(context.Background(), "default", fresh)

	if got := mock.PodCount(); got != 3 {
		t.Fatalf("setup: want 3 pods, got %d", got)
	}

	r := NewReaper(mock, "default", time.Minute)
	r.sweep(context.Background())

	if got := mock.PodCount(); got != 2 {
		t.Fatalf("after sweep: want 2 pods (one expired culled), got %d", got)
	}
	if _, ok := mock.Spec("sandbox-s0"); ok {
		t.Errorf("expired pod still present")
	}
	if _, ok := mock.Spec("sandbox-noTTL"); !ok {
		t.Errorf("no-TTL pod was unfairly culled")
	}
	if _, ok := mock.Spec("sandbox-fresh"); !ok {
		t.Errorf("fresh pod was unfairly culled")
	}
}

func TestReaperRespectsClock(t *testing.T) {
	mock := NewMockBackend()
	// Sandbox expires at T+10s.
	t0 := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	_ = seed(t, mock, 1, t0, 10)

	r := NewReaper(mock, "default", time.Minute)
	// First sweep at T+5s — should NOT reap.
	r.now = func() time.Time { return t0.Add(5 * time.Second) }
	r.sweep(context.Background())
	if got := mock.PodCount(); got != 1 {
		t.Fatalf("premature reap: want 1 pod, got %d", got)
	}

	// Second sweep at T+11s — should reap.
	r.now = func() time.Time { return t0.Add(11 * time.Second) }
	r.sweep(context.Background())
	if got := mock.PodCount(); got != 0 {
		t.Fatalf("missed reap: want 0 pods, got %d", got)
	}
}

func TestReaperHandlesManyConcurrently(t *testing.T) {
	mock := NewMockBackend()
	start := time.Now().Add(-time.Hour)
	seed(t, mock, 50, start, 10) // all expired

	r := NewReaper(mock, "default", time.Minute)
	r.sweep(context.Background())

	if got := mock.PodCount(); got != 0 {
		t.Fatalf("want 0 pods after concurrent sweep, got %d", got)
	}
}

func TestReaperSkipsBadAnnotation(t *testing.T) {
	mock := NewMockBackend()
	spec := SandboxSpec{SandboxID: "junk", NodeHost: "node"}
	_ = mock.CreatePod(context.Background(), "default", spec)
	_ = mock.CreateService(context.Background(), "default", spec)
	mock.SetAnnotationDirect("sandbox-junk", AnnotationExpiresAt, "not-a-time")

	r := NewReaper(mock, "default", time.Minute)
	r.sweep(context.Background()) // must not panic

	if got := mock.PodCount(); got != 1 {
		t.Fatalf("bad-annotation sandbox should be left alone, got %d pods", got)
	}
}

func TestReaperRunHonorsContext(t *testing.T) {
	mock := NewMockBackend()
	r := NewReaper(mock, "default", 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	time.Sleep(30 * time.Millisecond) // let it tick a couple of times
	cancel()

	select {
	case err := <-done:
		if err == nil || err != context.Canceled {
			t.Fatalf("Run returned %v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestReaperDisabledWhenIntervalZero(t *testing.T) {
	mock := NewMockBackend()
	r := NewReaper(mock, "default", 0)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("disabled reaper should return nil, got %v", err)
	}
}
