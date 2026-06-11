package main

import (
	"fmt"
	"sort"
)

// cmdSandbox dispatches `agentry sandbox *`. All paths talk to the
// control plane's /api/v1/clusters/{name}/sandboxes/{...} endpoints
// with the user's PAT. The control plane proxies through to the
// bridge over its admin mTLS cert, so the CLI never has to manage
// device certs for these read/delete flows.
//
//	agentry sandbox ls                    list sandboxes on current cluster
//	agentry sandbox use <id>              pin <id> as the default for env/service commands
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
		return die("no server set; run `agentry server use <name>` first")
	}
	list, err := fetchSandboxes(cfg)
	if err != nil {
		return die("fetch sandboxes: %v", err)
	}
	if len(list) == 0 {
		fmt.Println("(no sandboxes on this server)")
		return 0
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SandboxID < list[j].SandboxID })
	state := LoadState()
	tw := tabWriter()
	defer tw.Flush()
	fmt.Fprintln(tw, "SANDBOX\tSTATUS\tURL")
	for _, s := range list {
		name := s.SandboxID
		if s.SandboxID == state.CurrentSandbox {
			name = "* " + s.SandboxID
		} else {
			name = "  " + s.SandboxID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, s.Status, emptyAs(s.SandboxURL, "-"))
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
		return die("no server set; run `agentry server use <name>` first")
	}
	client, err := newAppClient(cfg)
	if err != nil {
		return die("%v", err)
	}
	if err := client.delete("clusters/" + cfg.Cluster + "/sandboxes/" + id); err != nil {
		return die("delete: %v", err)
	}
	// Clear the pin too — otherwise the next `agentry env`/`service`
	// call says "use --sandbox" but the user thought they had one
	// selected.
	state := LoadState()
	if state.CurrentSandbox == id {
		state.CurrentSandbox = ""
		_ = state.Save()
	}
	fmt.Printf("agentry: sandbox %q deleted\n", id)
	return 0
}

// sandboxInfo mirrors `SandboxSummary` returned by the control plane's
// `GET /api/v1/clusters/{id}/sandboxes`. The control plane proxies the
// underlying call to the bridge using its admin mTLS cert, then
// re-emits the snapshot — same shape, scoped to the calling org.
type sandboxInfo struct {
	SandboxID  string `json:"sandbox_id"`
	SandboxURL string `json:"sandbox_url"`
	Status     string `json:"status"`
}

func fetchSandboxes(cfg *Config) ([]sandboxInfo, error) {
	client, err := newAppClient(cfg)
	if err != nil {
		return nil, err
	}
	var out struct {
		Sandboxes []sandboxInfo `json:"sandboxes"`
	}
	if err := client.get("clusters/"+cfg.Cluster+"/sandboxes", &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}
