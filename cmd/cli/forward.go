package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/agentry/agentry/pkg/bridge"
	"github.com/agentry/agentry/pkg/tunnel"
	"github.com/hashicorp/yamux"
)

// cmdForward binds a local TCP port and pipes every accepted
// connection through the broker tunnel to a port inside a sandbox.
//
//	agentry forward [<sandbox>:]<port> [--local PORT]
//
// If the argument is just `<port>`, the sandbox defaults to whatever
// `agentry sandbox use` pinned. So once you've picked a sandbox the
// common case is `agentry forward 5432`.
//
// Raw TCP, no HTTP. Every accepted local conn opens one yamux stream
// on the device session, writes "CONNECT <sandbox>:<port>" + the
// X-Cluster header, reads the 200 back, then byte-pumps.
//
// HTTP / WebSocket / SSE all ride this because they're TCP too. So
// do psql, redis-cli, ssh, mongo, mysql, debugger attach, anything
// else that opens a TCP socket. The tunnel doesn't parse the bytes.
func cmdForward(args []string) int {
	fs := flag.NewFlagSet("agentry forward", flag.ContinueOnError)
	localPortFlag := fs.Int("local", -1, "local TCP port (default = remote port; 0 = OS-assigned)")
	flagArgs, posArgs := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(posArgs) != 1 {
		return die("agentry forward [<sandbox>:]<port> [--local PORT]")
	}

	// Two shapes:
	//   "sb:4321" -> explicit sandbox
	//   "4321"    -> pinned sandbox from state.json
	var sandbox, portStr string
	if i := strings.Index(posArgs[0], ":"); i >= 0 {
		sandbox = posArgs[0][:i]
		portStr = posArgs[0][i+1:]
	} else {
		sandbox = resolveSandbox("")
		portStr = posArgs[0]
	}
	if sandbox == "" {
		return die("no sandbox — pass <sandbox>:<port> or run `agentry sandbox use <id>` first")
	}
	if portStr == "" {
		return die("port missing")
	}
	remotePort, err := strconv.Atoi(portStr)
	if err != nil || remotePort < 1 || remotePort > 65535 {
		return die("invalid remote port %q", portStr)
	}
	if remotePort == 8080 {
		return die("cannot forward port 8080 (runtime API)")
	}
	localPort := *localPortFlag
	if localPort < 0 {
		localPort = remotePort
	}

	cfg, _, err := LoadConfig()
	if err != nil {
		return die("load config: %v (run `agentry init` first)", err)
	}
	if cfg.BrokerURL == "" {
		return die("config has no broker_url; run `agentry init`")
	}
	if cfg.Cluster == "" {
		return die("no server set; run `agentry server`")
	}

	ctx, cancel := signalContext()
	defer cancel()

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
	defer sess.Close()

	listenAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return die("bind %s: %v", listenAddr, err)
	}
	defer listener.Close()
	actualLocal := listener.Addr().(*net.TCPAddr).Port

	fmt.Printf("agentry forward: tcp://localhost:%d → sandbox %s, port %d (server=%s)\n",
		actualLocal, sandbox, remotePort, cfg.Cluster)
	fmt.Println("press Ctrl+C to stop")

	// Close the listener when ctx ends so the Accept loop unblocks.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	target := fmt.Sprintf("%s:%d", sandbox, remotePort)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			return die("accept: %v", err)
		}
		go handleForwardConn(ctx, conn, sess, target, cfg.Cluster)
	}
}

// handleForwardConn handles one accepted local TCP connection: open
// a yamux stream, send the CONNECT, read the 200, pipe bytes.
func handleForwardConn(ctx context.Context, conn net.Conn, sess *yamux.Session, target, cluster string) {
	defer conn.Close()

	stream, err := sess.OpenStream()
	if err != nil {
		// Tunnel went away. Closing conn signals to whoever dialed
		// us that we can't deliver. (No 502 — they're not speaking
		// HTTP necessarily.)
		return
	}
	defer stream.Close()

	headers := http.Header{
		bridge.HeaderTargetCluster: []string{cluster},
	}
	if err := tunnel.WriteConnect(stream, target, headers); err != nil {
		return
	}
	br := bufio.NewReader(stream)
	if err := tunnel.ReadConnectResponse(br); err != nil {
		// Broker / cluster / runtime rejected the CONNECT. Without
		// HTTP semantics on the local side, the only signal we have
		// is closing the conn. Log to stderr so the user sees why.
		fmt.Fprintf(os.Stderr, "agentry forward: %v\n", err)
		return
	}
	upstream := tunnel.NewDrainedReadWriteCloser(stream, br)
	_ = tunnel.CopyStreams(ctx, conn, upstream, tunnel.CopyOptions{})
}

// splitFlagsAndPositionals lets the user write either
// "agentry forward sb:8000 --local 8000" or
// "agentry forward --local 8000 sb:8000" — Go's stdlib flag parser stops
// at the first positional, so we pre-sort the args.
func splitFlagsAndPositionals(args []string) (flags, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, positionals
}
