package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// deployment_status is the read-only window from the agent into the
// control plane: "did the deploy I told the user to click actually come
// up, and what's its URL?" Deploys themselves still happen in the
// dashboard (one explicit, billable, human-in-the-loop action) — this
// tool only READS status so the agent can close the loop without asking
// the user to copy-paste from the browser.
//
// It's backed by an injected hook (Client.DeploymentStatusHook) because
// the MCP client's own transport only reaches the sandbox runtime; the
// control plane is a separate origin reached over the laptop's PAT,
// which only `agentry mcp` has. When the hook is absent (binary without
// control-plane auth) the tool degrades to a clear pointer at the
// dashboard rather than an opaque error.

func registerDeploymentStatusTool(server *mcp.Server, c *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "deployment_status",
		Description: "Read the deployment(s) for a sandbox from the control plane — status (pending/building/pushing/running/failed), public URL, revision, and last status message. " +
			"USE to close the loop after you tell the user to deploy: poll this until status is `running`, then hand them the URL — no need to ask them to read it off the dashboard. " +
			"READ-ONLY: it does NOT start a deploy. Deploys are an explicit action the user takes in the dashboard's Deploy panel; if there are no rows yet, tell them to click Deploy there.",
	}, deploymentStatus(c))
}

type deploymentStatusArgs struct {
	SandboxID string `json:"sandbox_id" jsonschema:"the sandbox whose deployments you want — the sandbox_id from sandbox_create (NOT the runtime URL)"`
}

func deploymentStatus(c *Client) mcp.ToolHandlerFor[deploymentStatusArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a deploymentStatusArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxID == "" {
			return errResult("sandbox_id is required"), nil, nil
		}
		if c.DeploymentStatusHook == nil {
			// No control-plane auth in this process. Don't pretend — point
			// at the surface that does have it.
			out := map[string]any{
				"available": false,
				"note":      "deployment status isn't readable from here (this agentry has no control-plane login). View deployments in the dashboard's Deployments page.",
			}
			return jsonResult(out), out, nil
		}
		res, err := c.DeploymentStatusHook(ctx, a.SandboxID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(res), res, nil
	}
}
