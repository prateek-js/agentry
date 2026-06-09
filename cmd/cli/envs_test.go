package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentry/agentry/pkg/mcp"
)

// Tests for cluster-default env vars (`agentry env set NAME`,
// no --sandbox). The shape mirrors binds_test.go's coverage:
// storage roundtrips, listing semantics, hook applies each saved
// env to a new sandbox, and the chainHooks composition runs both
// child hooks even if the first fails.

// pinAgentryConfig stages a fake config root under t.TempDir()
// and points AGENTRY_CONFIG at it for the duration of the test.
// envsDir reads ConfigPath() at call time, so this is enough to
// redirect every env-storage operation into the test tree.
func pinAgentryConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agentry.json")
	if err := os.WriteFile(cfg, []byte(`{"cluster":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTRY_CONFIG", cfg)
	return dir
}

func TestSaveLoadEnv_RoundTrip(t *testing.T) {
	pinAgentryConfig(t)
	if err := saveEnv("test", defaultProfile, &StoredEnv{Name: "JIRA_TOKEN", Value: "atlassian-xyz"}); err != nil {
		t.Fatal(err)
	}
	got, err := loadEnv("test", defaultProfile, "JIRA_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("load returned nil — file should exist")
	}
	if got.Name != "JIRA_TOKEN" || got.Value != "atlassian-xyz" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLoadEnv_Missing_ReturnsNil(t *testing.T) {
	pinAgentryConfig(t)
	got, err := loadEnv("test", defaultProfile, "NEVER_SAVED")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing file should return nil, got %+v", got)
	}
}

func TestSaveEnv_RejectsInvalidName(t *testing.T) {
	pinAgentryConfig(t)
	bads := []string{"", "lowercase", "0LEADING_DIGIT", "WITH.DOT", "WITH-DASH"}
	for _, name := range bads {
		err := saveEnv("test", defaultProfile, &StoredEnv{Name: name, Value: "x"})
		if err == nil {
			t.Errorf("saveEnv(%q) should have failed validation", name)
		}
	}
	for _, name := range []string{"JIRA_TOKEN", "ABC", "X_1", "_PREFIXED", "X123"} {
		if err := saveEnv("test", defaultProfile, &StoredEnv{Name: name, Value: "x"}); err != nil {
			t.Errorf("saveEnv(%q) unexpectedly failed: %v", name, err)
		}
	}
}

func TestSaveEnv_FilePerms(t *testing.T) {
	pinAgentryConfig(t)
	if err := saveEnv("test", defaultProfile, &StoredEnv{Name: "T", Value: "v"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(envFilePath("test", defaultProfile, "T"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file perms = %o; want 0600 (secrets should not be world-readable)", perm)
	}
}

func TestDeleteEnv_IdempotentOnMissing(t *testing.T) {
	pinAgentryConfig(t)
	if err := deleteEnv("test", defaultProfile, "NEVER_SAVED"); err != nil {
		t.Errorf("delete on missing file should be a no-op: %v", err)
	}
	if err := saveEnv("test", defaultProfile, &StoredEnv{Name: "X", Value: "v"}); err != nil {
		t.Fatal(err)
	}
	if err := deleteEnv("test", defaultProfile, "X"); err != nil {
		t.Fatalf("delete after save: %v", err)
	}
	if _, err := os.Stat(envFilePath("test", defaultProfile, "X")); !os.IsNotExist(err) {
		t.Errorf("file still exists after delete: %v", err)
	}
}

func TestListEnvs_SortsByName(t *testing.T) {
	pinAgentryConfig(t)
	for _, name := range []string{"ZULU", "ALPHA", "MIKE"} {
		if err := saveEnv("test", defaultProfile, &StoredEnv{Name: name, Value: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listEnvs("test", defaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	want := []string{"ALPHA", "MIKE", "ZULU"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("listEnvs order = %v; want %v", names, want)
	}
}

func TestListEnvs_EmptyClusterDir_ReturnsNoError(t *testing.T) {
	pinAgentryConfig(t)
	// Don't save anything — the envs/<cluster>/<profile>/ dir doesn't exist.
	got, err := listEnvs("test", defaultProfile)
	if err != nil {
		t.Fatalf("listing empty cluster should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %v", got)
	}
}

// constCtx is a getCtx helper that returns the same (cluster, profile)
// every call. Mirrors the production clusterAndProfile() shape so
// tests can assert without dragging config-file plumbing through each
// case.
func constCtx(cluster, profile string) func() (string, string) {
	return func() (string, string) { return cluster, profile }
}

// TestApplyClusterEnvDefaults_PostsEachSavedEnv exercises the hook
// end-to-end: stage a few envs on disk, call the hook against a fake
// provisioner, and confirm every saved env hit
// /api/sandboxes/{id}/secrets with the right body. The hook also
// auto-stamps AGENTRY_PROFILE on every sandbox, so we assert that
// third POST too.
func TestApplyClusterEnvDefaults_PostsEachSavedEnv(t *testing.T) {
	pinAgentryConfig(t)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(saveEnv("test", defaultProfile, &StoredEnv{Name: "JIRA_TOKEN", Value: "j"}))
	must(saveEnv("test", defaultProfile, &StoredEnv{Name: "STRIPE_KEY", Value: "s"}))

	var (
		mu      sync.Mutex
		got     []map[string]any
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		got = append(got, b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hc := &http.Client{Transport: &hostRewriter{base: srv.URL, wrapped: srv.Client().Transport}}
	hook := applyClusterEnvDefaults(constCtx("test", defaultProfile), hc)

	if err := hook(context.Background(), mcp.SandboxInfo{SandboxID: "sb_1"}); err != nil {
		t.Fatalf("hook returned err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d POSTs; want 3 (2 staged + AGENTRY_PROFILE auto-stamp)", len(got))
	}
	if !strings.HasPrefix(gotPath, "/api/sandboxes/sb_1/secrets") {
		t.Errorf("path = %q; want /api/sandboxes/sb_1/secrets", gotPath)
	}
	// All three names should appear; source should be the canonical
	// "cli-cluster-default" string so the runtime can attribute them.
	names := map[string]bool{}
	for _, b := range got {
		if n, ok := b["name"].(string); ok {
			names[n] = true
		}
		if src, ok := b["source"].(string); ok {
			if src != "cli-cluster-default" {
				t.Errorf("source = %q; want cli-cluster-default", src)
			}
		}
	}
	for _, want := range []string{"JIRA_TOKEN", "STRIPE_KEY", "AGENTRY_PROFILE"} {
		if !names[want] {
			t.Errorf("missing %q in staged names: %v", want, names)
		}
	}
}

func TestApplyClusterEnvDefaults_StampsProfileEvenWhenNothingStaged(t *testing.T) {
	pinAgentryConfig(t)
	// No saveEnv calls — the only thing the hook should post is the
	// AGENTRY_PROFILE auto-stamp. Stamping unconditionally is
	// load-bearing: app code that branches on profile shouldn't
	// require the operator to have set other envs.
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(body, &b)
		got = append(got, b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &hostRewriter{base: srv.URL, wrapped: srv.Client().Transport}}
	hook := applyClusterEnvDefaults(constCtx("test", "dev"), hc)
	if err := hook(context.Background(), mcp.SandboxInfo{SandboxID: "sb_x"}); err != nil {
		t.Errorf("hook should succeed with only auto-stamp: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 POST (the auto-stamp), got %d", len(got))
	}
	if name, _ := got[0]["name"].(string); name != "AGENTRY_PROFILE" {
		t.Errorf("name = %q; want AGENTRY_PROFILE", name)
	}
	if v, _ := got[0]["value"].(string); v != "dev" {
		t.Errorf("AGENTRY_PROFILE value = %q; want dev", v)
	}
}

func TestApplyClusterEnvDefaults_EmptyCluster_NoOp(t *testing.T) {
	pinAgentryConfig(t)
	// Save an env under "other-cluster" so the disk isn't empty —
	// but the hook is asked about "" (cluster unset). Should skip
	// entirely, including the AGENTRY_PROFILE stamp (no cluster
	// context = no sandbox to attribute the profile to).
	if err := saveEnv("other-cluster", defaultProfile, &StoredEnv{Name: "X", Value: "v"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("hook fired %s for empty cluster", r.URL.Path)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &hostRewriter{base: srv.URL, wrapped: srv.Client().Transport}}
	hook := applyClusterEnvDefaults(constCtx("", defaultProfile), hc)
	if err := hook(context.Background(), mcp.SandboxInfo{SandboxID: "sb_x"}); err != nil {
		t.Errorf("hook with empty cluster should not error: %v", err)
	}
}

// TestChainHooks_RunsBothEvenIfFirstErrors guarantees that a
// service-bind failure doesn't shadow env replay (or vice versa).
func TestChainHooks_RunsBothEvenIfFirstErrors(t *testing.T) {
	var ran [2]bool
	a := func(ctx context.Context, info mcp.SandboxInfo) error {
		ran[0] = true
		return io.EOF // fake error
	}
	b := func(ctx context.Context, info mcp.SandboxInfo) error {
		ran[1] = true
		return nil
	}
	hook := chainHooks(a, b)
	err := hook(context.Background(), mcp.SandboxInfo{SandboxID: "sb_x"})
	if err == nil {
		t.Errorf("chain should surface the first error")
	}
	if !ran[0] || !ran[1] {
		t.Errorf("both hooks should run; ran=%v", ran)
	}
}

func TestChainHooks_SkipsNil(t *testing.T) {
	ran := false
	a := func(ctx context.Context, info mcp.SandboxInfo) error { ran = true; return nil }
	hook := chainHooks(nil, a, nil)
	if err := hook(context.Background(), mcp.SandboxInfo{SandboxID: "sb_x"}); err != nil {
		t.Errorf("nil hooks should be skipped without error: %v", err)
	}
	if !ran {
		t.Errorf("non-nil hook should still fire when wrapped with nils")
	}
}

// hostRewriter swaps the "http://bridge.invalid" placeholder in
// outbound requests for the test server's actual URL so the hook
// (which builds requests against the canonical placeholder) can
// reach the test handler.
type hostRewriter struct {
	base    string
	wrapped http.RoundTripper
}

func (h *hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), "http://bridge.invalid") {
		// Rebuild URL with the test server's scheme + host.
		newURL := h.base + req.URL.Path
		if req.URL.RawQuery != "" {
			newURL += "?" + req.URL.RawQuery
		}
		newReq := req.Clone(req.Context())
		var err error
		newReq.URL, err = newReq.URL.Parse(newURL)
		if err != nil {
			return nil, err
		}
		newReq.Host = newReq.URL.Host
		req = newReq
	}
	if h.wrapped != nil {
		return h.wrapped.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}
