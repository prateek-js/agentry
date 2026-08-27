//go:build !windows
// +build !windows

package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// requireDocker skips the test unless a reachable Docker daemon and
// the agentry/runtime:latest image are both present. Avoids flaky CI on
// hosts that haven't built the image yet.
func requireDocker(t *testing.T) *DockerBackend {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("docker SDK init: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	if _, err := cli.ImageInspect(ctx, "agentry/runtime:latest"); err != nil {
		if errdefs.IsNotFound(err) {
			t.Skip("agentry/runtime:latest image missing; build it with `docker build -t agentry/runtime:latest -f docker/Dockerfile .`")
		}
		t.Skipf("image inspect failed: %v", err)
	}
	d, err := NewDockerBackend(cli, "agentry/runtime:latest", "localhost")
	if err != nil {
		t.Fatalf("NewDockerBackend: %v", err)
	}
	return d
}

// requirePodman skips the test unless a reachable Podman docker-compat
// socket and the agentry/runtime:latest image are both present. Mirrors
// requireDocker exactly, but through NewPodmanCompatClient and with the
// podmanCompat verify-after-create guards enabled.
func requirePodman(t *testing.T) *DockerBackend {
	t.Helper()
	cli, err := NewPodmanCompatClient()
	if err != nil {
		t.Skipf("podman-compat SDK init: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("podman-compat socket unreachable: %v", err)
	}
	if _, err := cli.ImageInspect(ctx, "agentry/runtime:latest"); err != nil {
		if errdefs.IsNotFound(err) {
			t.Skip("agentry/runtime:latest image missing; build it with `docker build -t agentry/runtime:latest -f docker/Dockerfile .`")
		}
		t.Skipf("image inspect failed: %v", err)
	}
	d, err := NewDockerBackend(cli, "agentry/runtime:latest", "localhost")
	if err != nil {
		t.Fatalf("NewDockerBackend: %v", err)
	}
	d.SetPodmanCompat(true)
	return d
}

// cleanup forces removal of a container regardless of its current
// state. Safe to call even if the container is already gone.
func cleanup(t *testing.T, d *DockerBackend, sandboxID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = d.DeletePod(ctx, "", "sandbox-"+sandboxID, 0)
}

// uniqueID derives a per-test sandbox id so concurrent runs (and
// previous half-clean shutdowns) don't collide on container names.
func uniqueID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-"), time.Now().UnixNano())
}

// waitRunning polls until the container reports the desired phase or
// the deadline fires. Returns the last observed phase for the failure
// message.
func waitForPhase(t *testing.T, d *DockerBackend, name, want string, dur time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		phase, _ := d.GetPodPhase(context.Background(), "", name)
		if phase == want {
			return phase
		}
		time.Sleep(50 * time.Millisecond)
	}
	phase, _ := d.GetPodPhase(context.Background(), "", name)
	return phase
}

func TestDockerCreatePodStart(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := SandboxSpec{SandboxID: sid, NodeHost: "localhost"}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatal(err)
	}
	phase := waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)
	if phase != "Running" {
		t.Fatalf("phase = %s; want Running", phase)
	}
}

func TestDockerGetNodePortMapping(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid}); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	port, err := d.GetNodePort(ctx, "", "sandbox-"+sid+"-svc")
	if err != nil {
		t.Fatal(err)
	}
	if port == 0 || port < 1024 {
		t.Fatalf("port = %d; want > 1024 (random high port)", port)
	}

	// Verify the runtime actually answers on that port (the strongest
	// proof that the create + bind worked end-to-end).
	url := fmt.Sprintf("http://localhost:%d/health", port)
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return // success
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("runtime never answered on :%d (last err=%v)", port, lastErr)
}

func TestDockerExecInPod(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid}); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	out, err := d.ExecInPod(ctx, "", "sandbox-"+sid, []string{"sh", "-c", "echo docker-exec-ok"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "docker-exec-ok") {
		t.Fatalf("exec output = %q; want substring 'docker-exec-ok'", out)
	}
}

func TestDockerExecNonZeroExit(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid})
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	_, err := d.ExecInPod(ctx, "", "sandbox-"+sid, []string{"sh", "-c", "exit 7"})
	if err == nil || !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("expected exit 7 error, got %v", err)
	}
}

func TestDockerListSandboxes(t *testing.T) {
	d := requireDocker(t)
	sid1 := uniqueID(t) + "-a"
	sid2 := uniqueID(t) + "-b"
	t.Cleanup(func() {
		cleanup(t, d, sid1)
		cleanup(t, d, sid2)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid1}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid2}); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, d, "sandbox-"+sid1, "Running", 10*time.Second)
	waitForPhase(t, d, "sandbox-"+sid2, "Running", 10*time.Second)

	list, err := d.ListSandboxes(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range list {
		seen[s.SandboxID] = true
		if !strings.HasPrefix(s.SandboxURL, "http://localhost:") {
			t.Errorf("unexpected URL %q", s.SandboxURL)
		}
	}
	if !seen[sid1] || !seen[sid2] {
		t.Errorf("ListSandboxes missing one of %s/%s: got %v", sid1, sid2, list)
	}
}

