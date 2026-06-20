package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/agentry/agentry/pkg/tunnel"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// runtime_client.go — shared plumbing for the subcommands that talk to a
// sandbox's runtime API through the broker tunnel (logs, sh, vsc).
//
// The path the bridge exposes is
//
//	/api/sandboxes/<sid>/runtime/<runtime-path>
//
// routed to the right cluster by the X-Cluster header. Everything here
// reuses the same dial + RoundTripper the other tunneled commands use.

// dialRuntime loads config and opens a broker tunnel. The caller must
// Close the returned session.
func dialRuntime() (*Config, *yamux.Session, error) {
	cfg, _, err := LoadConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	sess, err := openTunnel(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("dial broker: %w", err)
	}
	return cfg, sess, nil
}

// runtimeClient builds a tunneled HTTP client that routes to `cluster`.
func runtimeClient(sess *yamux.Session, cluster string) *http.Client {
	rt := &clusterStampedRT{next: tunnel.NewRoundTripper(sess), cluster: cluster}
	return &http.Client{Transport: rt}
}

// sandboxRuntimeURL builds the bridge path to a sandbox runtime endpoint.
// suffix is the runtime path without a leading slash, e.g.
// "v1/project/logs?name=app".
func sandboxRuntimeURL(sandbox, suffix string) string {
	return "http://bridge.invalid/api/sandboxes/" + sandbox + "/runtime/" + suffix
}

// dialRuntimeWS opens a WebSocket to a sandbox runtime endpoint through
// the tunnel. It dials a fresh yamux stream and lets gorilla run the
// upgrade handshake over it; the broker routes by the X-Cluster header.
func dialRuntimeWS(sess *yamux.Session, cluster, sandbox, wsSuffix string) (*websocket.Conn, error) {
	d := websocket.Dialer{
		NetDialContext: func(context.Context, string, string) (net.Conn, error) {
			return sess.OpenStream()
		},
		HandshakeTimeout: 15 * 1e9, // 15s
	}
	u := "ws://bridge.invalid/api/sandboxes/" + sandbox + "/runtime/" + wsSuffix
	conn, resp, err := d.Dial(u, http.Header{"X-Cluster": {cluster}})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("ws dial: %s", resp.Status)
		}
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	return conn, nil
}

// resolveProjectName picks the sandbox's project. One project (the norm)
// auto-selects; ambiguity asks for --project.
func resolveProjectName(client *http.Client, sandbox string) (string, error) {
	resp, err := client.Get(sandboxRuntimeURL(sandbox, "v1/project/list"))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var env struct {
		Data []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", err
	}
	if len(env.Data) == 1 {
		return env.Data[0].Name, nil
	}
	if len(env.Data) == 0 {
		return "", fmt.Errorf("no project in sandbox %q — start one first", sandbox)
	}
	var running []string
	names := make([]string, 0, len(env.Data))
	for _, p := range env.Data {
		names = append(names, p.Name)
		if p.Status == "running" {
			running = append(running, p.Name)
		}
	}
	if len(running) == 1 {
		return running[0], nil
	}
	return "", fmt.Errorf("multiple projects %v in %q — pass --project", names, sandbox)
}
