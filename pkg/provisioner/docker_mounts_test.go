package provisioner

import (
	"testing"

	"github.com/docker/docker/api/types/mount"
)

// TestDockerMountsFromVolumes is the pure-data unit test for the
// Volume → mount.Mount translation: no Docker daemon required.
func TestDockerMountsFromVolumes(t *testing.T) {
	t.Run("empty input yields nil", func(t *testing.T) {
		got, err := dockerMountsFromVolumes(nil)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != nil {
			t.Fatalf("got %v; want nil", got)
		}
	})

	t.Run("host_path → bind", func(t *testing.T) {
		got, err := dockerMountsFromVolumes([]Volume{{
			Name:      "creds",
			MountPath: "/etc/sandbox/creds",
			ReadOnly:  true,
			HostPath:  &HostPathSource{Path: "/home/u/.agentry/creds"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d; want 1", len(got))
		}
		m := got[0]
		if m.Type != mount.TypeBind {
			t.Errorf("Type = %q; want bind", m.Type)
		}
		if m.Source != "/home/u/.agentry/creds" {
			t.Errorf("Source = %q", m.Source)
		}
		if m.Target != "/etc/sandbox/creds" {
			t.Errorf("Target = %q", m.Target)
		}
		if !m.ReadOnly {
			t.Errorf("ReadOnly = false; want true")
		}
	})

	t.Run("empty_dir → volume", func(t *testing.T) {
		got, err := dockerMountsFromVolumes([]Volume{{
			Name:      "scratch",
			MountPath: "/scratch",
			EmptyDir:  &EmptyDirSource{},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Type != mount.TypeVolume {
			t.Fatalf("got %+v; want one mount.TypeVolume", got)
		}
	})

	t.Run("unsupported sources rejected", func(t *testing.T) {
		cases := []Volume{
			{Name: "a", MountPath: "/a", PVC: &PVCSource{ClaimName: "x"}},
			{Name: "b", MountPath: "/b", ConfigMap: &ConfigMapSource{Name: "x"}},
			{Name: "c", MountPath: "/c", Secret: &SecretSource{Name: "x"}},
		}
		for _, v := range cases {
			if _, err := dockerMountsFromVolumes([]Volume{v}); err == nil {
				t.Errorf("expected error for volume %q, got nil", v.Name)
			}
		}
	})
}
