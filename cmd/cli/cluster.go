package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// cmdCluster handles `xdp cluster [subcommand]`. With no subcommand,
// queries the broker for the cluster list and shows an interactive
// picker — that's the common case after `xdp init`. Explicit
// subcommands stay supported for scripted use.
//
//	xdp cluster              # interactive select
//	xdp cluster current      # print current
//	xdp cluster use <name>   # set without prompting
//	xdp cluster ls           # print list, no prompt
func cmdCluster(args []string) int {
	if len(args) == 0 {
		return clusterPick()
	}
	switch args[0] {
	case "current":
		return clusterCurrent()
	case "use":
		if len(args) < 2 {
			return die("xdp cluster use <name>")
		}
		return clusterUse(args[1])
	case "ls":
		return clusterLs()
	default:
		return die("xdp cluster: unknown subcommand %q", args[0])
	}
}

// clusterPick queries the broker, shows the list, and lets the user
// pick by number. The default selection ("press enter") is whatever
// the user had configured before — so a re-run is a no-op confirmation.
func clusterPick() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `xdp init` first)", err)
	}
	list, err := fetchClusters(cfg)
	if err != nil {
		return die("fetch cluster list: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("No clusters online. Ask your operator to start one and try again.")
		return 1
	}

	fmt.Println("Available clusters:")
	defaultIdx := 1
	for i, c := range list {
		marker := ""
		if c.ID == cfg.Cluster {
			marker = " (current)"
			defaultIdx = i + 1
		}
		fmt.Printf("  %d) %s%s\n", i+1, c.ID, marker)
	}
	fmt.Println()

	raw := prompt(fmt.Sprintf("Select cluster [%d]: ", defaultIdx))
	idx := defaultIdx
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > len(list) {
			return die("invalid selection %q", raw)
		}
		idx = n
	}
	chosen := list[idx-1].ID
	cfg.Cluster = chosen
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}
	fmt.Printf("xdp: cluster set to %q\n", chosen)
	return 0
}

func clusterCurrent() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		fmt.Println("(no cluster set — run `xdp cluster` to pick one)")
		return 0
	}
	fmt.Println(cfg.Cluster)
	return 0
}

func clusterUse(name string) int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `xdp init` first)", err)
	}
	cfg.Cluster = name
	if err := cfg.Save(); err != nil {
		return die("save config: %v", err)
	}
	fmt.Printf("xdp: cluster set to %q\n", name)
	return 0
}

func clusterLs() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	list, err := fetchClusters(cfg)
	if err != nil {
		return die("fetch cluster list: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("(no clusters online)")
		return 0
	}
	for _, c := range list {
		marker := ""
		if c.ID == cfg.Cluster {
			marker = " *"
		}
		fmt.Printf("%-30s %s%s\n", c.ID, c.ConnectedAgo, marker)
	}
	return 0
}

// clusterInfo mirrors what the broker returns on /api/clusters.
// Kept local rather than importing pkg/broker just for the response
// schema — the wire shape is the contract, not the Go struct.
type clusterInfo struct {
	ID           string `json:"id"`
	Connected    bool   `json:"connected"`
	ConnectedAgo string `json:"connected_ago,omitempty"`
}

func fetchClusters(cfg *Config) ([]clusterInfo, error) {
	if cfg.BrokerURL == "" {
		return nil, fmt.Errorf("config has no broker_url — run `xdp init`")
	}
	// Two auth modes, mutually exclusive:
	//   - prod: client cert (DeviceCertPath set) → mTLS, no bearer
	//   - dev:  bearer token  (DeviceToken set)  → plain HTTPS or HTTP
	url := strings.TrimRight(cfg.BrokerURL, "/") + "/api/clusters"
	req, _ := http.NewRequest("GET", url, nil)

	if cfg.DeviceCertPath == "" || cfg.DeviceKeyPath == "" {
		return nil, fmt.Errorf("config has no device cert — run `agentry init`")
	}
	tlsConf, err := buildClientTLS(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client TLS: %w", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Clusters []clusterInfo `json:"clusters"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out.Clusters, nil
}

// emptyAs (also referenced in main.go) — small helper to render
// fallback text when a config field is unset.
func init() { _ = os.Stderr }
