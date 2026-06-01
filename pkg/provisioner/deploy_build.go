package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// Deploy-build endpoint.
//
// User app → optimized image, with no Dockerfile required. Powered by
// Railpack (https://github.com/railwayapp/railpack), the Nixpacks
// successor: detects the project's stack (Node + Vite/Next, Python,
// Go, Ruby, Rust, etc.) and builds via BuildKit. The image lands in
// the cluster's Docker daemon — that's where the deployment container
// runs from.
//
// We shell out to the railpack binary rather than importing as a Go
// library. Upside: cleaner upgrade story (pin a version in the
// Dockerfile, bump on demand); we don't track railpack's internal API.
// Downside: an extra process per build, slightly more error-prone log
// parsing. Acceptable for now.
//
// Architecture inside the provisioner container:
//
//   1. Sandbox creates a tar of /workspace/<project> via runtime API.
//   2. Provisioner extracts the tar to /tmp/buildctx-<deployment>/.
//   3. Provisioner runs `railpack build <ctxdir> --name deploy-<...>`
//      with BUILDKIT_HOST pointing at the BuildKit container we
//      manage (see ensureBuildKit below).
//   4. Image lands in the local Docker daemon; deploy_run uses it.
//
// Escape hatch for advanced apps: drop a `railpack.json` in the
// project to override detection / install commands / start command.
// We don't honor user-supplied Dockerfiles in this path — if you want
// that level of control, you'd write a deploy_build hook (later).

// DeployBuildRequest is the body for POST /api/sandboxes/{id}/deploy-build.
type DeployBuildRequest struct {
	// Project is the relative path under /workspace. "" or "." means
	// /workspace itself (single-project setups). For multi-project
	// repos this is the project name under /workspace/projects/.
	Project string `json:"project,omitempty"`

	// ImageTag is the tag suffix for the built image. The full ref is
	// "deploy-<slug>:<image_tag>".
	ImageTag string `json:"image_tag"`
}

// DeployBuildResponse echoes the resulting image ref.
type DeployBuildResponse struct {
	ImageRef string `json:"image_ref"`
}

// docker returns the daemon client when the backend supports it
// (today: only DockerBackend). Used by the deploy build + run handlers
// for ImageBuild / ContainerCreate operations the generic Backend
// interface doesn't model.
func (p *Provisioner) docker() (*client.Client, error) {
	if d, ok := p.backend.(*DockerBackend); ok {
		return d.Client(), nil
	}
	return nil, fmt.Errorf("deploy build requires docker backend (got %T)", p.backend)
}

// buildKitContainerName is the name we use for the moby/buildkit
// container the provisioner manages. Single shared instance per
// cluster; railpack talks to it via BUILDKIT_HOST.
const buildKitContainerName = "agentry-buildkit"

// buildKitImage pins the BuildKit version. Track-with-buildkit-release
// when you bump.
const buildKitImage = "moby/buildkit:v0.16.0"

