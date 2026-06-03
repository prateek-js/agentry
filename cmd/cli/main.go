// xdp is the user-side daemon and CLI for ad-sandbox.
//
// In the steady state, the user has run `xdp init` once to write a
// config file and `xdp cluster use <name>` to pick which cluster to
// drive. Then Claude Desktop / Code is configured to spawn `xdp stdio`,
// which dials the configured broker over yamux and runs the MCP server
// with every outbound HTTP call routed through the tunnel.
//
// Subcommands:
//
//	xdp init [--broker URL]      write skeleton ~/.ad-sandbox/xdp.json
//	xdp cluster current          print the cluster xdp will target
//	xdp cluster use <name>       set the cluster
//	xdp stdio                    run the MCP server bound to stdio
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/agentry/agentry/pkg/bridge"
	"github.com/agentry/agentry/pkg/mcp"
	"github.com/agentry/agentry/pkg/tunnel"
)

const usage = `agentry — laptop-side daemon + CLI for agentry.run

Setup:
  agentry login                            browser auth — the usual path
  agentry logout                           drop + revoke the local token
  agentry init --app-url URL --token TOK [--name NAME]
                                           legacy device-cert enrollment
  agentry status

Cluster (the box running your sandboxes):
  agentry cluster                          interactive picker
  agentry cluster ls
  agentry cluster use <name>
  agentry cluster current

Sandbox (a container on the current cluster):
  agentry sandbox ls
  agentry sandbox use <id>                 pin <id> as default for env/forward
  agentry sandbox current
  agentry sandbox rm <id>

MCP (the integration point for Claude Desktop / Cursor / Roo):
  agentry mcp                              MCP server bound to stdin/stdout
                                           (alias: agentry stdio)

Port forwarding:
  agentry forward [<sandbox>:]<port> [--local PORT]
                                           sandbox defaults to current

Sandbox env vars:
  agentry env set NAME [VALUE] [--sandbox <id>]   (omit VALUE: hidden prompt)
  agentry env ls [--sandbox <id>]

Service catalog (cluster-scoped):
  agentry service ls
  agentry service bind <service>           cluster default; applied on every create
  agentry service bind <service> --from-env
  agentry service bind --sandbox <id> <service>     one-shot override
  agentry service binds                    list cluster defaults stored locally
  agentry service unbind <service>

Shared ports (expose a live sandbox port at a *.agentry.live URL):
  agentry share ls
  agentry share                            (use the dashboard for now)

Deployments (build prod image + run as a target — coming soon):
  agentry deploy ...

Configuration: ~/.agentry/agentry.json. Pinned sandbox: ~/.agentry/state.json.
Run "agentry login" once to authorize this machine, "agentry cluster" to
pick a target, then point your editor at "agentry mcp".
`

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	// stdio is the MCP transport on stdout; everything else logs to
	// stderr by default. Reset just to be explicit.
	log.SetOutput(os.Stderr)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "login":
		os.Exit(cmdLogin(os.Args[2:]))
	case "logout":
		os.Exit(cmdLogout(os.Args[2:]))
	case "init":
		// `init` is now an internal step that `login` triggers once
		// it has a PAT (it enrolls a device cert against the app).
		// Kept as a separate subcommand for back-compat scripts.
		os.Exit(cmdInit(os.Args[2:]))
	case "cluster":
		os.Exit(cmdCluster(os.Args[2:]))
	case "sandbox":
		os.Exit(cmdSandbox(os.Args[2:]))
	case "mcp", "stdio":
		// stdio is the legacy name kept as an alias so existing
		// Claude Desktop / Roo configs keep working.
		os.Exit(cmdMCP(os.Args[2:]))
	case "forward":
		os.Exit(cmdForward(os.Args[2:]))
	case "env":
		os.Exit(cmdEnv(os.Args[2:]))
	case "share":
		os.Exit(cmdShare(os.Args[2:]))
	case "deployment", "deploy":
		// Real Deploy lands soon (#118/#120). Until then, point users
		// at `agentry share` for live-port URLs or the dashboard for
		// the real Deploy flow (also coming there). Exit non-zero so
		// scripts notice.
		fmt.Fprintln(os.Stderr,
			"agentry deploy/deployment is coming soon —\n"+
				"  for sharing a sandbox port to a URL, use `agentry share` or the dashboard.")
		os.Exit(2)
	case "service":
		os.Exit(cmdService(os.Args[2:]))
	case "status":
		os.Exit(cmdStatus(os.Args[2:]))
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "agentry: unknown subcommand %q\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// cmdMCP is the workhorse: read config, dial broker, run the MCP
// server bound to stdin/stdout with a tunneled HTTP client. The CLI
// alias `agentry stdio` routes here so legacy editor configs (which
// spawn "agentry stdio") keep working.
func cmdMCP(_ []string) int {
	cfg, path, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry init` first; tried %s)", err, path)
	}
	if cfg.BrokerURL == "" {
		return die("config %s has no broker_url; run `agentry init` first", path)
	}
	if cfg.Cluster == "" {
		return die("no current cluster; run `agentry cluster use <name>` first")
	}

	ctx, cancel := signalContext()

	log.Printf("agentry: dialing broker %s as device=%s, cluster=%s",
		cfg.BrokerURL, cfg.DeviceID, cfg.Cluster)
	dial := tunnel.DialConfig{
		BrokerURL: cfg.BrokerURL,
		Role:      tunnel.RoleDevice,
		Headers:   http.Header{tunnel.HeaderDeviceID: []string{cfg.DeviceID}},
	}
	if cfg.DeviceCertPath != "" && cfg.DeviceKeyPath != "" {
		tlsConf, err := buildClientTLS(cfg)
		if err != nil {
			return die("build client TLS: %v", err)
		}
		dial.TLSConfig = tlsConf
	}
	sess, err := tunnel.Dial(ctx, dial)
	if err != nil {
		return die("dial broker: %v", err)
	}

	// http.Client whose transport is the tunnel RoundTripper plus a
	// per-request stamp of X-Cluster. The MCP client doesn't know
	// the bytes are tunneled — it just sees a working http.Client.
	//
	// clusterRef is a TTL-cached reader for the active cluster so
	// `agentry cluster use <name>` takes effect on the very next tool
	// call from Roo, without forcing the user to kill + restart this
	// stdio process. Reused by the post-create hook below so a new
	// sandbox's cluster-default service binds also come from the
	// current cluster, not the boot-time snapshot.
	clusterRef := newConfigCluster(cfg.Cluster)
	inner := tunnel.NewRoundTripper(sess)
	rt := &clusterStampedRT{
		next:       inner,
		getCluster: clusterRef.Get,
	}

	// Reconnect loop. yamux sessions die from idle timeouts, network
	// blips, autocert renewals on the bridge, anything that closes the
	// underlying TCP. Without this loop the MCP server stays alive but
	// every tool call returns "tunnel: session closed: no live session"
	// and the LLM caller (Roo / Claude Desktop) concludes the tunnel
	// is "down" and bails — even though the MCP subprocess itself is
	// trivially recoverable by redialing.
	//
	// On session close: log, exponential backoff, redial, SetSession.
	// In-flight requests keep using the dying session (RoundTripper's
	// atomic pointer is read-once per request); new requests after the
	// swap pick up the fresh one.
	go reconnectLoop(ctx, sess, inner, dial)
	defer func() {
		// Order matters: cancel ctx first so the reconnect goroutine
		// returns through its `<-ctx.Done()` branch and doesn't log
		// "session closed; reconnecting" on graceful shutdown. THEN
		// close whatever session is currently held.
		cancel()
		if s := inner.Session(); s != nil {
			_ = s.Close()
		}
	}()
	tunneledHTTP := &http.Client{Transport: rt}
	mcpClient := mcp.NewClient(mcp.Config{
		// ProvisionerURL has to be non-empty for the MCP client to
		// build absolute URLs for sandbox_create etc. The host is
		// ignored by the tunnel transport; only the path matters,
		// and the broker routes based on the X-Cluster header.
		ProvisionerURL: "http://bridge.invalid",
		HTTPClient:     tunneledHTTP,
		// After every successful sandbox_create, replay each
		// cluster-default service bind the user staged via
		// `agentry service bind <service>`. Real creds live in
		// ~/.agentry/services/<cluster>/; they ride the same tunnel
		// that just created the sandbox. clusterRef.Get is the same
		// TTL-cached reader the round-tripper uses, so a fresh
		// sandbox always gets the binds matching the cluster the
		// request was routed to.
		PostCreateHook: applyClusterDefaults(clusterRef.Get, tunneledHTTP),
	})

	if err := mcp.RunStdio(ctx, mcpClient); err != nil &&
		!errors.Is(err, context.Canceled) {
		return die("mcp server: %v", err)
	}
	return 0
}

// cmdStatus prints what the daemon would do if started. Does NOT dial
// the broker — it's a config-readback only, useful for "is my install
// healthy" before `agentry mcp` runs.
func cmdStatus(_ []string) int {
	cfg, path, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry init` first; tried %s)", err, path)
	}
	state := LoadState()
	fmt.Printf("config:   %s\n", path)
	fmt.Printf("device:   %s\n", cfg.DeviceID)
	fmt.Printf("broker:   %s\n", emptyAs(cfg.BrokerURL, "(not set; run `agentry init`)"))
	fmt.Printf("cluster:  %s\n", emptyAs(cfg.Cluster, "(not set; run `agentry cluster`)"))
	fmt.Printf("sandbox:  %s\n", emptyAs(state.CurrentSandbox, "(not pinned; run `agentry sandbox use <id>`)"))
	return 0
}

