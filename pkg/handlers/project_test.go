package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit stop must suppress the auto-restart loop. This is the load-
// bearing invariant for the deploy-time preview pause: we stop the dev
// server before preflight, expect it to STAY stopped, and only the
// caller's explicit Start brings it back. Before the fix, watchProcess
// auto-restarted any project where config.AutoRestart=true within
// 1-16 s, so an external pause had no chance against the build.
func TestStopSuppressesAutoRestart(t *testing.T) {
	dir := t.TempDir()
	projectsRoot := filepath.Join(dir, "projects")
	projDir := filepath.Join(projectsRoot, "app")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	// A sleep loop that auto-restarts on crash — same shape as a real
	// dev server config. The command intentionally has no early exit so
	// "did it come back" is unambiguous.
	cfg := map[string]any{
		"name":          "app",
		"type":          "service",
		"start_command": []string{"sh", "-c", "sleep 300"},
		"auto_restart":  true,
	}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(projDir, ".sandbox-project.json"), cfgBytes, 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	pm := NewProjectManager(dir)
	proj, err := pm.StartProject("app")
	if err != nil {
		t.Fatalf("StartProject: %v", err)
	}
	proj.mu.Lock()
	firstPID := proj.pid
	proj.mu.Unlock()
	if firstPID == 0 {
		t.Fatal("expected pid after StartProject")
	}

	// Explicit stop. Then wait longer than the max auto-restart backoff
	// (16 s in code) would let the loop fire if it were going to. Use
	// a shorter wait for test speed — within the 1 s backoff floor, an
	// unsuppressed auto-restart would already have run StartProject and
	// the project would either be "running" again with a new PID, or
	// be mid-spawn (status=starting). The current behaviour: stays
	// stopped indefinitely.
	if err := pm.StopProject("app"); err != nil {
		t.Fatalf("StopProject: %v", err)
	}

	// 3 s is enough: the auto-restart loop's first wake is after
	// `1<<(restartCount-1) = 1` second. If suppression isn't holding,
	// we'll catch it well within this window.
	time.Sleep(3 * time.Second)

	proj.mu.Lock()
	status := proj.status
	proj.mu.Unlock()
	if status == "running" || status == "starting" {
		t.Fatalf("project came back as %q after explicit stop — auto-restart leaked", status)
	}

	// And StartProject should still bring it back cleanly (manuallyStopped
	// must not carry across lifecycles — a fresh struct is created).
	proj2, err := pm.StartProject("app")
	if err != nil {
		t.Fatalf("StartProject after stop: %v", err)
	}
	proj2.mu.Lock()
	secondPID := proj2.pid
	secondStatus := proj2.status
	proj2.mu.Unlock()
	if secondPID == 0 || secondPID == firstPID {
		t.Errorf("expected a fresh pid after restart; first=%d second=%d", firstPID, secondPID)
	}
	if secondStatus != "running" {
		t.Errorf("status after restart = %q; want running", secondStatus)
	}

	// Cleanup so the test process tree doesn't hold orphan sleeps.
	_ = pm.StopProject("app")
}

// Crashes (process exit code != 0 without an explicit stop) still get
// auto-restarted — the manuallyStopped gate must not swallow real
// crash recovery. We verify by spawning a command that exits 1 quickly
// with auto_restart=true; the restart count should bump.
func TestCrashStillAutoRestarts(t *testing.T) {
	dir := t.TempDir()
	projectsRoot := filepath.Join(dir, "projects")
	projDir := filepath.Join(projectsRoot, "crasher")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	cfg := map[string]any{
		"name": "crasher",
		"type": "service",
		// Sleep 1, exit 1 — gives Start a moment to settle before the
		// crash, so the watchProcess goroutine has a fresh proj to
		// inspect.
		"start_command": []string{"sh", "-c", "sleep 1; exit 1"},
		"auto_restart":  true,
	}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(projDir, ".sandbox-project.json"), cfgBytes, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	pm := NewProjectManager(dir)
	if _, err := pm.StartProject("crasher"); err != nil {
		t.Fatalf("StartProject: %v", err)
	}

	// Wait for first crash + restart backoff (1 s) + a margin. After
	// this, restartCount should be ≥ 1 if the auto-restart loop fired.
	time.Sleep(4 * time.Second)

	pm.mu.Lock()
	proj := pm.projects["crasher"]
	pm.mu.Unlock()
	proj.mu.Lock()
	restarts := proj.restartCount
	proj.mu.Unlock()

	if restarts < 1 {
		t.Errorf("restartCount = %d; want ≥ 1 (crash should have auto-restarted)", restarts)
	}

	_ = pm.StopProject("crasher")
}
