//go:build !windows
// +build !windows

package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"
)

// curl is the inside-sandbox HTTP probe used by these tests. We need
// --connect-timeout so a blackholed packet doesn't hang the test, and
// -sS so we get clean stderr on failure.
const curlCmd = "curl -sS --max-time 5 --connect-timeout 4 -o /dev/null -w '%{http_code}' "

// execIn runs cmd inside the named sandbox and returns stdout + the
// (possibly nil) error. Wrapper around ExecInPod that keeps the test
// bodies readable.
func execIn(t *testing.T, d *DockerBackend, name string, cmd ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return d.ExecInPod(ctx, "", name, cmd)
}

// TestDockerEgressAllowMode_BlocksListedHosts is the "soft jail" case:
// the sandbox can reach the internet broadly, but specific addresses
// (here: the AWS IMDS endpoint) are dropped at the netns boundary.
// This is the closest analog to runtm.com's default posture.
func TestDockerEgressAllowMode_BlocksListedHosts(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	spec := SandboxSpec{
		SandboxID: sid,
		Egress: EgressPolicy{
			Mode: EgressAllow,
			Rules: []EgressRule{
				// IMDS lives on a link-local that's the classic privilege-
				// escalation target — block it explicitly even though the
				// world is otherwise open.
				{CIDR: "169.254.169.254/32"},
			},
		},
	}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	// Sanity: the runtime is up and the netns is healthy.
	if out, err := execIn(t, d, "sandbox-"+sid, "sh", "-c", curlCmd+"https://example.com/"); err != nil {
		t.Fatalf("baseline https://example.com failed: %v (out=%q)", err, out)
	} else if !strings.HasPrefix(out, "200") {
		t.Fatalf("baseline example.com returned %q; want 200", out)
	}

	// The block. curl should fail to connect; we expect a non-zero exit
	// surfaced as a Go error from ExecInPod.
	out, err := execIn(t, d, "sandbox-"+sid, "sh", "-c", curlCmd+"http://169.254.169.254/latest/meta-data/")
	if err == nil {
		t.Fatalf("expected curl to fail against blocked IMDS; got out=%q", out)
	}

	// Tamper check: a sandbox without CAP_NET_ADMIN must not be able to
	// flush the rules. nft prints the cap error to stderr, which
	// ExecInPod folds into the returned error message.
	_, err = execIn(t, d, "sandbox-"+sid, "sh", "-c", "nft flush ruleset")
	if err == nil {
		t.Fatal("expected nft flush to fail (no NET_ADMIN inside sandbox), but it succeeded")
	}
}

// TestDockerEgressDenyMode_AllowsOnlyListed is the "default deny"
// posture: nothing escapes unless an explicit rule lets it through.
// Here we allow HTTPS to anywhere; HTTP must fail.
func TestDockerEgressDenyMode_AllowsOnlyListed(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	spec := SandboxSpec{
		SandboxID: sid,
		Egress: EgressPolicy{
			Mode: EgressDeny,
			Rules: []EgressRule{
				{CIDR: "0.0.0.0/0", Proto: "tcp", Ports: []int{443}},
			},
		},
	}
	if err := d.CreatePod(ctx, "", spec); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	// HTTPS goes through.
	out, err := execIn(t, d, "sandbox-"+sid, "sh", "-c", curlCmd+"https://example.com/")
	if err != nil {
		t.Fatalf("https://example.com under deny+443: %v (out=%q)", err, out)
	}
	if !strings.HasPrefix(out, "200") && !strings.HasPrefix(out, "301") && !strings.HasPrefix(out, "302") {
		t.Fatalf("https://example.com returned %q; want 2xx/3xx", out)
	}

	// HTTP must be blocked — port 80 isn't in the allow list.
	_, err = execIn(t, d, "sandbox-"+sid, "sh", "-c", curlCmd+"http://example.com/")
	if err == nil {
		t.Fatal("http://example.com should be blocked under deny+443 only, but curl succeeded")
	}
}

// TestDockerEgressNoPolicy_IsTransparent verifies that the zero-value
// EgressPolicy installs no rules at all — important because the vast
// majority of sandboxes today don't set a policy and shouldn't pay any
// cost for the new code path.
func TestDockerEgressNoPolicy_IsTransparent(t *testing.T) {
	d := requireDocker(t)
	sid := uniqueID(t)
	t.Cleanup(func() { cleanup(t, d, sid) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.CreatePod(ctx, "", SandboxSpec{SandboxID: sid}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	waitForPhase(t, d, "sandbox-"+sid, "Running", 10*time.Second)

	// nft list with no rules installed → empty ruleset, no error.
	out, err := execIn(t, d, "sandbox-"+sid, "sh", "-c", "nft list ruleset 2>&1; echo done")
	if err != nil {
		t.Fatalf("nft list ruleset failed: %v (out=%q)", err, out)
	}
	if strings.Contains(out, "ad_sandbox_egress") {
		t.Fatalf("expected no ad_sandbox_egress table, got: %s", out)
	}
}
