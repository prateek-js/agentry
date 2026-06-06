package main

import (
	"fmt"
	"os"
	"strconv"
)

// cmdCluster handles `agentry server [subcommand]` (and the legacy
// alias `agentry cluster`). With no subcommand, queries the control
// plane for the server list and shows an interactive picker — that's
// the common case after `agentry login`. Explicit subcommands stay
// supported for scripted use.
//
// The backend uses "cluster" as its wire term (the bridge keys
// connections by cluster name, the API path is /api/v1/clusters/…).
// The CLI surface uses "server" because that's how operators think
// of the thing: the box running their sandboxes.
//
//	agentry server               # interactive select
//	agentry server current       # print current
//	agentry server use <name>    # set without prompting
//	agentry server ls            # print list, no prompt
func cmdCluster(args []string) int {
	if len(args) == 0 {
		return clusterPick()
	}
	switch args[0] {
	case "current":
		return clusterCurrent()
	case "use":
		if len(args) < 2 {
			return die("agentry server use <name>")
		}
		return clusterUse(args[1])
	case "ls":
		return clusterLs()
	default:
		return die("agentry server: unknown subcommand %q", args[0])
	}
}

// clusterPick queries the control plane, shows the list, and lets the
// user pick by number. The default selection ("press enter") is
// whatever the user had configured before — so a re-run is a no-op
// confirmation. We key everything off `name` because that's the
// human-readable handle the user typed at create time; the id is
// stored alongside so headless commands can pin precisely.
func clusterPick() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry login` first)", err)
	}
	list, err := fetchClusters(cfg)
	if err != nil {
		return die("fetch server list: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("No servers yet. Open https://app.agentry.run to add one.")
		return 1
	}

	fmt.Println("Available servers:")
	defaultIdx := 1
	for i, c := range list {
		marker := ""
		if c.Name == cfg.Cluster || c.ID == cfg.Cluster {
			marker = " (current)"
			defaultIdx = i + 1
		}
		fmt.Printf("  %d) %-30s %s%s\n", i+1, c.Name, c.Status, marker)
	}
	fmt.Println()

	raw := prompt(fmt.Sprintf("Select server [%d]: ", defaultIdx))
	idx := defaultIdx
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > len(list) {
			return die("invalid selection %q", raw)
		}
		idx = n
	}
	chosen := list[idx-1].Name
	cfg.Cluster = chosen
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}
	fmt.Printf("agentry: server set to %q\n", chosen)
	return 0
}

func clusterCurrent() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		fmt.Println("(no server set — run `agentry server` to pick one)")
		return 0
	}
	fmt.Println(cfg.Cluster)
	return 0
}

func clusterUse(name string) int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry login` first)", err)
	}
	cfg.Cluster = name
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}
	fmt.Printf("agentry: server set to %q\n", name)
	return 0
}

func clusterLs() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	list, err := fetchClusters(cfg)
	if err != nil {
		return die("fetch server list: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("(no servers — open https://app.agentry.run to add one)")
		return 0
	}
	tw := tabWriter()
	defer tw.Flush()
	fmt.Fprintln(tw, "SERVER\tSTATUS\tBACKEND")
	for _, c := range list {
		name := "  " + c.Name
		if c.Name == cfg.Cluster || c.ID == cfg.Cluster {
			name = "* " + c.Name
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, c.Status, emptyAs(c.Backend, "-"))
	}
	return 0
}

// clusterInfo mirrors `ClusterSummary` from the control plane
// (`GET /api/v1/clusters`). Kept local so the CLI doesn't import the
// agentry-app module — the wire shape is the contract.
type clusterInfo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Backend    string  `json:"backend"`
	Status     string  `json:"status"`
	LastSeenAt *string `json:"last_seen_at,omitempty"`
	CreatedAt  string  `json:"created_at"`
}

// fetchClusters calls the control plane via PAT. Org filtering happens
// server-side from the bearer token's identity — the CLI never sees a
// cluster outside its own org.
func fetchClusters(cfg *Config) ([]clusterInfo, error) {
	client, err := newAppClient(cfg)
	if err != nil {
		return nil, err
	}
	var out struct {
		Clusters []clusterInfo `json:"clusters"`
	}
	if err := client.get("clusters", &out); err != nil {
		return nil, err
	}
	return out.Clusters, nil
}

// emptyAs (also referenced in main.go) — small helper to render
// fallback text when a config field is unset.
func init() { _ = os.Stderr }
