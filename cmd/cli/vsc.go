package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// cmdVsc implements `agentry vsc [<sandbox>]` — start the sandbox's
// in-browser editor (code-server) and open it. The editor is served by
// the control plane behind your dashboard login, so a browser already
// signed in to agentry opens straight in.
func cmdVsc(args []string) int {
	fs := flag.NewFlagSet("agentry vsc", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sb := resolveSandbox(fs.Arg(0))
	if sb == "" {
		return die("usage: agentry vsc [<sandbox>]\n(no sandbox given and no current sandbox set)")
	}
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no server set; run `agentry server use <name>`")
	}
	client, err := newAppClient(cfg)
	if err != nil {
		return die("%v", err)
	}
	clusters, err := fetchClusters(cfg)
	if err != nil {
		return die("list servers: %v", err)
	}
	var clusterID string
	for _, c := range clusters {
		if c.Name == cfg.Cluster {
			clusterID = c.ID
			break
		}
	}
	if clusterID == "" {
		return die("server %q not found in your org", cfg.Cluster)
	}

	// Start code-server (idempotent; first launch can take ~20s). If it
	// errors we still open — the page can be refreshed once it's up.
	fmt.Println("starting editor…")
	startPath := fmt.Sprintf("clusters/%s/sandboxes/%s/runtime/v1/ide/start", clusterID, sb)
	if err := client.do("POST", startPath, nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "agentry: warning: could not pre-start editor: %v\n", err)
	}

	u := fmt.Sprintf("%s/api/v1/clusters/%s/sandboxes/%s/ide/",
		strings.TrimRight(cfg.AppURL, "/"), clusterID, sb)
	if err := openBrowser(u); err != nil {
		fmt.Println("open this in a browser signed in to agentry:")
		fmt.Println("  " + u)
		return 0
	}
	fmt.Printf("opened %s\n", u)
	return 0
}
