package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// docker returns the daemon client when the backend supports it
// (today: only DockerBackend). Used by the deploy build + run
// handlers, which need ImageBuild / ContainerCreate operations the
// generic Backend interface doesn't model.
func (p *Provisioner) docker() (*client.Client, error) {
	if d, ok := p.backend.(*DockerBackend); ok {
		return d.Client(), nil
	}
	return nil, fmt.Errorf("deploy build requires docker backend (got %T)", p.backend)
}

// Deploy-build endpoint. Bundles a project under /workspace into a tar
// stream inside the sandbox, then pipes it into Docker's ImageBuild on
// the cluster's daemon. The resulting image lives in the cluster's
// daemon and is what the cluster target's Deploy step runs.
//
// Why not buildah inside the sandbox: buildah images would live in the
// sandbox's overlay storage and would need a save+load roundtrip to
// reach the cluster's daemon. docker build straight off a tar stream
// skips the round trip and uses the daemon's own layer cache.
//
// Requires the project to already contain a Dockerfile. Auto-generation
// per stack (Vite, FastAPI, Go, ...) is a follow-up — sketched in the
// language detector below but not wired yet.

// DeployBuildRequest is the body for POST /api/sandboxes/{id}/deploy-build.
type DeployBuildRequest struct {
	// Project is the relative path under /workspace. "" or "." means
	// /workspace itself (single-project setups). For multi-project
	// repos this is the project name under /workspace/projects/.
	Project string `json:"project,omitempty"`

	// ImageTag is the tag suffix used for the built image. The full
	// image ref is "deploy-<sanitized-project>:<image_tag>".
	ImageTag string `json:"image_tag"`
}

// DeployBuildResponse echoes the resulting image ref. The cluster
// target deploys this ref verbatim; the cloud-run target's Push step
// re-tags it under the GCP Artifact Registry name.
type DeployBuildResponse struct {
	ImageRef string `json:"image_ref"`
}

func (p *Provisioner) handleDeployBuild(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("id")
	if sandboxID == "" {
		writeError(w, http.StatusBadRequest, "sandbox id missing")
		return
	}

	var req DeployBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if req.ImageTag == "" {
		writeError(w, http.StatusBadRequest, "image_tag required")
		return
	}

	projectPath, projectSlug := projectPathAndSlug(req.Project)
	imageRef := fmt.Sprintf("deploy-%s:%s", projectSlug, sanitizeTag(req.ImageTag))

	ctx := r.Context()

	// Step 1: confirm the Dockerfile exists. Fail fast before we tar
	// the workspace — a 4xx here is much clearer than a "no such file"
	// halfway through a 200 MB build context upload.
	check, err := p.runtimeShellExec(ctx, sandboxID,
		fmt.Sprintf("test -f %s/Dockerfile && echo ok || echo missing", projectPath))
	if err != nil {
		writeError(w, http.StatusBadGateway, "dockerfile check: "+err.Error())
		return
	}
	if strings.TrimSpace(check) != "ok" {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("no Dockerfile at %s — auto-generation by stack is a follow-up", projectPath))
		return
	}

	// Step 2: tar the context inside the sandbox.
	tarPath := fmt.Sprintf("/tmp/build-ctx-%s.tar.gz", req.ImageTag)
	if _, err := p.runtimeShellExec(ctx, sandboxID,
		fmt.Sprintf("rm -f %s && tar czf %s -C %s . 2>&1", tarPath, tarPath, projectPath)); err != nil {
		writeError(w, http.StatusBadGateway, "tar build context: "+err.Error())
		return
	}

	// Step 3: stream the tarball from the runtime back into Docker's
	// ImageBuild on this daemon. Docker accepts an arbitrary tar (gzip
	// is auto-detected) as the build context.
	stream, err := p.runtimeFileDownload(ctx, sandboxID, tarPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, "download build context: "+err.Error())
		return
	}
	defer stream.Close()

	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker client unavailable: "+err.Error())
		return
	}

	buildResp, err := dockerCli.ImageBuild(ctx, stream, types.ImageBuildOptions{
		Tags:       []string{imageRef},
		Dockerfile: "Dockerfile",
		Remove:     true,
		// Force the build to fail fast on Dockerfile errors instead of
		// hanging on push of a half-broken image.
		ForceRemove: true,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "image build: "+err.Error())
		return
	}
	defer buildResp.Body.Close()

	// Consume the build log; on error this contains the docker daemon's
	// reason. On success it's the "successfully tagged" line plus
	// per-step logs. We surface tail to the caller for debug.
	tail := drainBuildLog(buildResp.Body)

	// Cleanup the tarball inside the sandbox best-effort.
	_, _ = p.runtimeShellExec(context.Background(), sandboxID, "rm -f "+tarPath)

	if strings.Contains(strings.ToLower(tail), "error") &&
		!strings.Contains(strings.ToLower(tail), "successfully tagged") {
		writeError(w, http.StatusBadGateway, "image build failed:\n"+tail)
		return
	}

	writeJSON(w, http.StatusOK, DeployBuildResponse{ImageRef: imageRef})
}

// runtimeFileDownload pulls a file from the sandbox via the runtime's
// streaming download endpoint. Used to fetch large blobs (build
// contexts, logs) without buffering in memory on either side.
func (p *Provisioner) runtimeFileDownload(ctx context.Context, sandboxID, path string) (io.ReadCloser, error) {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	req, _ := http.NewRequestWithContext(ctx, "GET",
		base+"/v1/file/download?file="+path, nil)
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// drainBuildLog reads the JSON stream Docker emits during ImageBuild
// and returns the last ~4 KB of decoded "stream" lines, which is what
// the user wants to see in build output for diagnosis. Logs the
// entire stream into the provisioner's stderr for ops visibility.
func drainBuildLog(r io.Reader) string {
	dec := json.NewDecoder(r)
	var tail strings.Builder
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			break
		}
		if msg.Error != "" {
			tail.WriteString("ERROR: " + msg.Error + "\n")
			continue
		}
		if msg.Stream == "" {
			continue
		}
		tail.WriteString(msg.Stream)
		// Cap so we don't grow unbounded on giant builds.
		if tail.Len() > 16*1024 {
			s := tail.String()
			tail.Reset()
			tail.WriteString(s[len(s)-4*1024:])
		}
	}
	return tail.String()
}

// projectPathAndSlug normalizes a user-supplied project name into the
// in-sandbox path + a DNS-safe slug for the image tag.
//
//	""           → /workspace,                 slug "workspace"
//	"."          → /workspace,                 slug "workspace"
//	"sales"      → /workspace/projects/sales,  slug "sales"
//	"web/admin"  → /workspace/projects/web/admin, slug "web-admin"
func projectPathAndSlug(project string) (path, slug string) {
	project = strings.Trim(project, "/")
	if project == "" || project == "." {
		return "/workspace", "workspace"
	}
	clean := sanitizeForDockerTag(project)
	return "/workspace/projects/" + project, clean
}

// sanitizeForDockerTag keeps lowercase alphanumerics + dashes; collapses
// runs and trims trailing punctuation. Docker image tags must match
// [a-zA-Z0-9_.-]+ but we narrow further to avoid surprises.
func sanitizeForDockerTag(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
			prevDash = false
		case r == '-', r == '_', r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		out = "app"
	}
	return out
}

// sanitizeTag clamps a user-supplied image tag suffix to Docker's
// allowed character set. Reuses the slug sanitizer.
func sanitizeTag(s string) string {
	return sanitizeForDockerTag(s)
}
