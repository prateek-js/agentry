package main

import (
	"context"
	"fmt"
	"strings"
)

// deploymentStatusHook backs the MCP `deployment_status` tool. The MCP
// client's own transport only reaches the sandbox runtime over the
// tunnel; deployments live in the control plane, a separate origin the
// laptop reaches with its PAT. So this hook uses appClient (Bearer PAT)
// rather than the tunneled HTTP client the other tools share.
//
// We list the org's deployments and filter to the requested sandbox
// CLIENT-SIDE. The org-wide list endpoint already org-scopes server-side
// (the PAT carries OrgID), so the only thing left is matching sandbox_id.
// Returning a trimmed shape keeps the model's context lean — it doesn't
// need env keys or target internals to answer "is it up and what's the
// URL".
//
// getCfg is read per-invocation: the stdio process is long-lived and a
// re-login could rotate the token underneath us. A missing/loginless
// config returns a friendly error the model can relay verbatim.
func deploymentStatusHook(getCfg func() *Config) func(context.Context, string) (any, error) {
	return func(ctx context.Context, sandboxID string) (any, error) {
		cfg := getCfg()
		client, err := newAppClient(cfg)
		if err != nil {
			return nil, err
		}

		// Mirror of agentry-app's augmented deployment row. Only the
		// fields the model needs to answer "did it come up + where".
		type depRow struct {
			ID          string `json:"id"`
			Project     string `json:"project"`
			SandboxID   string `json:"sandbox_id"`
			Status      string `json:"status"`
			StatusMsg   string `json:"status_msg"`
			URL         string `json:"url"`
			Revision    int    `json:"revision"`
			ClusterName string `json:"cluster_name"`
			UpdatedAt   string `json:"updated_at"`
		}
		var resp struct {
			Deployments []depRow `json:"deployments"`
		}
		if err := client.get("deployments", &resp); err != nil {
			return nil, err
		}

		matches := make([]depRow, 0, 4)
		for _, d := range resp.Deployments {
			if d.SandboxID == sandboxID {
				matches = append(matches, d)
			}
		}

		out := map[string]any{
			"available":   true,
			"sandbox_id":  sandboxID,
			"deployments": matches,
			"count":       len(matches),
		}
		if len(matches) == 0 {
			out["note"] = "no deployments for this sandbox yet. Deploys start in the dashboard's Deploy panel — tell the user to click Deploy there, then poll this again."
		} else {
			// A one-line summary so the model doesn't have to reason over
			// the array to give the user a status sentence.
			var running, failed, building int
			for _, d := range matches {
				switch d.Status {
				case "running":
					running++
				case "failed":
					failed++
				case "pending", "building", "pushing":
					building++
				}
			}
			out["summary"] = fmt.Sprintf("%d deployment(s): %d running, %d in-progress, %d failed",
				len(matches), running, building, failed)
		}
		return out, nil
	}
}

// deploymentStatusAvailable reports whether the laptop has the
// control-plane login the hook needs. Used to decide whether to wire the
// hook at all — a nil hook makes the MCP tool degrade to a dashboard
// pointer instead of surfacing a login error on every call.
func deploymentStatusAvailable(cfg *Config) bool {
	return cfg != nil && strings.TrimSpace(cfg.AppURL) != "" && strings.TrimSpace(cfg.APIToken) != ""
}

// deploymentStatusHookIfAvailable returns the hook only when the laptop
// is logged in to the control plane. Captures cfg for the session — the
// PAT is stable across a single `agentry mcp` run.
func deploymentStatusHookIfAvailable(cfg *Config) func(context.Context, string) (any, error) {
	if !deploymentStatusAvailable(cfg) {
		return nil
	}
	return deploymentStatusHook(func() *Config { return cfg })
}
