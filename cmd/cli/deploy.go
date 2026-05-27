package main

import (
	"flag"
	"fmt"
)

// cmdDeploy runs `xdp deploy --sandbox <id>` — kicks the
// build+deploy chain via the provisioner. Terminal-side equivalent
// of the deploy MCP tool. Same underlying endpoint, same wire shape.
//
// User-side trigger (not LLM-side): the user typing `xdp deploy` is
// the explicit "I want this in production" gesture. Today the MCP
// tool also exists for full LLM-driven workflows; production-tier
// safety belongs on the XDP platform itself (auto-approval to
// staging, manual approval to prod, etc).
func cmdDeploy(args []string) int {
	fs := flag.NewFlagSet("xdp deploy", flag.ContinueOnError)
	sandbox := fs.String("sandbox", "", "sandbox id to deploy (required)")
	cluster := fs.String("cluster", "", "target cluster (defaults to current cluster from config)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sandbox == "" {
		return die("xdp deploy --sandbox <id> [--cluster NAME]")
	}

	body := map[string]any{}
	if *cluster != "" {
		body["cluster"] = *cluster
	}

	fmt.Printf("xdp deploy: sandbox=%s\n", *sandbox)
	return callProvisioner("POST", "/api/sandboxes/"+*sandbox+"/deploy", body)
}
