package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// cmdSandbox dispatches `agentry sandbox *`. All paths talk to the
// bridge's /api/clusters/{cluster}/sandboxes/{...} endpoints over
// plain HTTPS with the device mTLS cert as auth — no yamux session
// needed; the bridge already proxies through to the provisioner.
//
//	agentry sandbox ls                    list sandboxes on current cluster
//	agentry sandbox use <id>              pin <id> as the default for env/forward
//	agentry sandbox current               print the pinned sandbox
//	agentry sandbox rm <id>               delete (also clears pin if it matched)
func cmdSandbox(args []string) int {
	if len(args) == 0 {
		return die("agentry sandbox: need a subcommand (ls|use|current|rm)")
	}
	switch args[0] {
	case "ls", "list":
		return sandboxLs()
	case "use":
		if len(args) < 2 {
			return die("agentry sandbox use <id>")
		}
		return sandboxUse(args[1])
	case "current":
		return sandboxCurrent()
	case "rm", "delete":
		if len(args) < 2 {
			return die("agentry sandbox rm <id>")
		}
		return sandboxRm(args[1])
	default:
		return die("agentry sandbox: unknown subcommand %q", args[0])
	}
}

func sandboxLs() int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster set; run `agentry cluster use <name>` first")
	}
	list, err := fetchSandboxes(cfg)
	if err != nil {
		return die("fetch sandboxes: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("(no sandboxes on this cluster)")
		return 0
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SandboxID < list[j].SandboxID })
	state := LoadState()
	for _, s := range list {
		marker := ""
		if s.SandboxID == state.CurrentSandbox {
			marker = " *"
		}
		fmt.Printf("%-24s %s%s\n", s.SandboxID, s.Status, marker)
	}
	return 0
}

func sandboxUse(id string) int {
	state := LoadState()
	state.CurrentSandbox = id
	if err := state.Save(); err != nil {
		return die("save state: %v", err)
	}
	fmt.Printf("agentry: current sandbox set to %q\n", id)
	return 0
}

func sandboxCurrent() int {
	state := LoadState()
	if state.CurrentSandbox == "" {
		fmt.Println("(no current sandbox — run `agentry sandbox use <id>`)")
		return 0
	}
	fmt.Println(state.CurrentSandbox)
	return 0
}

func sandboxRm(id string) int {
	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v", err)
	}
	if cfg.Cluster == "" {
		return die("no cluster set; run `agentry cluster use <name>` first")
	}
	url := strings.TrimRight(cfg.BrokerURL, "/") + "/api/clusters/" + cfg.Cluster + "/sandboxes/" + id
	req, _ := http.NewRequest("DELETE", url, nil)
	resp, err := bridgeClient(cfg).Do(req)
	if err != nil {
		return die("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return die("status=%d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// Clear the pin too — otherwise the next `agentry forward` says
	// "use --sandbox" but the user thought they had one selected.
	state := LoadState()
	if state.CurrentSandbox == id {
		state.CurrentSandbox = ""
		_ = state.Save()
	}
	fmt.Printf("agentry: sandbox %q deleted\n", id)
	return 0
}

// sandboxInfo mirrors the bridge's /api/clusters/{c}/sandboxes
// response. Local copy of the wire shape — same rationale as
// clusterInfo in cluster.go.
type sandboxInfo struct {
	SandboxID  string `json:"sandbox_id"`
	SandboxURL string `json:"sandbox_url"`
	Status     string `json:"status"`
}

func fetchSandboxes(cfg *Config) ([]sandboxInfo, error) {
	url := strings.TrimRight(cfg.BrokerURL, "/") + "/api/clusters/" + cfg.Cluster + "/sandboxes"
	req, _ := http.NewRequest("GET", url, nil)
	resp, err := bridgeClient(cfg).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Sandboxes []sandboxInfo `json:"sandboxes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

// bridgeClient builds an http.Client using the device mTLS cert. Used
// by sandbox + deployment + cluster paths that hit the bridge's
// /api/* admin endpoints over plain HTTPS (not the yamux tunnel).
// Shared because all three commands need the same TLS setup.
func bridgeClient(cfg *Config) *http.Client {
	tlsConf, err := buildClientTLS(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentry: build client TLS: %v\n", err)
		return http.DefaultClient
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}
}
