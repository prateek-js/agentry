package bridge

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// DeployRegistry — pure data-structure tests. No HTTP, no tunnel.
// The registry is the hot path on every browser request to a deployed
// URL; if it deadlocks, drops routes, or returns stale data, the
// entire deploy surface goes dark. Lock the contract here.

func TestDeployRegistry_LookupCaseInsensitive(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "Foo-Bar.AGENTRY.live", ClusterID: "c1"})

	for _, h := range []string{
		"foo-bar.agentry.live",
		"Foo-Bar.AGENTRY.live",
		"FOO-BAR.AGENTRY.LIVE",
	} {
		got, ok := r.Lookup(h)
		if !ok {
			t.Errorf("Lookup(%q) = missing; want hit", h)
			continue
		}
		if got.ClusterID != "c1" {
			t.Errorf("Lookup(%q).ClusterID = %q; want c1", h, got.ClusterID)
		}
	}
}

func TestDeployRegistry_LookupReturnsValue(t *testing.T) {
	r := NewDeployRegistry()
	want := DeployRoute{
		Hostname:     "shopwave-ecom-8c62.agentry.live",
		Kind:         "deployment",
		ClusterID:    "homelab",
		OrgID:        "org_x",
		AuthMode:     "org",
		DeploymentID: "dep_123",
	}
	r.Set(want)
	got, ok := r.Lookup(want.Hostname)
	if !ok {
		t.Fatal("Lookup missed a hostname that was just set")
	}
	if got != want {
		t.Errorf("Lookup returned %+v; want %+v", got, want)
	}
}

func TestDeployRegistry_LookupEmpty(t *testing.T) {
	r := NewDeployRegistry()
	if _, ok := r.Lookup("nope.agentry.live"); ok {
		t.Error("empty registry returned a hit")
	}
}

func TestDeployRegistry_SetReplacesExisting(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "x.agentry.live", ClusterID: "c-old"})
	r.Set(DeployRoute{Hostname: "x.agentry.live", ClusterID: "c-new"})
	got, _ := r.Lookup("x.agentry.live")
	if got.ClusterID != "c-new" {
		t.Errorf("Set didn't replace: got %q, want c-new", got.ClusterID)
	}
	if all := r.All(); len(all) != 1 {
		t.Errorf("All() = %d entries; want 1", len(all))
	}
}

func TestDeployRegistry_DeleteRemoves(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "kept.agentry.live", ClusterID: "c"})
	r.Set(DeployRoute{Hostname: "gone.agentry.live", ClusterID: "c"})
	r.Delete("gone.agentry.live")
	if _, ok := r.Lookup("gone.agentry.live"); ok {
		t.Error("Delete didn't remove the hostname")
	}
	if _, ok := r.Lookup("kept.agentry.live"); !ok {
		t.Error("Delete removed the wrong row")
	}
}

func TestDeployRegistry_DeleteMissingIsNoop(t *testing.T) {
	r := NewDeployRegistry()
	// Must not panic, must not error (signature is void).
	r.Delete("does-not-exist.agentry.live")
	if all := r.All(); len(all) != 0 {
		t.Errorf("All() = %d; want 0", len(all))
	}
}

func TestDeployRegistry_ReplaceAllSwapsTable(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "a.agentry.live", ClusterID: "c1"})
	r.Set(DeployRoute{Hostname: "b.agentry.live", ClusterID: "c1"})

	// Swap in a brand-new table containing only c.
	r.ReplaceAll([]DeployRoute{
		{Hostname: "c.agentry.live", ClusterID: "c2"},
	})

	if _, ok := r.Lookup("a.agentry.live"); ok {
		t.Error("ReplaceAll left stale entry a.agentry.live")
	}
	if _, ok := r.Lookup("b.agentry.live"); ok {
		t.Error("ReplaceAll left stale entry b.agentry.live")
	}
	if got, ok := r.Lookup("c.agentry.live"); !ok || got.ClusterID != "c2" {
		t.Errorf("ReplaceAll didn't install c.agentry.live: got=%+v ok=%v", got, ok)
	}
}

