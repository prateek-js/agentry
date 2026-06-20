package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
)

// cmdLogs implements `agentry logs [<sandbox>] [-f] [-n N] [--project P]`.
// Snapshot by default; -f/--follow streams the project's log tail live
// over the runtime SSE endpoint. The sandbox defaults to the current one.
func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("agentry logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow log output (stream)")
	fs.BoolVar(follow, "follow", false, "follow log output (stream)")
	lines := fs.Int("n", 100, "lines to show (snapshot mode)")
	fs.IntVar(lines, "lines", 100, "lines to show (snapshot mode)")
	project := fs.String("project", "", "project name (default: the sandbox's project)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sb := resolveSandbox(fs.Arg(0))
	if sb == "" {
		return die("usage: agentry logs [<sandbox>] [-f] [-n N]\n(no sandbox given and no current sandbox set)")
	}

	cfg, sess, err := dialRuntime()
	if err != nil {
		return die("%v", err)
	}
	defer sess.Close()
	client := runtimeClient(sess, cfg.Cluster)

	name := *project
	if name == "" {
		if name, err = resolveProjectName(client, sb); err != nil {
			return die("%v", err)
		}
	}

	if *follow {
		resp, err := client.Get(sandboxRuntimeURL(sb, "v1/project/logs/stream?name="+url.QueryEscape(name)))
		if err != nil {
			return die("stream: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return die("stream status=%d", resp.StatusCode)
		}
		// SSE: lines arrive as "data: <line>" separated by blank lines.
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if s, ok := strings.CutPrefix(line, "data: "); ok {
				fmt.Println(s)
			} else if strings.HasPrefix(line, "event: error") {
				return die("server: project %q not found", name)
			}
		}
		return 0
	}

	resp, err := client.Get(sandboxRuntimeURL(sb,
		fmt.Sprintf("v1/project/logs?name=%s&lines=%d", url.QueryEscape(name), *lines)))
	if err != nil {
		return die("logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return die("logs status=%d", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Lines []string `json:"lines"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return die("decode: %v", err)
	}
	for _, l := range env.Data.Lines {
		fmt.Println(l)
	}
	return 0
}
