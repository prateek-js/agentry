package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// Removed-command behaviour. `agentry share` and `agentry deploy` used
// to be CLI surface; they've moved to the dashboard. The CLI now
// prints a pointer + exits 2 (usage error, not a runtime success) so
// scripts that still call these notice instead of silently doing
// nothing.

func TestDispatch_ShareRedirectsToDashboard(t *testing.T) {
	out, code := captureStderr(t, func() int { return dispatch([]string{"share"}) })
	if code != 2 {
		t.Errorf("exit = %d; want 2", code)
	}
	if !strings.Contains(out, "dashboard") || !strings.Contains(out, "app.agentry.run") {
		t.Errorf("stderr should point at the dashboard, got %q", out)
	}
}

func TestDispatch_DeployRedirectsToDashboard(t *testing.T) {
	for _, name := range []string{"deploy", "deployment"} {
		out, code := captureStderr(t, func() int { return dispatch([]string{name}) })
		if code != 2 {
			t.Errorf("%s exit = %d; want 2", name, code)
		}
		if !strings.Contains(out, "dashboard") {
			t.Errorf("%s stderr should mention dashboard, got %q", name, out)
		}
	}
}

// `agentry cluster` and `agentry server` MUST route to the same
// handler. The CLI surface uses "server" now; existing scripts and
// muscle memory still type "cluster". Both shapes need to work
// identically — same exit code, same stderr if the user passes a bad
// subcommand. This test pins both paths.
func TestDispatch_ClusterAliasesToServer(t *testing.T) {
	t.Setenv("AGENTRY_CONFIG", t.TempDir()+"/empty.json")
	outServer, codeServer := captureStderr(t, func() int {
		return dispatch([]string{"server", "bogus-subcommand"})
	})
	outCluster, codeCluster := captureStderr(t, func() int {
		return dispatch([]string{"cluster", "bogus-subcommand"})
	})
	if codeServer != codeCluster {
		t.Errorf("server exit=%d, cluster exit=%d — alias diverged", codeServer, codeCluster)
	}
	// Both should hit the "unknown subcommand" path; pin the wording
	// so the alias can't silently drop into a different code path.
	for label, out := range map[string]string{"server": outServer, "cluster": outCluster} {
		if !strings.Contains(out, "unknown subcommand") {
			t.Errorf("%s did not report 'unknown subcommand': %q", label, out)
		}
	}
}

func TestDispatch_UnknownPrintsUsageAndExit2(t *testing.T) {
	out, code := captureStderr(t, func() int { return dispatch([]string{"bogus-thing"}) })
	if code != 2 {
		t.Errorf("exit = %d; want 2", code)
	}
	if !strings.Contains(out, "unknown subcommand") {
		t.Errorf("stderr should name the failure, got %q", out)
	}
	// Usage block is shown so the user has the menu to pick from.
	if !strings.Contains(out, "GETTING STARTED") {
		t.Errorf("usage block missing from stderr")
	}
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn,
// reads everything written, restores Stderr, and returns the bytes.
func captureStderr(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = wPipe
	done := make(chan int, 1)
	go func() { done <- fn() }()

	// Close write side as soon as fn returns so the reader sees EOF.
	code := <-done
	_ = wPipe.Close()
	os.Stderr = orig

	raw, _ := io.ReadAll(rPipe)
	_ = rPipe.Close()
	return string(raw), code
}