func TestDeployRegistry_ReplaceAllEmptyClears(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "a.agentry.live", ClusterID: "c1"})
	r.ReplaceAll(nil)
	if all := r.All(); len(all) != 0 {
		t.Errorf("ReplaceAll(nil) left %d entries; want 0", len(all))
	}
	if _, ok := r.Lookup("a.agentry.live"); ok {
		t.Error("ReplaceAll(nil) didn't clear the table")
	}
}

func TestDeployRegistry_ReplaceAllLowercasesKeys(t *testing.T) {
	r := NewDeployRegistry()
	r.ReplaceAll([]DeployRoute{
		{Hostname: "Mixed-Case.Agentry.Live", ClusterID: "c1"},
	})
	// Lookup with all-lower must hit — without this, control plane
	// pushing a route with capital letters would 404 every browser
	// request because Lookup lowercases its input.
	if _, ok := r.Lookup("mixed-case.agentry.live"); !ok {
		t.Error("ReplaceAll didn't lowercase the key")
	}
}

func TestDeployRegistry_All(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "a.agentry.live", ClusterID: "c1"})
	r.Set(DeployRoute{Hostname: "b.agentry.live", ClusterID: "c2"})
	r.Set(DeployRoute{Hostname: "c.agentry.live", ClusterID: "c3"})

	got := r.All()
	if len(got) != 3 {
		t.Fatalf("All() = %d; want 3", len(got))
	}
	seen := make(map[string]bool)
	for _, route := range got {
		seen[route.Hostname] = true
	}
	for _, want := range []string{"a.agentry.live", "b.agentry.live", "c.agentry.live"} {
		if !seen[want] {
			t.Errorf("All() missing %s", want)
		}
	}
}

// TestDeployRegistry_AllIsSnapshot proves All returns a fresh slice the
// caller can mutate without affecting the registry. If All shared the
// backing storage, an admin endpoint serializing the result could in
// principle race a concurrent Set and produce torn JSON.
func TestDeployRegistry_AllIsSnapshot(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "x.agentry.live", ClusterID: "c1"})
	snap := r.All()
	if len(snap) != 1 {
		t.Fatalf("All() = %d; want 1", len(snap))
	}
	// Mutate the caller's slice and confirm the registry is untouched.
	snap[0].ClusterID = "tampered"
	got, _ := r.Lookup("x.agentry.live")
	if got.ClusterID != "c1" {
		t.Errorf("registry mutated through All() snapshot: got %q", got.ClusterID)
	}
}

