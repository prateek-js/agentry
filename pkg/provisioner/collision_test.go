package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentry/agentry/pkg/auth"
)

// Sandbox-name collision behavior. The "ecommerce-store" incident:
// the LLM picked the same name in two separate Roo chats, both POSTs
// landed at the same provisioner, the second silently reused the
// first sandbox's container and the second chat's files clobbered
// the first chat's workspace.
//
// The fix in handleCreate:
//   ReuseExisting=true  → return the existing sandbox (legacy resume)
//   ReuseExisting=false → allocate <id>-<4hex>; response carries the
//                         allocated id so the caller uses that
//                         id for follow-ups.
//
// These tests pin every important shape.

// postCreate is a small helper that wraps the auth-header dance of
// POST /api/sandboxes so each test body stays terse. Returns the
// HTTP status and the parsed SandboxInfo (only set on 200).
func postCreate(t *testing.T, ts string, key string, body any) (int, SandboxInfo) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts+"/api/sandboxes", bytes.NewReader(buf))
	req.Header.Set(auth.HeaderName, key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/sandboxes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode, SandboxInfo{}
	}
	var info SandboxInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode SandboxInfo: %v", err)
	}
	return resp.StatusCode, info
}

// 1. Happy path: no collision → caller's requested name is kept.
func TestCreateSandbox_NoCollision_KeepsRequestedName(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")
	status, info := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "ecommerce-store", "thread_id": "t1"})
	if status != 200 {
		t.Fatalf("status = %d; want 200", status)
	}
	if info.SandboxID != "ecommerce-store" {
		t.Errorf("returned sandbox_id = %q; want ecommerce-store", info.SandboxID)
	}
	if mock.PodCount() != 1 {
		t.Errorf("pods = %d; want 1", mock.PodCount())
	}
	if _, ok := mock.Spec("sandbox-ecommerce-store"); !ok {
		t.Errorf("expected pod sandbox-ecommerce-store")
	}
}

// 2. Collision + reuse_existing=true → reuses the existing sandbox.
//    This is the "agentry attach <name>" semantics — explicitly opted
//    in by the caller, so silently returning the existing pod is the
//    intended outcome.
func TestCreateSandbox_Collision_ReuseExisting_ReturnsSame(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")
	// First create.
	_, first := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "ecommerce-store", "thread_id": "t1"})
	if first.SandboxID != "ecommerce-store" {
		t.Fatalf("setup: first create returned %q; want ecommerce-store", first.SandboxID)
	}
	// Second create with reuse_existing=true.
	status, second := postCreate(t, ts.URL, "secret", map[string]any{
		"sandbox_id":     "ecommerce-store",
		"thread_id":      "t2",
		"reuse_existing": true,
	})
	if status != 200 {
		t.Fatalf("reuse status = %d; want 200", status)
	}
	if second.SandboxID != "ecommerce-store" {
		t.Errorf("reuse returned %q; want ecommerce-store (same)", second.SandboxID)
	}
	if mock.PodCount() != 1 {
		t.Errorf("reuse should not allocate a new pod; pods = %d", mock.PodCount())
	}
}

// 3. Collision without reuse_existing → fresh suffixed sandbox.
//    THIS IS THE FIX for the ecommerce-store bug. Both chats stay
//    isolated even though both posted the same name.
func TestCreateSandbox_Collision_DefaultAllocatesSuffixedFresh(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")

	// First chat: creates "ecommerce-store".
	_, first := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "ecommerce-store", "thread_id": "t1"})
	if first.SandboxID != "ecommerce-store" {
		t.Fatalf("setup: first create returned %q", first.SandboxID)
	}

	// Second chat: posts the same name, default reuse_existing=false.
	status, second := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "ecommerce-store", "thread_id": "t2"})
	if status != 200 {
		t.Fatalf("second status = %d; want 200", status)
	}
	if second.SandboxID == "ecommerce-store" {
		t.Errorf("second create returned the original name (silently reused — the bug)")
	}
	if !strings.HasPrefix(second.SandboxID, "ecommerce-store-") {
		t.Errorf("suffixed id %q must keep the requested base as prefix", second.SandboxID)
	}
	// Suffix is "<base>-<4hex>".
	suffix := strings.TrimPrefix(second.SandboxID, "ecommerce-store-")
	if len(suffix) != 4 || !isHex(suffix) {
		t.Errorf("suffix %q must be 4 hex chars", suffix)
	}

	// Two distinct pods now exist — workspace isolation is restored.
	if mock.PodCount() != 2 {
		t.Errorf("pods = %d; want 2 (one per chat)", mock.PodCount())
	}
	if _, ok := mock.Spec("sandbox-ecommerce-store"); !ok {
		t.Error("first sandbox should still exist")
	}
	if _, ok := mock.Spec("sandbox-" + second.SandboxID); !ok {
		t.Errorf("second sandbox sandbox-%s should exist", second.SandboxID)
	}
}

