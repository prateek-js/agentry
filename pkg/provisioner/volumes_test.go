package provisioner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestValidateVolumes_OK(t *testing.T) {
	vols := []Volume{
		{Name: "data", MountPath: "/data", PVC: &PVCSource{ClaimName: "my-pvc"}},
		{Name: "scratch", MountPath: "/scratch", EmptyDir: &EmptyDirSource{Medium: "Memory", SizeLimit: "1Gi"}},
		{Name: "secrets", MountPath: "/etc/secrets", Secret: &SecretSource{Name: "db-creds"}, ReadOnly: true},
		{Name: "config", MountPath: "/etc/app", ConfigMap: &ConfigMapSource{Name: "app-conf"}},
	}
	if err := validateVolumes(vols); err != nil {
		t.Fatal(err)
	}
}

func TestValidateVolumes_RejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		vol  Volume
		want string
	}{
		{
			name: "missing name",
			vol:  Volume{MountPath: "/x", EmptyDir: &EmptyDirSource{}},
			want: "name is required",
		},
		{
			name: "bad name",
			vol:  Volume{Name: "BadName", MountPath: "/x", EmptyDir: &EmptyDirSource{}},
			want: "DNS-1123",
		},
		{
			name: "missing mount path",
			vol:  Volume{Name: "v", EmptyDir: &EmptyDirSource{}},
			want: "mount_path is required",
		},
		{
			name: "relative mount path",
			vol:  Volume{Name: "v", MountPath: "data", EmptyDir: &EmptyDirSource{}},
			want: "absolute",
		},
		{
			name: "non-canonical mount path",
			vol:  Volume{Name: "v", MountPath: "/data/../x", EmptyDir: &EmptyDirSource{}},
			want: "canonical form",
		},
		{
			name: "reserved mount path",
			vol:  Volume{Name: "v", MountPath: "/proc", EmptyDir: &EmptyDirSource{}},
			want: "reserved",
		},
		{
			name: "no source",
			vol:  Volume{Name: "v", MountPath: "/x"},
			want: "source",
		},
		{
			name: "two sources",
			vol: Volume{Name: "v", MountPath: "/x",
				EmptyDir: &EmptyDirSource{}, PVC: &PVCSource{ClaimName: "p"}},
			want: "exactly one",
		},
		{
			name: "pvc missing claim",
			vol:  Volume{Name: "v", MountPath: "/x", PVC: &PVCSource{}},
			want: "pvc.claim_name",
		},
		{
			name: "host_path missing path",
			vol:  Volume{Name: "v", MountPath: "/x", HostPath: &HostPathSource{}},
			want: "host_path.path",
		},
		{
			name: "host_path not absolute",
			vol:  Volume{Name: "v", MountPath: "/x", HostPath: &HostPathSource{Path: "data"}},
			want: "absolute",
		},
		{
			name: "configmap missing name",
			vol:  Volume{Name: "v", MountPath: "/x", ConfigMap: &ConfigMapSource{}},
			want: "config_map.name",
		},
		{
			name: "secret missing name",
			vol:  Volume{Name: "v", MountPath: "/x", Secret: &SecretSource{}},
			want: "secret.name",
		},
		{
			name: "bad empty_dir medium",
			vol:  Volume{Name: "v", MountPath: "/x", EmptyDir: &EmptyDirSource{Medium: "Disk"}},
			want: "empty_dir.medium",
		},
		{
			name: "bad empty_dir size",
			vol:  Volume{Name: "v", MountPath: "/x", EmptyDir: &EmptyDirSource{SizeLimit: "1 big chunk"}},
			want: "empty_dir.size_limit",
		},
		{
			name: "sub_path absolute",
			vol:  Volume{Name: "v", MountPath: "/x", SubPath: "/abs", EmptyDir: &EmptyDirSource{}},
			want: "sub_path",
		},
		{
			name: "sub_path escape",
			vol:  Volume{Name: "v", MountPath: "/x", SubPath: "../etc", EmptyDir: &EmptyDirSource{}},
			want: "'..'",
		},
		{
			name: "sub_path interior escape",
			vol:  Volume{Name: "v", MountPath: "/x", SubPath: "a/../../b", EmptyDir: &EmptyDirSource{}},
			want: "'..'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVolumes([]Volume{tc.vol})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateVolumes_DuplicateName(t *testing.T) {
	err := validateVolumes([]Volume{
		{Name: "v", MountPath: "/a", EmptyDir: &EmptyDirSource{}},
		{Name: "v", MountPath: "/b", EmptyDir: &EmptyDirSource{}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("want duplicate-name error, got %v", err)
	}
}

func TestValidateVolumes_DuplicateMountPath(t *testing.T) {
	err := validateVolumes([]Volume{
		{Name: "a", MountPath: "/data", EmptyDir: &EmptyDirSource{}},
		{Name: "b", MountPath: "/data", EmptyDir: &EmptyDirSource{}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate mount_path") {
		t.Fatalf("want duplicate-mount-path error, got %v", err)
	}
}

func TestBuildVolumes_EmptyReturnsNil(t *testing.T) {
	v, m, err := buildVolumes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if v != nil || m != nil {
		t.Errorf("empty input should yield nil slices, got vols=%v mounts=%v", v, m)
	}
}

func TestBuildVolumes_MapsAllSources(t *testing.T) {
	defaultMode := int32(0o400)
	optional := true
	in := []Volume{
		{Name: "data", MountPath: "/data", PVC: &PVCSource{ClaimName: "pvc-1"}, ReadOnly: true},
		{Name: "host", MountPath: "/mnt/host", HostPath: &HostPathSource{Path: "/var/lib/sandbox", Type: "DirectoryOrCreate"}},
		{Name: "cm", MountPath: "/etc/app", ConfigMap: &ConfigMapSource{Name: "app-conf", DefaultMode: &defaultMode}},
		{Name: "sec", MountPath: "/etc/secrets", Secret: &SecretSource{Name: "db-creds", Optional: &optional}, SubPath: "db"},
		{Name: "tmp", MountPath: "/tmp/work", EmptyDir: &EmptyDirSource{Medium: "Memory", SizeLimit: "256Mi"}},
	}
	vols, mounts, err := buildVolumes(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != len(in) || len(mounts) != len(in) {
		t.Fatalf("len mismatch: vols=%d mounts=%d want=%d", len(vols), len(mounts), len(in))
	}

	// PVC
	if vols[0].PersistentVolumeClaim == nil {
		t.Error("pvc volume source missing")
	}
	if !vols[0].PersistentVolumeClaim.ReadOnly {
		t.Error("read_only not propagated to PVC source")
	}
	// HostPath
	if vols[1].HostPath == nil || *vols[1].HostPath.Type != "DirectoryOrCreate" {
		t.Error("host_path source or type missing")
	}
	// ConfigMap
	if vols[2].ConfigMap == nil || vols[2].ConfigMap.DefaultMode == nil || *vols[2].ConfigMap.DefaultMode != defaultMode {
		t.Error("config_map source or default_mode missing")
	}
	// Secret
	if vols[3].Secret == nil || vols[3].Secret.Optional == nil || !*vols[3].Secret.Optional {
		t.Error("secret source or optional missing")
	}
	// EmptyDir
	if vols[4].EmptyDir == nil || vols[4].EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Error("empty_dir source or medium missing")
	}
	if vols[4].EmptyDir.SizeLimit == nil || vols[4].EmptyDir.SizeLimit.String() != "256Mi" {
		t.Errorf("empty_dir size_limit = %v; want 256Mi", vols[4].EmptyDir.SizeLimit)
	}

	// Mounts
	if mounts[0].MountPath != "/data" || !mounts[0].ReadOnly {
		t.Errorf("mount[0]: %+v", mounts[0])
	}
	if mounts[3].SubPath != "db" {
		t.Errorf("mount[3].SubPath = %q; want db", mounts[3].SubPath)
	}
}

func TestBuildPodIncludesVolumes(t *testing.T) {
	spec := SandboxSpec{
		SandboxID: "s1", Image: "img",
		Volumes: []Volume{
			{Name: "data", MountPath: "/data", PVC: &PVCSource{ClaimName: "pvc-1"}},
		},
	}
	pod, err := buildPod("default", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != "data" {
		t.Errorf("pod.Spec.Volumes = %+v", pod.Spec.Volumes)
	}
	if len(pod.Spec.Containers[0].VolumeMounts) != 1 ||
		pod.Spec.Containers[0].VolumeMounts[0].MountPath != "/data" {
		t.Errorf("container VolumeMounts = %+v", pod.Spec.Containers[0].VolumeMounts)
	}
}

func TestCreateAcceptsVolumes(t *testing.T) {
	ts, mock := newTestProvisioner(t, "")

	body := `{
		"sandbox_id":"s1",
		"volumes":[
			{"name":"data","mount_path":"/data","pvc":{"claim_name":"shared"}}
		]
	}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	got, ok := mock.Spec("sandbox-s1")
	if !ok {
		t.Fatal("spec not recorded")
	}
	if len(got.Volumes) != 1 || got.Volumes[0].PVC == nil ||
		got.Volumes[0].PVC.ClaimName != "shared" {
		t.Errorf("Volumes not threaded: %+v", got.Volumes)
	}
}

func TestCreateRejectsInvalidVolume(t *testing.T) {
	ts, mock := newTestProvisioner(t, "")

	// Two volumes claiming the same MountPath — should 400.
	body := `{
		"sandbox_id":"s1",
		"volumes":[
			{"name":"a","mount_path":"/data","pvc":{"claim_name":"x"}},
			{"name":"b","mount_path":"/data","pvc":{"claim_name":"y"}}
		]
	}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	if mock.PodCount() != 0 {
		t.Fatalf("pod created despite validation failure")
	}

	var got struct{ Message string }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Message, "duplicate mount_path") {
		t.Errorf("error message = %q; want a duplicate-mount-path hint", got.Message)
	}
}
