package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/errcode"
)

// BuildManifest is the deployment-time descriptor — what cluster
// services, what secrets, what ports the deployed image needs. The
// image carries an /xdp.json copy of this so the deploy API can
// validate the bindings against the target cluster's catalog before
// scheduling the pod.
type BuildManifest struct {
	APIVersion string   `json:"api_version"` // "agentry.run/v1alpha1"
	Name       string   `json:"name"`        // app name
	Image      string   `json:"image"`       // resolved image ref (tag + digest)
	Services   []string `json:"services,omitempty"`
	Secrets    []string `json:"secrets,omitempty"`
	Ports      []int    `json:"ports,omitempty"`
	BuiltAt    string   `json:"built_at"`
}

// BuildRequest is the body for POST /api/sandboxes/{id}/build.
type BuildRequest struct {
	Tag     string `json:"tag,omitempty"`     // image tag (default: ad-sandbox-app:<sandbox-id>-<ts>)
	Project string `json:"project,omitempty"` // project name when multiple; default = single project in /workspace/projects/
}

// BuildResponse carries the artifacts of a successful build. The
// caller (deploy) reads BuildManifest verbatim and posts it to the
// deploy API.
type BuildResponse struct {
	Image      string        `json:"image"`
	Manifest   BuildManifest `json:"manifest"`
	Dockerfile string        `json:"dockerfile"`
}

// handleBuild generates the image artifacts for this sandbox. For
// v1 it writes the manifest + Dockerfile into /workspace/.build/
// inside the sandbox and returns the resolved tag. A future phase
// shells out to buildah, pushes the image, and records the digest.
//
// For now the image tag is opaque — deploy uses it as an identifier;
// the real build happens once the stub XDP integration becomes a
// real one.
func (p *Provisioner) handleBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInvalidRequest, "sandbox id missing in path"))
		return
	}
	var req BuildRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errcode.WriteJSON(w, errcode.New(errcode.InvalidRequest, "bad request body: %v", err))
			return
		}
	}

	// Read the lockfile — that's the source of truth for what
	// services / secrets the manifest needs to declare. Without
	// bindings recorded, build still succeeds but the manifest's
	// services/secrets lists are empty.
	lock, err := p.readLockfile(r.Context(), id)
	if err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInternal, "read lockfile: %v", err))
		return
	}

	tag := req.Tag
	if tag == "" {
		tag = fmt.Sprintf("ad-sandbox-app:%s-%d", id, time.Now().Unix())
	}

	manifest := BuildManifest{
		APIVersion: "agentry.run/v1alpha1",
		Name:       id, // sandbox id doubles as app name for v1
		Image:      tag,
		BuiltAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if lock != nil {
		for _, b := range lock.Bindings {
			manifest.Services = append(manifest.Services, b.Service)
		}
		manifest.Secrets = append(manifest.Secrets, lock.Secrets...)
	}

	// Generate a Dockerfile. v1 picks a base image based on what's
	// in /workspace/projects/ — Python if requirements.txt is there,
	// Node if package.json, plain bash otherwise. The xdp-entrypoint
	// shim is inlined so apps don't need a separate base-image
	// dependency.
	df, err := p.generateDockerfile(r.Context(), id, req.Project)
	if err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInternal, "generate dockerfile: %v", err))
		return
	}

	// Persist artifacts inside the sandbox so the user can inspect.
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	if err := p.runtimeFileWrite(r.Context(), id, "/workspace/.build/xdp.json", append(manifestJSON, '\n')); err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInternal, "write manifest: %v", err))
		return
	}
	if err := p.runtimeFileWrite(r.Context(), id, "/workspace/.build/Dockerfile", []byte(df)); err != nil {
		errcode.WriteJSON(w, errcode.New(errcode.SandboxInternal, "write dockerfile: %v", err))
		return
	}

	writeJSON(w, 200, BuildResponse{
		Image:      tag,
		Manifest:   manifest,
		Dockerfile: df,
	})
}