// 4. The spec recorded for the second sandbox uses the suffixed id —
//    NOT the requested id. If the spec.SandboxID were the requested
//    name, AGENTRY_APP_NAME / pod labels would collide downstream and
//    the bug would manifest at a different layer.
func TestCreateSandbox_Collision_SpecCarriesAllocatedID(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")
	_, _ = postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "myapp", "thread_id": "t1"})
	_, second := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "myapp", "thread_id": "t2"})

	spec, ok := mock.Spec("sandbox-" + second.SandboxID)
	if !ok {
		t.Fatalf("no pod for sandbox-%s", second.SandboxID)
	}
	if spec.SandboxID != second.SandboxID {
		t.Errorf("spec.SandboxID = %q; want %q (must reflect allocated id, not request)",
			spec.SandboxID, second.SandboxID)
	}
}

// 5. The suffix loop must skip a randomly-collided candidate and try
//    again. Stub crypto/rand so the first two attempts pick a name
//    that's already taken; assert the handler tried again and landed
//    on the third.
func TestAllocFreshSandboxID_RetriesOnSuffixCollision(t *testing.T) {
	mock := NewMockBackend()
	cfg := Config{Namespace: "test-ns", NodeHost: "h", Labels: map[string]string{"a": "b"}}
	p := NewWithKey(cfg, mock, "k")

	// Pre-seed two name shapes the rand stub will produce first.
	mock.preSeed("base-aaaa", "h", 30000)
	mock.preSeed("base-bbbb", "h", 30001)

	// Stub: return bytes that map to 'a','a','a','a' then 'b','b','b','b'
	// then 'c','c','c','c'. Hex maps `int(b)%16` → index. We want
	// "aaaa", "bbbb", "cccc". 0xaa → index 10 → 'a'. 0xbb → 11 → 'b'.
	// 0xcc → 12 → 'c'.
	defer restoreRand(stubRand(t, []byte{
		0xaa, 0xaa, 0xaa, 0xaa,
		0xbb, 0xbb, 0xbb, 0xbb,
		0xcc, 0xcc, 0xcc, 0xcc,
	}))

	got, err := p.allocFreshSandboxID(context.Background(), "base")
	if err != nil {
		t.Fatal(err)
	}
	if got != "base-cccc" {
		t.Errorf("got %q; want base-cccc (first two were pre-seeded as taken)", got)
	}
}

// 6. After running out of suffix attempts, allocFreshSandboxID errors
//    out rather than looping forever or panicking. Pre-seed every
//    candidate the stubbed rand can produce; assert we fail clean.
func TestAllocFreshSandboxID_FailsAfterMaxAttempts(t *testing.T) {
	mock := NewMockBackend()
	cfg := Config{Namespace: "test-ns", NodeHost: "h"}
	p := NewWithKey(cfg, mock, "k")

	// Stub rand to always return 'a' bytes. Pre-seed base-aaaa so
	// every attempt collides; the loop should give up.
	mock.preSeed("base-aaaa", "h", 30000)
	defer restoreRand(stubRand(t, bytes.Repeat([]byte{0xaa}, 100)))

	_, err := p.allocFreshSandboxID(context.Background(), "base")
	if err == nil {
		t.Error("expected exhaustion error when every suffix collides")
	}
}