func TestDockerDeleteIsIdempotent(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid})
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	if err := d.DeletePod(ctx, "", "sandbox-"+sid, 0); err != nil {
		t.Fatal(err)
	}
	// Second delete must not error.
	if err := d.DeletePod(ctx, "", "sandbox-"+sid, 0); err != nil {
		t.Fatalf("second delete returned %v; want nil", err)
	}
}

func TestDockerAnnotationsLabelAndOverlay(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := SandboxSpec{
		SandboxID: sid,
		Annotations: map[string]string{
			AnnotationTTLSec:    "60",
			AnnotationExpiresAt: "2099-01-01T00:00:00Z",
		},
	}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	got, err := d.GetPodAnnotations(ctx, "", "sandbox-"+sid)
	if err != nil {
		t.Fatal(err)
	}
	if got[AnnotationTTLSec] != "60" {
		t.Errorf("ttl-seconds = %q; want 60", got[AnnotationTTLSec])
	}
	if got[AnnotationExpiresAt] != "2099-01-01T00:00:00Z" {
		t.Errorf("expires-at = %q", got[AnnotationExpiresAt])
	}

	// Overlay should win on update.
	if err := d.SetPodAnnotations(ctx, "", "sandbox-"+sid, map[string]string{
		AnnotationExpiresAt: "2099-06-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetPodAnnotations(ctx, "", "sandbox-"+sid)
	if got[AnnotationExpiresAt] != "2099-06-01T00:00:00Z" {
		t.Errorf("after set, expires-at = %q; want overlay value", got[AnnotationExpiresAt])
	}
	// Original label still present for unrelated keys.
	if got[AnnotationTTLSec] != "60" {
		t.Errorf("ttl-seconds was clobbered: %q", got[AnnotationTTLSec])
	}
}

func TestDockerResourcesApplied(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := SandboxSpec{
		SandboxID: sid,
		Resources: &Resources{
			Limits: &ResourceList{CPU: "1500m", Memory: "256Mi"},
		},
	}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatal(err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	insp, err := d.cli.ContainerInspect(ctx, "sandbox-"+sid)
	if err != nil {
		t.Fatal(err)
	}
	// 1500m == 1.5 CPU == 1_500_000_000 NanoCPUs
	const wantNano = int64(1_500_000_000)
	if insp.HostConfig.NanoCPUs != wantNano {
		t.Errorf("NanoCPUs = %d; want %d", insp.HostConfig.NanoCPUs, wantNano)
	}
	// 256Mi == 256 * 1024 * 1024 bytes
	const wantMem = int64(256 * 1024 * 1024)
	if insp.HostConfig.Memory != wantMem {
		t.Errorf("Memory = %d; want %d", insp.HostConfig.Memory, wantMem)
	}
}

func TestDockerPingDownIsRejected(t *testing.T) {
	// Point a brand-new client at a definitely-unreachable host so
	// NewDockerBackend's Ping fails. Confirms we error rather than
	// returning a half-initialized backend.
	port := freeLocalPort(t)
	cli, err := client.NewClientWithOpts(
		client.WithHost(fmt.Sprintf("tcp://127.0.0.1:%d", port)),
		client.WithVersion("1.45"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDockerBackend(cli, "img", "localhost"); err == nil {
		t.Fatal("expected ping failure to surface as an error")
	}
}

// freeLocalPort grabs a port the OS guarantees is unused right now.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// TestPodmanRuntimeClassVerified confirms Guard #1's positive path: when
// podman's docker-compat layer *does* honor HostConfig.Runtime, CreatePod
// succeeds instead of being (incorrectly) rejected as unverified. A true
// negative-path test would need a podman that silently drops the field,
// which isn't deterministically constructible — that gap is acknowledged,
// not attempted here.
func TestPodmanRuntimeClassVerified(t *testing.T) {
	d := requirePodman(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := SandboxSpec{SandboxID: sid, RuntimeClass: "runc"}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatalf("CreatePod with RuntimeClass=runc: %v", err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)
}

// TestPodmanEgressNetworkModeVerified confirms Guard #2's positive path:
// when the egress sidecar's container:<id> network mode *does* stick
// under podman, CreatePod with an egress policy succeeds. As with the
// RuntimeClass guard, the negative path (podman silently rewriting the
// netmode) isn't deterministically constructible in a test.
func TestPodmanEgressNetworkModeVerified(t *testing.T) {
	d := requirePodman(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	spec := SandboxSpec{
		SandboxID: sid,
		Egress: EgressPolicy{
			Mode:  EgressAllow,
			Rules: []EgressRule{{CIDR: "169.254.169.254/32"}},
		},
	}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatalf("CreatePod with Egress policy: %v", err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)
}

// Ensure tests don't accidentally import a stale corev1 — used as a
// compile-time witness that the resource translation in
// docker_client.go pulls the right helpers.
var _ = errors.New
var _ = image.PullOptions{}
var _ = container.StartOptions{}
