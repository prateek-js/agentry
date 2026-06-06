package main

import (
	"strings"
	"testing"
)

// Top-level usage block guards. New commands SHOULD show up here, and
// removed commands MUST NOT. Drift is the leading cause of stale CLI
// help — pin the load-bearing facts so a PR can't quietly break them.

func TestUsage_HasAllCurrentCommands(t *testing.T) {
	required := []string{
		// onboarding
		"login", "init", "logout",
		// daily
		"sandbox", "pull", "forward", "env",
		// editor integration
		"mcp",
		// admin
		"server", "service",
		// meta
		"status", "version", "help",
	}
	for _, cmd := range required {
		if !strings.Contains(usage, cmd) {
			t.Errorf("usage missing %q — did the new command land without a help entry?", cmd)
		}
	}
}

func TestUsage_RemovedCommandsAreGone(t *testing.T) {
	// `share ls`, `share publish`, and `agentry deploy …` used to be
	// listed as actual subcommands. Now the dashboard owns those flows
	// — usage should mention them only in the "moved to dashboard"
	// note, not as commands the user can run.
	banned := []string{
		"agentry share ls",
		"agentry share publish",
		"agentry deploy ...",
		"coming soon",
	}
	for _, frag := range banned {
		if strings.Contains(usage, frag) {
			t.Errorf("usage still mentions removed surface %q", frag)
		}
	}
}

// "cluster" still ships as a hidden alias for "server" (back-compat),
// but the usage block — which is what new users read — must use
// "server" only. The dispatcher_test exercises the alias separately.
func TestUsage_PrefersServerOverCluster(t *testing.T) {
	if strings.Contains(usage, "agentry cluster") {
		t.Error("usage still advertises `agentry cluster`; should be `agentry server`")
	}
	if !strings.Contains(usage, "agentry server") {
		t.Error("usage missing `agentry server`")
	}
}

func TestUsage_PointsAtDashboardForShareAndDeploy(t *testing.T) {
	if !strings.Contains(usage, "SHARES + DEPLOYS") {
		t.Error("usage missing SHARES + DEPLOYS section")
	}
	if !strings.Contains(usage, "app.agentry.run") {
		t.Error("usage should name the dashboard URL")
	}
}
