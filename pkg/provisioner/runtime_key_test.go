package provisioner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRuntimeAPIKey_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{CertDir: dir}
	EnsureRuntimeAPIKey(&cfg)

	if cfg.RuntimeAPIKey == "" {
		t.Fatal("expected a key to be generated")
	}
	if len(cfg.RuntimeAPIKey) != 64 { // 32 random bytes, hex-encoded
		t.Errorf("key len = %d; want 64 hex chars", len(cfg.RuntimeAPIKey))
	}
	path := filepath.Join(dir, runtimeKeyFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not persisted: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o; want 0600", perm)
	}
	b, _ := os.ReadFile(path)
	if string(b) != cfg.RuntimeAPIKey {
		t.Errorf("persisted key %q != config key %q", b, cfg.RuntimeAPIKey)
	}
}

func TestEnsureRuntimeAPIKey_ReusesPersisted(t *testing.T) {
	dir := t.TempDir()
	cfg1 := Config{CertDir: dir}
	EnsureRuntimeAPIKey(&cfg1)

	// A second provisioner boot against the same CertDir must read the
	// same key — otherwise already-running sandboxes (which baked the
	// first key into their env) would start failing auth on restart.
	cfg2 := Config{CertDir: dir}
	EnsureRuntimeAPIKey(&cfg2)

	if cfg1.RuntimeAPIKey != cfg2.RuntimeAPIKey {
		t.Fatalf("key not stable across boots: %q vs %q", cfg1.RuntimeAPIKey, cfg2.RuntimeAPIKey)
	}
}

func TestEnsureRuntimeAPIKey_ExplicitOverrideWins(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{CertDir: dir, RuntimeAPIKey: "operator-supplied"}
	EnsureRuntimeAPIKey(&cfg)

	if cfg.RuntimeAPIKey != "operator-supplied" {
		t.Fatalf("override clobbered: %q", cfg.RuntimeAPIKey)
	}
	if _, err := os.Stat(filepath.Join(dir, runtimeKeyFile)); !os.IsNotExist(err) {
		t.Error("should not persist a file when the key is supplied explicitly")
	}
}

func TestEnsureRuntimeAPIKey_NoCertDirStaysEmpty(t *testing.T) {
	cfg := Config{} // no CertDir → pure local-dev posture
	EnsureRuntimeAPIKey(&cfg)
	if cfg.RuntimeAPIKey != "" {
		t.Fatalf("expected empty key without CertDir; got %q", cfg.RuntimeAPIKey)
	}
}

func TestDefaultListenAddrIsLoopback(t *testing.T) {
	os.Unsetenv("PROVISIONER_ADDR")
	if got := DefaultConfig().ListenAddr; got != "127.0.0.1:8002" {
		t.Fatalf("default ListenAddr = %q; want 127.0.0.1:8002 (loopback)", got)
	}
}