// ReplaceAll is what the control plane calls every 10s. While that
// runs, browser traffic is doing Lookup. The contract is: a Lookup
// during ReplaceAll either sees the old table or the new — never an
// empty or torn one. Hammer it under -race.
func TestDeployRegistry_ConcurrentLookupReplaceAll(t *testing.T) {
	r := NewDeployRegistry()
	// Seed with one stable route so Lookup must always succeed.
	const host = "stable.agentry.live"

	const replaces = 200
	const lookers = 8
	const lookupsPerWorker = 5000

	// Pre-build replacement payloads with the stable host always
	// present plus some churn around it.
	payloads := make([][]DeployRoute, replaces)
	for i := range payloads {
		payloads[i] = []DeployRoute{
			{Hostname: host, ClusterID: fmt.Sprintf("c%d", i)},
			{Hostname: fmt.Sprintf("churn-%d.agentry.live", i), ClusterID: "c-churn"},
		}
	}
	// Prime so the first lookups don't see empty.
	r.ReplaceAll(payloads[0])

	var (
		wg        sync.WaitGroup
		misses    atomic.Int64
		lookCount atomic.Int64
		stop      atomic.Bool
	)

	for i := 0; i < lookers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < lookupsPerWorker; j++ {
				if _, ok := r.Lookup(host); !ok {
					misses.Add(1)
				}
				lookCount.Add(1)
				if stop.Load() {
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < replaces; i++ {
			r.ReplaceAll(payloads[i])
		}
		// Let lookups finish their loops.
		stop.Store(true)
	}()
	wg.Wait()

	if misses.Load() != 0 {
		t.Errorf("Lookup(%q) missed %d / %d times during ReplaceAll — atomic-swap broken",
			host, misses.Load(), lookCount.Load())
	}
}

// TestDeployRegistry_ConcurrentSetDelete stresses the per-row locks
// (the rarer-but-still-supported delta path).
func TestDeployRegistry_ConcurrentSetDelete(t *testing.T) {
	r := NewDeployRegistry()
	const workers = 8
	const iter = 1000

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iter; i++ {
				host := fmt.Sprintf("h-%d-%d.agentry.live", w, i)
				r.Set(DeployRoute{Hostname: host, ClusterID: "c"})
				if _, ok := r.Lookup(host); !ok {
					t.Errorf("Set then Lookup(%q) missed", host)
				}
				r.Delete(host)
				if _, ok := r.Lookup(host); ok {
					t.Errorf("Delete then Lookup(%q) hit", host)
				}
			}
		}()
	}
	wg.Wait()
}

// TestDeployRegistry_LookupHandlesPortInHost asserts the contract for
// the caller (HandleDeployment): Lookup itself does NOT strip ports.
// The caller is responsible for that. Documenting the contract here
// keeps a future "let's be helpful" change from breaking the lookup
// shape that's been in production.
func TestDeployRegistry_LookupDoesNotStripPort(t *testing.T) {
	r := NewDeployRegistry()
	r.Set(DeployRoute{Hostname: "x.agentry.live", ClusterID: "c1"})
	if _, ok := r.Lookup("x.agentry.live:8443"); ok {
		t.Error("Lookup should NOT strip port — caller's job (HandleDeployment does it)")
	}
}

// quick sanity that even with thousands of routes ReplaceAll completes
// in a sensible time. Not a perf gate — just a smoke test that the
// O(N) swap stays O(N).
func TestDeployRegistry_ReplaceAllScales(t *testing.T) {
	r := NewDeployRegistry()
	const N = 5000
	routes := make([]DeployRoute, N)
	for i := range routes {
		routes[i] = DeployRoute{Hostname: fmt.Sprintf("h%d.agentry.live", i), ClusterID: "c"}
	}
	start := time.Now()
	r.ReplaceAll(routes)
	if d := time.Since(start); d > time.Second {
		t.Errorf("ReplaceAll(%d) took %s; suspicious", N, d)
	}
	if got, ok := r.Lookup("h2500.agentry.live"); !ok || got.ClusterID != "c" {
		t.Errorf("post-ReplaceAll lookup failed: ok=%v got=%+v", ok, got)
	}
}

// Guard against accidentally regressing the "lowercase normalization
// is applied on every write path" rule. ReplaceAll bypasses Set; Set
// uses ToLower; if someone refactors ReplaceAll to call a different
// path, this test will catch the divergence.
func TestDeployRegistry_NormalizationParity(t *testing.T) {
	mixed := "Mixed.AGENTRY.live"
	lower := strings.ToLower(mixed)

	rSet := NewDeployRegistry()
	rSet.Set(DeployRoute{Hostname: mixed, ClusterID: "c"})

	rRA := NewDeployRegistry()
	rRA.ReplaceAll([]DeployRoute{{Hostname: mixed, ClusterID: "c"}})

	if _, ok := rSet.Lookup(lower); !ok {
		t.Error("Set didn't normalize to lowercase key")
	}
	if _, ok := rRA.Lookup(lower); !ok {
		t.Error("ReplaceAll didn't normalize to lowercase key")
	}
}
