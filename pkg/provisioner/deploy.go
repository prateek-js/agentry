package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agentry/agentry/pkg/errcode"
)

// DeployRequest is the body for POST /api/sandboxes/{id}/deploy.
// Defaults: tag empty → derived from sandbox id, cluster empty →
// derived from provisioner config (ClusterID).
type DeployRequest struct {
	Tag     string `json:"tag,omitempty"`
	Cluster string `json:"cluster,omitempty"`
}

// DeployResponse is the broker's mock-XDP record propagated back.
type DeployResponse struct {
	DeploymentID string `json:"deployment_id"`
	Cluster      string `json:"cluster"`
	Name         string `json:"name"`
	PublicURL    string `json:"public_url"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

// handleDeploy is POST /api/sandboxes/{id}/deploy. Reads the build
// manifest from /workspace/.build/xdp.json (caller is expected to
// have run build first), POSTs it to the broker's /api/deploy stub,
// and propagates the response. v1 doesn't fail if the manifest is
// missing — it runs build implicitly first. Simpler UX, same wire.
func (p *Provisioner) handleDeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInvalidRequest, "sandbox id missing in path"))
		return
	}
	var req DeployRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errcode.WriteJSON(w, errcode.New(errcode.InvalidRequest, "bad request body: %v", err))
			return
		}
	}
	cluster := req.Cluster
	if cluster == "" {
		cluster = p.config.ClusterID
	}
	if cluster == "" {
		errcode.WriteJSON(w, errcode.New(errcode.InvalidRequest, "cluster is required (provisioner has no ClusterID configured)"))
		return
	}

	// Implicitly build first — saves a round-trip for the LLM /
	// `xdp deploy` flow. If you want explicit control over the tag,
	// pass tag via DeployRequest (we propagate it into the build).
	manifest, err := p.runBuild(r.Context(), id, req.Tag)
	if err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInternal, "build before deploy: %v", err))
		return
	}

	// Post the manifest to the broker. The broker is reachable from
	// the provisioner via its cluster→broker session — but for the
	// stub deploy API we use plain HTTP (no auth proof needed in
	// dev). Production swaps in mTLS to the real XDP endpoint.
	resp, err := p.postDeployToBroker(r.Context(), cluster, manifest)
	if err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInternal, "post deploy: %v", err))
		return
	}
	writeJSON(w, 200, resp)
}

// runBuild is the in-process equivalent of POST .../build — we do
// the same work inline so deploy doesn't have to call its own HTTP
// endpoint. Returns the manifest the build produced.
func (p *Provisioner) runBuild(ctx context.Context, sandboxID, tag string) (BuildManifest, error) {
	// Use the build handler's logic by reusing the lockfile read +
	// manifest construction. Re-using generateDockerfile is overkill
	// here — for deploy we just need the manifest fields.
	lock, _ := p.readLockfile(ctx, sandboxID)
	if tag == "" {
		tag = fmt.Sprintf("ad-sandbox-app:%s-build", sandboxID)
	}
	m := BuildManifest{
		APIVersion: "agentry.run/v1alpha1",
		Name:       sandboxID,
		Image:      tag,
	}
	if lock != nil {
		for _, b := range lock.Bindings {
			m.Services = append(m.Services, b.Service)
		}
		m.Secrets = append(m.Secrets, lock.Secrets...)
	}
	return m, nil
}

// postDeployToBroker dials the broker and sends the manifest. The
// broker URL is the same one BrokerClient uses for the tunnel; this
// is a separate HTTP call (not over the yamux session) because the
// stub deploy API is a plain HTTP endpoint. Real XDP integration
// will move this onto the broker tunnel or a dedicated mTLS path.
func (p *Provisioner) postDeployToBroker(ctx context.Context, cluster string, manifest BuildManifest) (DeployResponse, error) {
	if p.config.BridgeURL == "" {
		return DeployResponse{}, fmt.Errorf("BROKER_URL not configured — cannot deploy")
	}
	body, _ := json.Marshal(map[string]any{
		"cluster":  cluster,
		"manifest": manifest,
	})
	url := strings.TrimRight(p.config.BridgeURL, "/") + "/api/deploy"
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Provisioner identifies itself with a service token of sorts;
	// the broker accepts any "Bearer dev-…" in DEV_MODE.
	req.Header.Set("Authorization", "Bearer dev-cluster-"+cluster)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return DeployResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return DeployResponse{}, fmt.Errorf("broker rejected deploy: %d %s", resp.StatusCode, raw)
	}
	var out DeployResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return DeployResponse{}, err
	}
	return out, nil
}
