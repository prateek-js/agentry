//go:build !windows
// +build !windows

package provisioner

import (
	"context"
	"testing"
	"time"
)

// TestDockerHardening_StrictDefault is the load-bearing test for the
// sandbox security posture. Spins up a real container with default
// (strict) mode and asserts every hardening flag the audit identified:
//
//   - CapDrop includes "ALL"
//   - CapAdd is empty (no SYS_ADMIN)
//   - SecurityOpt contains "no-new-privileges:true"
//   - SecurityOpt does NOT contain seccomp=unconfined / apparmor=unconfined
//   - PidsLimit is bounded (4096)
//
// If any of these regress, sandbox-escape risk goes up materially —
// hence the asserts at the wire/HostConfig level rather than at code-
// review time.
func TestDockerHardening_StrictDefault(t *testing.T) {
	d := requireDocker(t)
	// Explicit: default constructor is strict. Don't call SetBuilderMode.
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := SandboxSpec{SandboxID: sid}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if phase := waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second); phase != "Running" {
		t.Fatalf("container never reached Running, last phase=%s", phase)
	}

	info, err := d.cli.ContainerInspect(ctx, "sandbox-"+sid)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	if !contains(info.HostConfig.CapDrop, "ALL") {
		t.Errorf("CapDrop missing ALL: %v", info.HostConfig.CapDrop)
	}
	if len(info.HostConfig.CapAdd) > 0 {
		t.Errorf("CapAdd should be empty in strict mode, got %v", info.HostConfig.CapAdd)
	}
	if !contains(info.HostConfig.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("SecurityOpt missing no-new-privileges:true: %v", info.HostConfig.SecurityOpt)
	}
	if contains(info.HostConfig.SecurityOpt, "seccomp=unconfined") {
		t.Errorf("strict mode must not be seccomp=unconfined: %v", info.HostConfig.SecurityOpt)
	}
	if contains(info.HostConfig.SecurityOpt, "apparmor=unconfined") {
		t.Errorf("strict mode must not be apparmor=unconfined: %v", info.HostConfig.SecurityOpt)
	}
	if info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != 4096 {
		got := int64(-1)
		if info.HostConfig.PidsLimit != nil {
			got = *info.HostConfig.PidsLimit
		}
		t.Errorf("PidsLimit = %d, want 4096", got)
	}
}

// TestDockerHardening_BuilderMode covers the opposite axis: when an
// operator opts into builder mode (the buildah/OCI use case),
// SYS_ADMIN comes back + seccomp/apparmor unconfined + bigger
// ulimits — and no-new-privileges stays on so a child process can't
// further escalate. PidsLimit holds.
func TestDockerHardening_BuilderMode(t *testing.T) {
	d := requireDocker(t)
	d.SetBuilderMode(true)
	t.Cleanup(func() { d.SetBuilderMode(false) }) // reset for other tests
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if phase := waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second); phase != "Running" {
		t.Fatalf("container never reached Running, last phase=%s", phase)
	}

	info, err := d.cli.ContainerInspect(ctx, "sandbox-"+sid)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}

	// Docker normalizes "SYS_ADMIN" → "CAP_SYS_ADMIN" in the inspect
	// output; accept either form.
	if !contains(info.HostConfig.CapAdd, "SYS_ADMIN") && !contains(info.HostConfig.CapAdd, "CAP_SYS_ADMIN") {
		t.Errorf("builder mode missing SYS_ADMIN: %v", info.HostConfig.CapAdd)
	}
	if !contains(info.HostConfig.SecurityOpt, "seccomp=unconfined") {
		t.Errorf("builder mode missing seccomp=unconfined: %v", info.HostConfig.SecurityOpt)
	}
	if !contains(info.HostConfig.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("builder mode still must include no-new-privileges:true: %v", info.HostConfig.SecurityOpt)
	}
	if info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != 4096 {
		got := int64(-1)
		if info.HostConfig.PidsLimit != nil {
			got = *info.HostConfig.PidsLimit
		}
		t.Errorf("PidsLimit = %d, want 4096", got)
	}
}

// TestDockerHardening_RuntimePortBindsLoopback pins the runtime API
// to 127.0.0.1 — the single most damaging regression in the docker
// backend would be flipping back to 0.0.0.0, which republishes
// shell-exec / file-write / code-exec to anyone who can reach the
// host on the ephemeral port. The bridge mTLS gate + org-scope checks
// only apply to traffic that came through the tunnel; a direct hit on
// the host port skips all of it. Assert at the HostConfig level so a
// silent bind change at code review time can't slip through.
func TestDockerHardening_RuntimePortBindsLoopback(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if phase := waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second); phase != "Running" {
		t.Fatalf("container never reached Running, last phase=%s", phase)
	}

	info, err := d.cli.ContainerInspect(ctx, "sandbox-"+sid)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	bindings, ok := info.HostConfig.PortBindings[containerHTTPPort]
	if !ok || len(bindings) == 0 {
		t.Fatalf("no host binding for %s", containerHTTPPort)
	}
	for _, b := range bindings {
		// Empty HostIP also publishes to all interfaces — equally bad.
		// Loopback is the ONLY acceptable value.
		if b.HostIP != loopbackHostIP {
			t.Errorf("HostConfig binding HostIP = %q; want %q (publishing the runtime API beyond loopback exposes shell-exec / file-write / code-exec)", b.HostIP, loopbackHostIP)
		}
	}
	// Inspect's NetworkSettings carries the *resolved* binding the
	// daemon installed. Assert the same property there, since that's
	// what userspace sees with `docker ps` / iptables.
	if info.NetworkSettings != nil {
		nsBindings, ok := info.NetworkSettings.Ports[containerHTTPPort]
		if ok {
			for _, b := range nsBindings {
				if b.HostIP != loopbackHostIP {
					t.Errorf("NetworkSettings binding HostIP = %q; want %q", b.HostIP, loopbackHostIP)
				}
			}
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