// 7. End-to-end: the handler returns 500 if collision can't be
//    resolved (defense — should never happen in practice).
func TestCreateSandbox_CollisionUnresolvable_Returns500(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")

	// First, create the original.
	_, _ = postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "x", "thread_id": "t1"})

	// Stub rand to a single value, pre-seed that suffixed name so the
	// retry loop exhausts.
	defer restoreRand(stubRand(t, bytes.Repeat([]byte{0xaa}, 100)))
	mock.preSeed("x-aaaa", "h", 30099)

	buf, _ := json.Marshal(map[string]any{"sandbox_id": "x", "thread_id": "t2"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes", bytes.NewReader(buf))
	req.Header.Set(auth.HeaderName, "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("unresolvable collision status = %d; want 500", resp.StatusCode)
	}
}

// 8. sanitizeSandboxID — pin the contract that allocFreshSandboxID
//    depends on. Bad shapes get scrubbed so the pod-name layer never
//    sees something docker refuses.
func TestSanitizeSandboxID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ecommerce-store", "ecommerce-store"},
		{"EcommerceStore", "ecommercestore"},
		{"my.app", "my-app"},
		{"my_app", "my-app"},
		{"--leading-dash", "leading-dash"},
		{"trailing-dash-", "trailing-dash"},
		{"con--secutive", "con-secutive"},
		{"weird!@#chars$$$here", "weirdcharshere"},
		{"", ""},
		{strings.Repeat("a", 100), strings.Repeat("a", 40)}, // truncate at 40
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := sanitizeSandboxID(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeSandboxID(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// 9. randomHexSuffix returns the right shape from the real source.
func TestRandomHexSuffix(t *testing.T) {
	s := randomHexSuffix(4)
	if len(s) != 4 {
		t.Errorf("len = %d; want 4", len(s))
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Errorf("non-hex char %c in %q", r, s)
		}
	}
}

// 10. Concurrent racing creates — N goroutines POST the same name.
//     Without the auto-suffix one of them would silently reuse and
//     clobber. With the fix, every goroutine ends up with a distinct
//     sandbox_id (except at most one that gets the original name).
//     Hammer it under -race.
func TestCreateSandbox_ConcurrentCollisions_AllDistinct(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")

	const N = 16
	var (
		wg       sync.WaitGroup
		seenMu   sync.Mutex
		seen     = map[string]int{} // sandbox_id → hit count
		fail     atomic.Int64
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, info := postCreate(t, ts.URL, "secret",
				map[string]any{"sandbox_id": "raceapp", "thread_id": fmt.Sprintf("t-%d", 0)})
			if status != 200 {
				fail.Add(1)
				return
			}
			seenMu.Lock()
			seen[info.SandboxID]++
			seenMu.Unlock()
		}()
	}
	wg.Wait()
	if fail.Load() != 0 {
		t.Errorf("%d / %d creates failed", fail.Load(), N)
	}
	// Sanity: distinct sandbox_ids — duplicates would mean two callers
	// got the same workspace, which is the bug. With 16 callers and
	// 65536 possible suffixes, collisions are vanishingly rare; the
	// 8-attempt retry covers the rest.
	for id, hits := range seen {
		if hits > 1 {
			t.Errorf("sandbox_id %q returned to %d callers — should be unique", id, hits)
		}
	}
	// And the pod count matches the number of distinct ids.
	if mock.PodCount() != len(seen) {
		t.Errorf("pod count %d != distinct ids %d", mock.PodCount(), len(seen))
	}
}

// 11. Reuse path returns the SAME public URL as the first create.
//     If the URL changed under reuse semantics, callers caching it
//     would silently start hitting the wrong sandbox.
func TestCreateSandbox_ReuseExisting_StableURL(t *testing.T) {
	ts, _ := newTestProvisioner(t, "secret")
	_, first := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "stable", "thread_id": "t1"})
	_, second := postCreate(t, ts.URL, "secret", map[string]any{
		"sandbox_id":     "stable",
		"thread_id":      "t2",
		"reuse_existing": true,
	})
	if first.SandboxURL != second.SandboxURL {
		t.Errorf("reuse URL = %q; want %q (same)", second.SandboxURL, first.SandboxURL)
	}
}

// 12. Auto-suffix path returns a DIFFERENT URL (different port via
//     the mock backend's nextPort counter). Confirms isolation
//     extends to the routing layer.
func TestCreateSandbox_AutoSuffix_DifferentURL(t *testing.T) {
	ts, _ := newTestProvisioner(t, "secret")
	_, first := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "diff", "thread_id": "t1"})
	_, second := postCreate(t, ts.URL, "secret",
		map[string]any{"sandbox_id": "diff", "thread_id": "t2"})
	if first.SandboxURL == second.SandboxURL {
		t.Errorf("auto-suffixed sandbox shares URL %q with original — routing not isolated",
			first.SandboxURL)
	}
}

// ── helpers ─────────────────────────────────────────────────────────

// stubRand replaces cryptoRandRead with a stub that returns `bytes`
// in order. Returns a restore func.
func stubRand(t *testing.T, source []byte) func() {
	t.Helper()
	orig := cryptoRandRead
	var pos int
	var mu sync.Mutex
	cryptoRandRead = func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		for i := range p {
			if pos >= len(source) {
				return i, nil
			}
			p[i] = source[pos]
			pos++
		}
		return len(p), nil
	}
	return func() { cryptoRandRead = orig }
}

func restoreRand(restore func()) { restore() }

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
