package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The deployment_status hook reaches the control plane over the PAT and
// filters the org-wide list to one sandbox. These pin: the right
// endpoint + auth header, sandbox-id filtering, the summary line, and
// the empty-case note.

func TestDeploymentStatusHook_FiltersBySandbox(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deployments": []map[string]any{
				{"id": "dep_1", "sandbox_id": "sb_a", "status": "running", "url": "https://a.agentry.live"},
				{"id": "dep_2", "sandbox_id": "sb_b", "status": "failed"},
				{"id": "dep_3", "sandbox_id": "sb_a", "status": "building"},
			},
		})
	}))
	defer srv.Close()

	cfg := &Config{AppURL: srv.URL, APIToken: "pat_xyz"}
	hook := deploymentStatusHook(func() *Config { return cfg })
	res, err := hook(context.Background(), "sb_a")
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)

	if gotPath != "/api/v1/deployments" {
		t.Errorf("hit %q; want /api/v1/deployments", gotPath)
	}
	if gotAuth != "Bearer pat_xyz" {
		t.Errorf("auth = %q; want Bearer pat_xyz", gotAuth)
	}
	if m["count"].(int) != 2 {
		t.Fatalf("count = %v; want 2 (only sb_a rows)", m["count"])
	}
	summary, _ := m["summary"].(string)
	if !strings.Contains(summary, "1 running") || !strings.Contains(summary, "1 in-progress") {
		t.Errorf("summary should count states; got %q", summary)
	}
}

func TestDeploymentStatusHook_EmptyHasNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"deployments": []any{}})
	}))
	defer srv.Close()

	cfg := &Config{AppURL: srv.URL, APIToken: "pat_xyz"}
	hook := deploymentStatusHook(func() *Config { return cfg })
	res, err := hook(context.Background(), "sb_none")
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["count"].(int) != 0 {
		t.Errorf("count = %v; want 0", m["count"])
	}
	if note, _ := m["note"].(string); !strings.Contains(note, "dashboard") {
		t.Errorf("empty result should point at the dashboard; got %q", note)
	}
}

func TestDeploymentStatusHook_NotLoggedIn(t *testing.T) {
	cfg := &Config{} // no AppURL / APIToken
	hook := deploymentStatusHook(func() *Config { return cfg })
	_, err := hook(context.Background(), "sb_a")
	if err == nil {
		t.Fatal("a loginless config should error so the model can relay it")
	}
}

func TestDeploymentStatusAvailable(t *testing.T) {
	if deploymentStatusAvailable(nil) {
		t.Error("nil cfg should be unavailable")
	}
	if deploymentStatusAvailable(&Config{AppURL: "https://x"}) {
		t.Error("missing token should be unavailable")
	}
	if !deploymentStatusAvailable(&Config{AppURL: "https://x", APIToken: "t"}) {
		t.Error("AppURL + token should be available")
	}
	if deploymentStatusHookIfAvailable(&Config{}) != nil {
		t.Error("loginless cfg should yield a nil hook (tool degrades to dashboard pointer)")
	}
}
