package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfigCluster drops a minimal agentry.json carrying the given
// cluster and points $AGENTRY_CONFIG at it for the duration of the test.
func writeConfigCluster(t *testing.T, cluster string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentry.json")
	if err := os.WriteFile(path, []byte(`{"cluster":"`+cluster+`"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTRY_CONFIG", path)
}

// A pinned ref (agentry mcp --server <name>) must always report its
// pinned server and never consult the config file — so a concurrent
// `agentry server use` on the laptop can't redirect a pinned session.
func TestPinnedConfigCluster_IgnoresConfigSwitch(t *testing.T) {
	writeConfigCluster(t, "other")
	ref := newPinnedConfigCluster("pinned")
	if got := ref.Get(); got != "pinned" {
		t.Fatalf("pinned Get() = %q, want %q", got, "pinned")
	}
	// Even after a config that names a different cluster, the pin holds.
	if got := ref.Get(); got != "pinned" {
		t.Fatalf("pinned Get() after reread = %q, want %q", got, "pinned")
	}
}

// A non-pinned ref follows the config: the first Get() (TTL elapsed
// from the zero lastRead) reads the file and returns the current
// server, so `agentry server use` takes effect on the next tool call.
func TestConfigCluster_FollowsConfigSwitch(t *testing.T) {
	writeConfigCluster(t, "live")
	ref := newConfigCluster("boot")
	if got := ref.Get(); got != "live" {
		t.Fatalf("Get() = %q, want %q (should reflect config)", got, "live")
	}
}
