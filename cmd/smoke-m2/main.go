// smoke-m2 is a one-shot end-to-end exerciser for M2:
//
//   1. Dial the bridge as role=device (no cert — dev mode only)
//   2. With X-Cluster set, drive the provisioner through the tunnel:
//      - POST /api/sandboxes      → create a sandbox container
//      - POST .../runtime/v1/shell/exec  → run a command inside it
//      - DELETE /api/sandboxes/<id>  → tear it down
//
// This is the "agentry CLI in 80 lines" until M4 ports the real one.
// Used in development to confirm the bridge + provisioner +
// runtime image wiring is correct end-to-end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/agentry/agentry/pkg/bridge"
	"github.com/agentry/agentry/pkg/tunnel"
)

func main() {
	brokerURL := flag.String("bridge", "http://localhost:18090", "bridge URL to dial")
	cluster := flag.String("cluster", "docker-smoke", "X-Cluster value")
	sandboxID := flag.String("sandbox", "smoke-1", "sandbox id to create + delete")
	command := flag.String("command", "echo hello agentry && uname -a", "shell command to run")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log.Printf("dialing bridge %s as device=smoke-client", *brokerURL)
	sess, err := tunnel.Dial(ctx, tunnel.DialConfig{
		BrokerURL: *brokerURL,
		Role:      tunnel.RoleDevice,
		Headers: http.Header{
			tunnel.HeaderDeviceID: []string{"smoke-client"},
		},
	})
	if err != nil {
		die("dial: %v", err)
	}
	defer sess.Close()

	client := &http.Client{
		Transport: &stamped{next: tunnel.NewRoundTripper(sess), cluster: *cluster},
	}

	log.Printf("creating sandbox %q", *sandboxID)
	createBody, _ := json.Marshal(map[string]any{
		"sandbox_id":  *sandboxID,
		"ttl_seconds": 300,
	})
	must(client, "POST", "http://bridge.invalid/api/sandboxes", createBody)

	log.Printf("running command: %s", *command)
	cmdBody, _ := json.Marshal(map[string]any{
		"command": *command,
		"timeout": 20,
	})
	must(client, "POST",
		fmt.Sprintf("http://bridge.invalid/api/sandboxes/%s/runtime/v1/shell/exec", *sandboxID),
		cmdBody)

	log.Printf("deleting sandbox %q", *sandboxID)
	must(client, "DELETE",
		fmt.Sprintf("http://bridge.invalid/api/sandboxes/%s", *sandboxID), nil)

	log.Print("✓ M2 e2e smoke passed")
}

// stamped is the device → bridge → cluster routing hop. Same shape as
// cmd/xdp's clusterStampedRT in ad-sandbox: a thin wrapper that adds
// X-Cluster to every request before forwarding to the tunnel
// RoundTripper.
type stamped struct {
	next    http.RoundTripper
	cluster string
}

func (s *stamped) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(bridge.HeaderTargetCluster, s.cluster)
	return s.next.RoundTrip(r)
}

func must(client *http.Client, method, url string, body []byte) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		die("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		die("%s %s → %d: %s", method, url, resp.StatusCode, raw)
	}
	if len(raw) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, raw, "  ", "  ") == nil {
			log.Printf("← %d\n  %s", resp.StatusCode, pretty.String())
		} else {
			log.Printf("← %d %s", resp.StatusCode, raw)
		}
	} else {
		log.Printf("← %d (empty)", resp.StatusCode)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "smoke-m2: "+format+"\n", args...)
	os.Exit(1)
}
