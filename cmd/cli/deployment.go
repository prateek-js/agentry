package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// cmdDeployment dispatches `agentry deployment *`. Read-only for now:
//
//	agentry deployment ls       list public URLs on the current cluster
//
// publish + unpublish live in the dashboard. They go through the
// control plane (app.agentry.run) so they pick up org auth and audit
// rows, and the CLI doesn't have a token path to app.agentry.run yet
// — a follow-up will add `agentry login` and unlock CLI publishing.
func cmdDeployment(args []string) int {
	if len(args) == 0 {
		return die("agentry deployment: need a subcommand (ls)")
	}
	switch args[0] {
	case "ls", "list":
		return deploymentLs()
	case "publish", "unpublish":
		fmt.Fprintf(os.Stderr,
			"agentry deployment %s isn't wired into the CLI yet —\n"+
				"  use the dashboard: https://app.agentry.run\n",
			args[0])
		return 1
	default:
		return die("agentry deployment: unknown subcommand %q", args[0])
	}
}

func deploymentLs() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster set; run `agentry cluster use <name>` first")
	}
	url := strings.TrimRight(cfg.BrokerURL, "/") + "/api/deploy-routes"
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := bridgeClient(cfg).Do(req)
	if err != nil {
		return die("fetch routes: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return die("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var wrap struct {
		Routes []struct {
			Hostname  string `json:"hostname"`
			ClusterID string `json:"cluster_id"`
			SandboxID string `json:"sandbox_id"`
			Port      int    `json:"port"`
			AuthMode  string `json:"auth_mode"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return die("decode: %v", err)
	}
	// Filter to the current cluster — the bridge holds routes for
	// every cluster it serves, but the user only cares about theirs.
	shown := 0
	for _, r := range wrap.Routes {
		if r.ClusterID != cfg.Cluster {
			continue
		}
		fmt.Printf("https://%-34s sandbox=%s port=%d auth=%s\n",
			r.Hostname, r.SandboxID, r.Port, r.AuthMode)
		shown++
	}
	if shown == 0 {
		fmt.Println("(no deployments published for this cluster)")
	}
	return 0
}
