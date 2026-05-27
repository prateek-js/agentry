package provisioner

import (
	"os"
	"reflect"
	"testing"
)

func TestDefaultVolumesFromEnv(t *testing.T) {
	t.Run("unset env yields nil", func(t *testing.T) {
		t.Setenv("SANDBOX_DEFAULT_CREDS_DIR", "")
		if got := defaultVolumesFromEnv(); got != nil {
			t.Fatalf("got %v; want nil", got)
		}
	})

	t.Run("set env + dir exists yields creds bind-mount", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SANDBOX_DEFAULT_CREDS_DIR", dir)
		got := defaultVolumesFromEnv()
		if len(got) != 1 {
			t.Fatalf("len = %d; want 1", len(got))
		}
		v := got[0]
		if v.Name != "sandbox-creds" {
			t.Errorf("Name = %q", v.Name)
		}
		if v.MountPath != "/etc/sandbox/creds" {
			t.Errorf("MountPath = %q", v.MountPath)
		}
		if !v.ReadOnly {
			t.Errorf("ReadOnly = false; want true")
		}
		if v.HostPath == nil || v.HostPath.Path != dir {
			t.Errorf("HostPath = %+v", v.HostPath)
		}
	})

	t.Run("set env + dir missing yields nil (soft fail)", func(t *testing.T) {
		// A path that won't exist — uses tempdir as a parent so the
		// nested name is guaranteed unique and absent.
		missing := t.TempDir() + "/does-not-exist"
		t.Setenv("SANDBOX_DEFAULT_CREDS_DIR", missing)
		if got := defaultVolumesFromEnv(); got != nil {
			t.Fatalf("got %v; want nil (missing dir should soft-fail)", got)
		}
	})

	t.Run("set env to a file (not dir) yields nil", func(t *testing.T) {
		// Same soft-fail behavior when the path exists but isn't a directory.
		f, err := os.CreateTemp(t.TempDir(), "creds-file")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		t.Setenv("SANDBOX_DEFAULT_CREDS_DIR", f.Name())
		if got := defaultVolumesFromEnv(); got != nil {
			t.Fatalf("got %v; want nil (file should soft-fail)", got)
		}
	})
}

func TestMergeDefaultVolumes(t *testing.T) {
	creds := Volume{
		Name:      "sandbox-creds",
		MountPath: "/etc/sandbox/creds",
		ReadOnly:  true,
		HostPath:  &HostPathSource{Path: "/host/creds"},
	}

	t.Run("nil defaults is a no-op", func(t *testing.T) {
		req := []Volume{{Name: "x", MountPath: "/x", HostPath: &HostPathSource{Path: "/y"}}}
		got := mergeDefaultVolumes(req, nil)
		if !reflect.DeepEqual(got, req) {
			t.Fatalf("got %+v; want %+v", got, req)
		}
	})

	t.Run("default appended when no conflict", func(t *testing.T) {
		req := []Volume{{Name: "data", MountPath: "/data", HostPath: &HostPathSource{Path: "/d"}}}
		got := mergeDefaultVolumes(req, []Volume{creds})
		if len(got) != 2 {
			t.Fatalf("len = %d; want 2", len(got))
		}
		if got[1].Name != creds.Name || got[1].MountPath != creds.MountPath {
			t.Errorf("appended volume = %+v; want creds default", got[1])
		}
	})

	t.Run("caller name takes precedence", func(t *testing.T) {
		override := Volume{Name: "sandbox-creds", MountPath: "/somewhere/else", HostPath: &HostPathSource{Path: "/x"}}
		got := mergeDefaultVolumes([]Volume{override}, []Volume{creds})
		if len(got) != 1 || got[0].MountPath != "/somewhere/else" {
			t.Fatalf("got %+v; want only the caller's override", got)
		}
	})

	t.Run("caller mount_path takes precedence", func(t *testing.T) {
		override := Volume{Name: "user-creds", MountPath: "/etc/sandbox/creds", HostPath: &HostPathSource{Path: "/x"}}
		got := mergeDefaultVolumes([]Volume{override}, []Volume{creds})
		if len(got) != 1 || got[0].Name != "user-creds" {
			t.Fatalf("got %+v; want only the caller's override", got)
		}
	})

	t.Run("inputs are not mutated", func(t *testing.T) {
		req := []Volume{{Name: "data", MountPath: "/data", HostPath: &HostPathSource{Path: "/d"}}}
		origLen := len(req)
		_ = mergeDefaultVolumes(req, []Volume{creds})
		if len(req) != origLen {
			t.Fatalf("req mutated: len %d → %d", origLen, len(req))
		}
	})
}

// Ensures the Setenv helper isn't mistakenly leaking between tests.
func TestSandboxCredsEnvIsolation(t *testing.T) {
	if v := os.Getenv("SANDBOX_DEFAULT_CREDS_DIR"); v != "" {
		t.Fatalf("env leaked into TestSandboxCredsEnvIsolation: %q", v)
	}
}