// Guard so concurrent deploy-builds don't race the lazy ensureBuildKit.
var buildKitMu sync.Mutex

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

	// Step 0: BuildKit container must be up. Lazy ensure on first build.
	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker client unavailable: "+err.Error())
		return
	}
	if err := ensureBuildKit(ctx, dockerCli); err != nil {
		writeError(w, http.StatusServiceUnavailable, "buildkit bootstrap: "+err.Error())
		return
	}

	// Step 1: tar the project inside the sandbox.
	tarPathSandbox := fmt.Sprintf("/tmp/build-ctx-%s.tar.gz", req.ImageTag)
	if _, err := p.runtimeShellExec(ctx, sandboxID,
		fmt.Sprintf("rm -f %s && tar czf %s -C %s . 2>&1",
			tarPathSandbox, tarPathSandbox, projectPath)); err != nil {
		writeError(w, http.StatusBadGateway, "tar build context: "+err.Error())
		return
	}

	// Step 2: download to provisioner-side dir, untar.
	ctxDir := filepath.Join("/tmp", "buildctx-"+req.ImageTag)
	if err := os.RemoveAll(ctxDir); err != nil {
		writeError(w, http.StatusInternalServerError, "cleanup: "+err.Error())
		return
	}
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "mkdir: "+err.Error())
		return
	}
	defer func() {
		_ = os.RemoveAll(ctxDir)
		_, _ = p.runtimeShellExec(context.Background(), sandboxID, "rm -f "+tarPathSandbox)
	}()

	stream, err := p.runtimeFileDownload(ctx, sandboxID, tarPathSandbox)
	if err != nil {
		writeError(w, http.StatusBadGateway, "download build context: "+err.Error())
		return
	}
	tarCmd := exec.CommandContext(ctx, "tar", "-xzf", "-", "-C", ctxDir)
	tarCmd.Stdin = stream
	tarOut, tarErr := tarCmd.CombinedOutput()
	stream.Close()
	if tarErr != nil {
		writeError(w, http.StatusBadGateway,
			fmt.Sprintf("untar build context: %v\n%s", tarErr, string(tarOut)))
		return
	}

	// Step 3: railpack build. Image lands in the local docker daemon
	// via BuildKit's docker exporter.
	cmd := exec.CommandContext(ctx, "railpack", "build", ctxDir,
		"--name", imageRef,
		"--progress", "plain")
	cmd.Env = append(os.Environ(),
		"BUILDKIT_HOST=docker-container://"+buildKitContainerName,
	)
	out, runErr := cmd.CombinedOutput()
	tail := tailLog(string(out), 4*1024)
	if runErr != nil {
		writeError(w, http.StatusBadGateway,
			"railpack build failed: "+runErr.Error()+"\n\n"+tail)
		return
	}

	// Step 4: verify the image actually landed in the daemon. railpack
	// can return 0 from a partial run in some edge cases; double-check
	// rather than trust the exit code alone.
	imgs, err := dockerCli.ImageList(ctx, image.ListOptions{})
	if err == nil {
		found := false
		for _, im := range imgs {
			for _, tag := range im.RepoTags {
				if tag == imageRef {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			writeError(w, http.StatusBadGateway,
				"railpack reported success but image not in daemon:\n"+tail)
			return
		}
	}

	writeJSON(w, http.StatusOK, DeployBuildResponse{ImageRef: imageRef})
}

// ensureBuildKit makes sure the BuildKit container is running. Lazy +
// idempotent: first deploy-build creates it; subsequent ones reuse it.
// Restart-aware: if the container exists but is stopped (provisioner
// reboot), we Start() it.
func ensureBuildKit(ctx context.Context, cli *client.Client) error {
	buildKitMu.Lock()
	defer buildKitMu.Unlock()

	info, err := cli.ContainerInspect(ctx, buildKitContainerName)
	if err == nil {
		if info.State != nil && info.State.Running {
			return nil
		}
		// Exists but stopped → start it.
		if err := cli.ContainerStart(ctx, buildKitContainerName, container.StartOptions{}); err != nil {
			return fmt.Errorf("start existing buildkit: %w", err)
		}
		return nil
	}
	// Doesn't exist → pull image, then create + start.
	//
	// ImagePull returns a ReadCloser of JSON progress events; the pull
	// only actually completes once that stream is fully drained.
	// Skipping the drain → ContainerCreate trips "No such image".
	pullStream, err := cli.ImagePull(ctx, buildKitImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull buildkit image: %w", err)
	}
	_, _ = io.Copy(io.Discard, pullStream)
	pullStream.Close()
	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: buildKitImage},
		&container.HostConfig{
			Privileged:    true,
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		},
		nil, nil, buildKitContainerName)
	if err != nil {
		return fmt.Errorf("create buildkit: %w", err)
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start buildkit: %w", err)
	}
	return nil
}

// runtimeFileDownload pulls a file from the sandbox via the runtime's
// streaming download endpoint.
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

// tailLog returns the last `max` bytes of s for inclusion in error
// responses; trims to the start of a line so we don't show a half-line.
func tailLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	tail := s[len(s)-max:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return tail
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
// runs and trims trailing punctuation.
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
// allowed character set.
func sanitizeTag(s string) string {
	return sanitizeForDockerTag(s)
}