func emptyAs(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// signalContext returns a context that cancels on SIGTERM/SIGINT.
// The MCP RunStdio loop respects ctx so a Ctrl+C from Claude Desktop's
// subprocess management unblocks cleanly.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		cancel()
	}()
	return ctx, cancel
}

func die(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "agentry: "+format+"\n", args...)
	return 1
}

// clusterStampedRT wraps a RoundTripper and adds X-Cluster: <name>
// to every request. This is the bridge between mcp.Client (which
// addresses one logical "provisioner") and the broker (which routes
// per request based on this header).
//
// Either `cluster` (static, used by one-shot CLI subcommands that
// exit before the user could plausibly switch clusters) or `getCluster`
// (dynamic, used by the long-running `agentry mcp` stdio process so
// `agentry cluster use <name>` takes effect on the next tool call
// without restarting the child) must be set. getCluster wins when
// both are present.
type clusterStampedRT struct {
	next       http.RoundTripper
	cluster    string
	getCluster func() string
}

func (r *clusterStampedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we don't mutate the caller's request — required by
	// http.RoundTripper contract.
	req = req.Clone(req.Context())
	name := r.cluster
	if r.getCluster != nil {
		name = r.getCluster()
	}
	req.Header.Set(bridge.HeaderTargetCluster, name)
	return r.next.RoundTrip(req)
}

