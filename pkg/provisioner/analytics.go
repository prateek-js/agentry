package provisioner

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// analytics is a best-effort PostHog capture for sandbox-lifecycle usage
// metrics. Sandbox create/delete happen here on the provisioner (via MCP)
// and never touch the control plane, so the control plane can't meter
// them — this is the only place they're observable.
//
// Enabled only when POSTHOG_API_KEY is set; every call is async and
// swallows errors so analytics can never affect sandbox operations.
// Metadata-only: cluster name, sandbox id, owning org — never app data.
type analytics struct {
	key, host, cluster, certDir string
	http                        *http.Client
	orgOnce                     sync.Once
	orgID                       string
}

// newAnalytics returns nil (a valid no-op for the track method) unless
// POSTHOG_API_KEY is set.
func newAnalytics(cfg Config) *analytics {
	key := os.Getenv("POSTHOG_API_KEY")
	if key == "" {
		return nil
	}
	host := os.Getenv("POSTHOG_HOST")
	if host == "" {
		host = "https://us.i.posthog.com"
	}
	return &analytics{
		key:     key,
		host:    strings.TrimRight(host, "/"),
		cluster: cfg.ClusterID,
		certDir: cfg.CertDir,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// org resolves (once) the owning org_id from the cluster cert's URI SAN
// (urn:agentry:org:<id>), which the control plane stamps at enrollment.
// Cached; empty when the cert is absent or carries no org SAN.
func (a *analytics) org() string {
	a.orgOnce.Do(func() {
		if a.certDir == "" {
			return
		}
		b, err := os.ReadFile(filepath.Join(a.certDir, "cluster.crt"))
		if err != nil {
			return
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return
		}
		for _, u := range cert.URIs {
			if s := u.String(); strings.HasPrefix(s, "urn:agentry:org:") {
				a.orgID = strings.TrimPrefix(s, "urn:agentry:org:")
				return
			}
		}
	})
	return a.orgID
}

// track fires a metadata-only event to PostHog. Nil-safe + async.
func (a *analytics) track(event, sandboxID string) {
	if a == nil {
		return
	}
	org := a.org()
	go func() {
		distinct := org
		if distinct == "" {
			distinct = "cluster:" + a.cluster
		}
		props := map[string]any{
			"cluster":    a.cluster,
			"sandbox_id": sandboxID,
			"source":     "provisioner",
			"org_id":     org,
		}
		if org != "" {
			props["$groups"] = map[string]any{"organization": org}
		}
		body, err := json.Marshal(map[string]any{
			"api_key":     a.key,
			"event":       event,
			"distinct_id": distinct,
			"properties":  props,
		})
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.host+"/capture/", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.http.Do(req)
		if err != nil {
			log.Printf("analytics: posthog capture %q: %v", event, err)
			return
		}
		_ = resp.Body.Close()
	}()
}
