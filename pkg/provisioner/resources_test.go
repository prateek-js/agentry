package provisioner

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/agentry-ai/agentry/pkg/auth"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildResourceRequirementsDefaultsWhenNil(t *testing.T) {
	got, err := buildResourceRequirements(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultResources()
	for _, key := range []corev1.ResourceName{
		corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage,
	} {
		gReq := got.Requests[key]
		wReq := want.Requests[key]
		if gReq.Cmp(wReq) != 0 {
			t.Errorf("requests[%s] = %s; want %s", key, gReq.String(), wReq.String())
		}
		gLim := got.Limits[key]
		wLim := want.Limits[key]
		if gLim.Cmp(wLim) != 0 {
			t.Errorf("limits[%s] = %s; want %s", key, gLim.String(), wLim.String())
		}
	}
}

func TestBuildResourceRequirementsOverrides(t *testing.T) {
	r := &Resources{
		Requests: &ResourceList{CPU: "2", Memory: "4Gi"},
		Limits:   &ResourceList{CPU: "8", Memory: "16Gi", GPU: "2"},
	}
	got, err := buildResourceRequirements(r)
	if err != nil {
		t.Fatal(err)
	}
	cpu := got.Requests[corev1.ResourceCPU]
	if v := cpu.String(); v != "2" {
		t.Errorf("requests.cpu = %s; want 2", v)
	}
	mem := got.Requests[corev1.ResourceMemory]
	if v := mem.String(); v != "4Gi" {
		t.Errorf("requests.memory = %s; want 4Gi", v)
	}
	defaults := defaultResources()
	gotES := got.Requests[corev1.ResourceEphemeralStorage]
	defES := defaults.Requests[corev1.ResourceEphemeralStorage]
	if gotES.Cmp(defES) != 0 {
		t.Errorf("ephemeral-storage request not preserved at default")
	}
	limCPU := got.Limits[corev1.ResourceCPU]
	if v := limCPU.String(); v != "8" {
		t.Errorf("limits.cpu = %s; want 8", v)
	}
	gpu := got.Limits[gpuResourceName]
	if v := gpu.String(); v != "2" {
		t.Errorf("limits.gpu = %s; want 2", v)
	}
}

func TestBuildResourceRequirementsInvalidQuantity(t *testing.T) {
	cases := []struct {
		name string
		r    *Resources
		want string // substring expected in error
	}{
		{
			name: "bad cpu",
			r:    &Resources{Requests: &ResourceList{CPU: "two-cores"}},
			want: "requests.cpu",
		},
		{
			name: "bad memory",
			r:    &Resources{Limits: &ResourceList{Memory: "1 gigabyte"}},
			want: "limits.memory",
		},
		{
			name: "bad gpu",
			r:    &Resources{Limits: &ResourceList{GPU: "many"}},
			want: "limits.gpu",
		},
		{
			name: "bad storage",
			r:    &Resources{Limits: &ResourceList{EphemeralStorage: "1xB"}},
			want: "limits.ephemeral_storage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildResourceRequirements(tc.r)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestBuildPodResourcesAndRuntimeClass(t *testing.T) {
	spec := SandboxSpec{
		SandboxID: "abc",
		Image:     "img",
		Resources: &Resources{
			Requests: &ResourceList{CPU: "1500m", Memory: "2Gi"},
			Limits:   &ResourceList{GPU: "1"},
		},
		RuntimeClass: "gvisor",
	}
	pod, err := buildPod("default", spec)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("RuntimeClassName = %v; want gvisor", pod.Spec.RuntimeClassName)
	}
	c := pod.Spec.Containers[0]
	cpu := c.Resources.Requests[corev1.ResourceCPU]
	if v := cpu.String(); v != "1500m" {
		t.Errorf("cpu request = %s; want 1500m", v)
	}
	gpu := c.Resources.Limits[gpuResourceName]
	if v := gpu.String(); v != "1" {
		t.Errorf("gpu limit = %s; want 1", v)
	}
}

func TestBuildPodRuntimeClassUnsetByDefault(t *testing.T) {
	pod, err := buildPod("default", SandboxSpec{SandboxID: "abc", Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("RuntimeClassName should be nil by default, got %v", *pod.Spec.RuntimeClassName)
	}
}

// TestCreateHonorsResourcesAndRuntimeClass exercises the full HTTP path and
// verifies the spec recorded by the mock K8s client carries the overrides.
func TestCreateHonorsResourcesAndRuntimeClass(t *testing.T) {
	ts, mock := newTestProvisioner(t, "")

	body := `{
		"sandbox_id":"s1",
		"resources":{
			"requests":{"cpu":"2","memory":"4Gi"},
			"limits":{"cpu":"8","memory":"16Gi","gpu":"1"}
		},
		"runtime_class":"kata"
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
	if got.RuntimeClass != "kata" {
		t.Errorf("RuntimeClass = %q; want kata", got.RuntimeClass)
	}
	if got.Resources == nil || got.Resources.Limits == nil || got.Resources.Limits.GPU != "1" {
		t.Errorf("Resources GPU not threaded through: %+v", got.Resources)
	}
}

func TestCreateRejectsInvalidResourceQuantity(t *testing.T) {
	ts, mock := newTestProvisioner(t, "")

	body := `{"sandbox_id":"s1","resources":{"requests":{"cpu":"bananas"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for invalid quantity", resp.StatusCode)
	}
	if mock.PodCount() != 0 {
		t.Fatalf("pod created despite validation failure: %d pods", mock.PodCount())
	}
}

// Silence unused import warning when the test file is the only consumer of a
// package; the auth import is here for symmetry with sibling tests.
var _ = auth.HeaderName
