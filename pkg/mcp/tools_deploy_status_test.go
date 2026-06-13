package mcp

import (
	"context"
	"errors"
	"testing"
)

// deployment_status is hook-backed: pkg/mcp owns the tool shell (arg
// validation, nil-hook degradation, error passthrough); the CLI owns the
// control-plane call. These pin the shell.

func TestDeploymentStatus_RequiresSandboxID(t *testing.T) {
	c := NewClient(Config{})
	res, _, err := deploymentStatus(c)(context.Background(), nil, deploymentStatusArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("missing sandbox_id should be a tool error")
	}
}

func TestDeploymentStatus_NilHookDegradesGracefully(t *testing.T) {
	// No DeploymentStatusHook → not an error, a pointer at the dashboard.
	c := NewClient(Config{})
	res, data, err := deploymentStatus(c)(context.Background(), nil, deploymentStatusArgs{SandboxID: "sb1"})
	if err != nil {
		t.Fatal(err)
	}
	if res != nil && res.IsError {
		t.Fatal("a missing hook should NOT be a tool error — it should degrade")
	}
	m := data.(map[string]any)
	if avail, _ := m["available"].(bool); avail {
		t.Error("available should be false without a hook")
	}
	if _, ok := m["note"]; !ok {
		t.Error("degraded response should carry a note pointing at the dashboard")
	}
}

func TestDeploymentStatus_PassesThroughHookResult(t *testing.T) {
	want := map[string]any{"available": true, "count": 2}
	var gotSandbox string
	c := NewClient(Config{
		DeploymentStatusHook: func(_ context.Context, sandboxID string) (any, error) {
			gotSandbox = sandboxID
			return want, nil
		},
	})
	_, data, err := deploymentStatus(c)(context.Background(), nil, deploymentStatusArgs{SandboxID: "sb_xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if gotSandbox != "sb_xyz" {
		t.Errorf("hook got sandbox %q; want sb_xyz", gotSandbox)
	}
	m := data.(map[string]any)
	if m["count"].(int) != 2 {
		t.Errorf("hook result not passed through: %+v", m)
	}
}

func TestDeploymentStatus_HookErrorIsToolError(t *testing.T) {
	c := NewClient(Config{
		DeploymentStatusHook: func(_ context.Context, _ string) (any, error) {
			return nil, errors.New("not logged in — run `agentry login`")
		},
	})
	res, _, err := deploymentStatus(c)(context.Background(), nil, deploymentStatusArgs{SandboxID: "sb1"})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("a hook error should surface as a tool error")
	}
}
