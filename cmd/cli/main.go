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
	"syscall"
	"time"

	"github.com/agentry/agentry/pkg/bridge"
	"github.com/agentry/agentry/pkg/mcp"
	"github.com/agentry/agentry/pkg/tunnel"
)

const usage = `agentry — laptop-side daemon + CLI for agentry.run

Usage:
  agentry init --app-url URL --token TOK [--name NAME]
  agentry cluster                          (interactive picker)
  agentry cluster current
  agentry cluster use <name>
  agentry cluster ls
  agentry stdio                            (MCP server bound to stdin/stdout)
  agentry forward <sandbox>:<port> [--local PORT]
  agentry env set --sandbox <id> NAME [VALUE]    (omit VALUE for hidden prompt)
  agentry env list --sandbox <id>
  agentry service list                            (catalog of cluster services)
  agentry service bind <service>                  (cluster-default: stored locally, applied to every new sandbox)
  agentry service bind <service> --from-env       (cluster-default, scripted)
  agentry service bind --sandbox <id> <service>  (one-shot override for one sandbox)
  agentry service binds                           (list cluster defaults stored on this laptop)
  agentry service unbind <service>                (drop a cluster default)
  agentry status
  agentry help

Configuration lives in ~/.agentry/agentry.json. Run "agentry init" once
on a new machine using the token from the dashboard, then "agentry
cluster" to pick a target, then point Claude Desktop / Cursor / Roo
at "agentry stdio" for the MCP integration. Use "agentry forward" to
expose a port running inside a sandbox on a local port so you can
hit it from your browser.
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
	case "init":
		os.Exit(cmdInit(os.Args[2:]))
	case "cluster":
		os.Exit(cmdCluster(os.Args[2:]))
	case "stdio":
		os.Exit(cmdStdio(os.Args[2:]))
	case "forward":
		os.Exit(cmdForward(os.Args[2:]))
	case "env":
		os.Exit(cmdEnv(os.Args[2:]))
	case "deploy":
		os.Exit(cmdDeploy(os.Args[2:]))
	case "service":
		os.Exit(cmdService(os.Args[2:]))
	case "status":
		os.Exit(cmdStatus(os.Args[2:]))
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "xdp: unknown subcommand %q\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// cmdStdio is the workhorse: read config, dial broker, run the MCP
// server bound to stdin/stdout with a tunneled HTTP client.
func cmdStdio(_ []string) int {
	cfg, path, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `xdp init` first; tried %s)", err, path)
	}
	if cfg.BrokerURL == "" {
		return die("config %s has no broker_url; run `xdp init --broker URL` first", path)
	}
	if cfg.Cluster == "" {
		return die("no current cluster; run `xdp cluster use <name>` first")
	}

	ctx, cancel := signalContext()

	log.Printf("xdp: dialing broker %s as device=%s, cluster=%s",
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
	inner := tunnel.NewRoundTripper(sess)
	rt := &clusterStampedRT{
		next:    inner,
		cluster: cfg.Cluster,
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
		// `xdp service bind <service>`. Real creds live in
		// ~/.ad-sandbox/services/<cluster>/; they ride the same
		// tunnel that just created the sandbox.
		PostCreateHook: applyClusterDefaults(cfg.Cluster, tunneledHTTP),
	})

	if err := mcp.RunStdio(ctx, mcpClient); err != nil &&
		!errors.Is(err, context.Canceled) {
		return die("mcp server: %v", err)
	}
	return 0
}

// cmdStatus prints what the daemon would do if started. Does NOT dial
// the broker — it's a config-readback only, useful for "is my install
// healthy" before `xdp stdio` runs.
func cmdStatus(_ []string) int {
	cfg, path, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `xdp init` first; tried %s)", err, path)
	}
	fmt.Printf("config:   %s\n", path)
	fmt.Printf("device:   %s\n", cfg.DeviceID)
	fmt.Printf("broker:   %s\n", emptyAs(cfg.BrokerURL, "(not set; run `xdp init --broker URL`)"))
	fmt.Printf("cluster:  %s\n", emptyAs(cfg.Cluster, "(not set; run `xdp cluster use <name>`)"))
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
	fmt.Fprintf(os.Stderr, "xdp: "+format+"\n", args...)
	return 1
}

// clusterStampedRT wraps a RoundTripper and adds X-Cluster: <name>
// to every request. This is the bridge between mcp.Client (which
// addresses one logical "provisioner") and the broker (which routes
// per request based on this header).
type clusterStampedRT struct {
	next    http.RoundTripper
	cluster string
}

func (r *clusterStampedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we don't mutate the caller's request — required by
	// http.RoundTripper contract.
	req = req.Clone(req.Context())
	req.Header.Set(bridge.HeaderTargetCluster, r.cluster)
	return r.next.RoundTrip(req)
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
		log.Printf("xdp: tunnel session closed; reconnecting")

		backoff := tunnel.NewBackoff(tunnel.DialerBackoff())
		for {
			if ctx.Err() != nil {
				return
			}
			sess, err := tunnel.Dial(ctx, dial)
			if err == nil {
				log.Printf("xdp: tunnel reconnected")
				rt.SetSession(sess)
				current = sess
				break
			}
			delay := backoff.Next()
			log.Printf("xdp: reconnect attempt %d failed: %v (retry in %s)",
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