// configCluster reads the active cluster from ~/.agentry/agentry.json
// with a 1-second TTL cache. Lets `agentry mcp` pick up an out-of-band
// `agentry cluster use <name>` without forcing the user to kill + let
// Roo respawn the child. The per-call cost is at most one file read
// per second; tool calls take 100-1000 ms of network anyway, so the
// 10-50 µs cost is invisible.
//
// Safe for concurrent use. Logs to stderr on the first call AFTER a
// switch, so operators can correlate "my Roo started routing to a
// different cluster" with the change.
type configCluster struct {
	mu       sync.Mutex
	value    string
	lastRead time.Time
	ttl      time.Duration
	prev     string // last logged value
}

// newConfigCluster returns a cache pre-warmed with the initial value.
// The cache is poll-on-read; there's no background goroutine to clean
// up at process exit.
func newConfigCluster(initial string) *configCluster {
	return &configCluster{
		value: initial,
		prev:  initial,
		ttl:   time.Second,
	}
}

// Get returns the active cluster name. Cheap when called within the
// TTL window; one config-file read otherwise. Falls back to the last
// known good value if the file became unreadable (e.g. user deleted
// ~/.agentry/ mid-session).
func (c *configCluster) Get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastRead) < c.ttl {
		return c.value
	}
	c.lastRead = time.Now()
	cfg, _, err := LoadConfig()
	if err != nil || cfg.Cluster == "" {
		return c.value // last good
	}
	if cfg.Cluster != c.prev {
		log.Printf("agentry: cluster switched %q → %q (next tool call routes there)",
			c.prev, cfg.Cluster)
		c.prev = cfg.Cluster
	}
	c.value = cfg.Cluster
	return c.value
}

// reconnectLoop watches the current tunnel session; whenever it
// closes (idle timeout, network blip, autocert renewal on the bridge,
// anything) we redial with exponential backoff and atomically swap the
// fresh session into the RoundTripper. The MCP server keeps running
// across the gap; the next tool call after a successful reconnect
// sees a healthy session instead of "no live session".
//
// Returns when ctx is cancelled (Ctrl+C or signalContext fire).
func reconnectLoop(ctx context.Context, initial tunnelSession, rt *tunnel.RoundTripper, dial tunnel.DialConfig) {
	current := initial
	for {
		// Wait for the current session to die or ctx to cancel.
		select {
		case <-ctx.Done():
			return
		case <-current.CloseChan():
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("agentry: tunnel session closed; reconnecting")

		backoff := tunnel.NewBackoff(tunnel.DialerBackoff())
		for {
			if ctx.Err() != nil {
				return
			}
			sess, err := tunnel.Dial(ctx, dial)
			if err == nil {
				log.Printf("agentry: tunnel reconnected")
				rt.SetSession(sess)
				current = sess
				break
			}
			delay := backoff.Next()
			log.Printf("agentry: reconnect attempt %d failed: %v (retry in %s)",
				backoff.Attempts(), err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}
}

// tunnelSession is the subset of *yamux.Session reconnectLoop needs.
// Kept as a tiny interface so the loop is testable without standing up
// a real yamux session.
type tunnelSession interface {
	CloseChan() <-chan struct{}
}