// generateDockerfile produces a Dockerfile string for the sandbox's
// project tree. For v1 it's intentionally one-size-fits-most:
// detect language by hint files, copy /workspace/projects/{name}
// into /app, install deps, set xdp-entrypoint as the ENTRYPOINT.
func (p *Provisioner) generateDockerfile(ctx context.Context, sandboxID, project string) (string, error) {
	// Detect project layout. v1 expects exactly one project under
	// /workspace/projects/ unless `project` is specified.
	projects, err := p.listProjects(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "", fmt.Errorf("no projects found under /workspace/projects/ — scaffold one before building")
	}
	if project == "" {
		if len(projects) > 1 {
			return "", fmt.Errorf("multiple projects (%v); pass project=NAME to choose", projects)
		}
		project = projects[0]
	} else {
		found := false
		for _, p := range projects {
			if p == project {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("project %q not in %v", project, projects)
		}
	}

	// Detect language. Best-effort by hint file; falls back to bash.
	base, install, startCmd := p.detectLanguage(ctx, sandboxID, project)

	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by ad-sandbox build — sandbox=%s project=%s\n", sandboxID, project)
	fmt.Fprintf(&b, "FROM %s\n", base)
	b.WriteString("WORKDIR /app\n")
	fmt.Fprintf(&b, "COPY projects/%s/ /app/\n", project)
	if install != "" {
		fmt.Fprintf(&b, "RUN %s\n", install)
	}
	b.WriteString("\n# xdp-entrypoint: sources /var/run/agentry/<service>/<env-var> files,\n")
	b.WriteString("# exports them as env, then exec's the user's command. This is what\n")
	b.WriteString("# makes the env contract identical between sandbox-dev and deployed pod.\n")
	b.WriteString("COPY xdp-entrypoint /usr/local/bin/xdp-entrypoint\n")
	b.WriteString("RUN chmod +x /usr/local/bin/xdp-entrypoint\n")
	b.WriteString("ENTRYPOINT [\"/usr/local/bin/xdp-entrypoint\"]\n")
	fmt.Fprintf(&b, "CMD %s\n", startCmd)

	return b.String(), nil
}

// listProjects returns names of directories under /workspace/projects/
// that contain a .sandbox-project.json. Driven via the runtime's
// shell-exec endpoint since we don't have a file_list helper here.
func (p *Provisioner) listProjects(ctx context.Context, sandboxID string) ([]string, error) {
	out, err := p.runtimeShellExec(ctx, sandboxID,
		"ls /workspace/projects/ 2>/dev/null | while read d; do [ -f /workspace/projects/$d/.sandbox-project.json ] && echo $d; done")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// detectLanguage picks a base image, install command, and start
// command based on hint files in the project directory.
func (p *Provisioner) detectLanguage(ctx context.Context, sandboxID, project string) (base, install, startCmd string) {
	dir := "/workspace/projects/" + project

	if out, _ := p.runtimeShellExec(ctx, sandboxID, "test -f "+dir+"/requirements.txt && echo yes"); strings.TrimSpace(out) == "yes" {
		return "python:3.12-slim",
			"pip install --no-cache-dir -r requirements.txt",
			`["python3", "main.py"]`
	}
	if out, _ := p.runtimeShellExec(ctx, sandboxID, "test -f "+dir+"/package.json && echo yes"); strings.TrimSpace(out) == "yes" {
		return "node:22-slim",
			"npm install --omit=dev",
			`["node", "index.js"]`
	}
	return "debian:stable-slim", "", `["bash", "main.sh"]`
}

// runtimeShellExec is the shell counterpart to runtimeFileWrite — runs
// a command in the sandbox and returns stdout. Used by build for
// project detection; future build phases will use it to drive the
// real buildah invocation.
func (p *Provisioner) runtimeShellExec(ctx context.Context, sandboxID, command string) (string, error) {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return "", fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	body, _ := json.Marshal(map[string]any{"command": command})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/shell/exec",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("shell exec status %d", resp.StatusCode)
	}
	var wrap struct {
		Data struct {
			Output string `json:"output"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return "", err
	}
	return wrap.Data.Output, nil
}
