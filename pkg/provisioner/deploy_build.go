package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// splitNonEmpty splits s on sep, trims each piece, and drops empties.
// Tiny helper for parsing whitespace-separated shell output.
func splitNonEmpty(s, sep string) []string {
	out := make([]string, 0, 4)
	for _, p := range strings.Split(s, sep) {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

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

	// BuildEnv is the env stamped during `npm run build` / equivalent.
	// Next.js (and any framework that reads env at module load time)
	// crashes the build without these — even though the same values
	// live as runtime container env. We pass each as `--env K=V` to
	// railpack. These values DO end up baked into image layers when
	// referenced during the build; the image stays on the user's own
	// docker daemon and is never pushed to a public registry, so the
	// blast radius is contained to the user's own infra.
	BuildEnv map[string]string `json:"build_env,omitempty"`
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

	ctx := r.Context()

	// Resolve an empty/unset project at the front gate.
	//
	// Agentry sandboxes scaffold user code under /workspace/projects/<name>/,
	// not at /workspace itself. With the dashboard's one-project-per-
	// sandbox model the form now always sends project="" — letting that
	// flow through to projectPathAndSlug("") would aim railpack at
	// /workspace, which contains .bashrc/.profile/projects/ and detects
	// as no stack. Auto-resolve to the sole project on disk before the
	// existence gate so the legacy "real /workspace project" layout
	// (rare but supported) keeps working untouched.
	requestedProject := req.Project
	if rp := strings.TrimSpace(requestedProject); rp == "" || rp == "." {
		if sole := p.detectSoleProject(ctx, sandboxID); sole != "" {
			log.Printf("deploy-build: sandbox=%s project unset; auto-using sole project %q under /workspace/projects/",
				sandboxID, sole)
			requestedProject = sole
		}
	}

	projectPath, projectSlug, projErr := projectPathAndSlug(requestedProject)
	if projErr != nil {
		writeError(w, http.StatusBadRequest, projErr.Error())
		return
	}
	imageRef := fmt.Sprintf("deploy-%s:%s", projectSlug, sanitizeTag(req.ImageTag))

	log.Printf("deploy-build: sandbox=%s project=%q image_tag=%s — start", sandboxID, requestedProject, req.ImageTag)

	// Step 0a: validate the project actually has source.
	//
	// Two failure shapes share this gate:
	//   1. The LLM typed "pocket-expense-tracker" but the scaffold
	//      lives in /workspace/projects/app/ — typo / guess.
	//   2. The LLM fired `deploy` a few seconds before its scaffold
	//      finished writing files, or mid-restructure (rm before mv).
	//
	// (1) is fixed by auto-resolving when exactly one project is on
	// disk. (2) is fixed by polling — if neither the requested project
	// NOR any candidate exists yet, wait up to 30 s before giving up.
	// The LLM rarely retries on a hard 400, so eating a few seconds
	// of wall-clock here turns flaky deploys into successful ones at
	// no cost when the project genuinely doesn't exist.
	checkProject := func() (string, []string) {
		exists, _ := p.runtimeShellExec(ctx, sandboxID,
			fmt.Sprintf("test -d %s && [ -n \"$(ls -A %s 2>/dev/null)\" ] && echo yes", projectPath, projectPath))
		if strings.TrimSpace(exists) == "yes" {
			return "yes", nil
		}
		subdirs, _ := p.runtimeShellExec(ctx, sandboxID,
			"ls -d /workspace/projects/*/ 2>/dev/null | xargs -n1 basename 2>/dev/null | head -10")
		return "", splitNonEmpty(strings.TrimSpace(subdirs), "\n")
	}

	exists, candidates := checkProject()
	const settleBudget = 30 * time.Second
	deadline := time.Now().Add(settleBudget)
	waited := false
	for exists != "yes" && len(candidates) != 1 && time.Now().Before(deadline) {
		// Either nothing on disk yet, or two+ candidates with the
		// requested name missing — the latter we can't auto-resolve
		// anyway, so don't sit on it. Only sleep when truly empty.
		if len(candidates) > 1 {
			break
		}
		if !waited {
			log.Printf("deploy-build: sandbox=%s project=%q not on disk; "+
				"waiting up to %s for the LLM to finish scaffolding…",
				sandboxID, req.Project, settleBudget)
			waited = true
		}
		select {
		case <-ctx.Done():
			writeError(w, http.StatusServiceUnavailable, "client disconnected while waiting for project: "+ctx.Err().Error())
			return
		case <-time.After(2 * time.Second):
		}
		exists, candidates = checkProject()
	}

	switch {
	case exists == "yes":
		// Path-as-requested exists with content — proceed as-is.
	case len(candidates) == 1:
		// Auto-resolve: only one project on disk, and the caller asked
		// for a missing name. Treat it as a typo / guess and use the
		// real project.
		log.Printf("deploy-build: sandbox=%s requested project %q missing; "+
			"using sole project on disk (%q)", sandboxID, req.Project, candidates[0])
		// candidates[0] comes from `ls | xargs basename`, so it's
		// a single path component (already safe), but route through
		// the same validator anyway — defense in depth.
		var autoErr error
		projectPath, projectSlug, autoErr = projectPathAndSlug(candidates[0])
		if autoErr != nil {
			writeError(w, http.StatusInternalServerError,
				"auto-resolved project failed validation: "+autoErr.Error())
			return
		}
		imageRef = fmt.Sprintf("deploy-%s:%s", projectSlug, sanitizeTag(req.ImageTag))
	default:
		msg := fmt.Sprintf("no source files at %s", projectPath)
		switch {
		case len(candidates) > 1:
			msg += fmt.Sprintf(" — pick one of: %s (pass via the Project field)",
				strings.Join(candidates, ", "))
		case len(candidates) == 0 && waited:
			msg += fmt.Sprintf(" (and /workspace/projects/ stayed empty for %s — the LLM never scaffolded the app)", settleBudget)
		case len(candidates) == 0:
			msg += " (and /workspace/projects/ is empty — the LLM may not have scaffolded the app yet)"
		}
		log.Printf("deploy-build: sandbox=%s FAILED at project-check: %s", sandboxID, msg)
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Step 0c: pause the dev server in the build path, blow away half-
	// written build artifacts, then run the preflight build.
	//
	// Why pause: `next dev` (preview) and `next build` (preflight) both
	// write to /workspace/projects/<name>/.next/. Concurrent writes
	// produce "Cannot find module './chunks/vendor-chunks/<x>.js'"
	// failures the LLM can't fix — the file isn't missing in source,
	// it's just been clobbered mid-write. Pausing the dev process for
	// the build window is the only durable fix.
	//
	// Why the rm -rf: a dev server crash mid-write can leave .next in
	// a half-baked state that even `next build` from scratch trips on
	// (page manifests pointing at chunks that don't exist). Cheapest
	// way to guarantee a clean slate is to nuke the build output dirs
	// across the common frameworks before we start.
	//
	// Resume on defer with a Background context: the request ctx may
	// be cancelled by the time we return (client disconnect on slow
	// builds), and we still want the preview back. Shares pointing at
	// the same port come back to life along with the dev server — no
	// share-table touch needed.
	pausedProjects := p.pauseProjectsAt(ctx, sandboxID, projectPath)
	defer p.resumeProjects(sandboxID, pausedProjects)

	_, _ = p.runtimeShellExec(ctx, sandboxID, fmt.Sprintf(
		"rm -rf %s/.next %s/.turbo %s/dist %s/.svelte-kit %s/.output 2>/dev/null || true",
		projectPath, projectPath, projectPath, projectPath, projectPath))

	// Why preflight inside the sandbox: "next build" / "vite build" do
	// full type-checking; "next dev" / "vite dev" don't. Without this,
	// every TS error the dev server tolerates surfaces only at railpack
	// time (45-60 s into the deploy), wrapped in a buildkit error
	// frame. Catching it here means the user gets the compiler's own
	// error ~10-20 s after clicking Deploy.
	//
	// Skipped when: no package.json, or no "build" script in it.
	// (Python projects, Go projects, single-page projects without a
	// build step etc. just proceed straight to railpack.)
	if out, ranIt, perr := p.preflightBuild(ctx, sandboxID, projectPath, req.BuildEnv); perr != nil {
		log.Printf("deploy-build: sandbox=%s preflight FAILED: %v", sandboxID, perr)
		writeError(w, http.StatusBadRequest,
			"preflight build failed in the sandbox before reaching railpack:\n\n"+out)
		return
	} else if ranIt {
		log.Printf("deploy-build: sandbox=%s preflight build OK", sandboxID)
	}

	// Step 0b: BuildKit container must be up. Lazy ensure on first build.
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
		log.Printf("deploy-build: sandbox=%s FAILED at tar: %v", sandboxID, err)
		writeError(w, http.StatusBadGateway, "tar build context: "+err.Error())
		return
	}
	log.Printf("deploy-build: sandbox=%s tar of %s ok — starting railpack", sandboxID, projectPath)

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
	railpackArgs := []string{"build", ctxDir, "--name", imageRef, "--progress", "plain"}
	// Forward each build-time env var. Next.js + many other stacks
	// read env at module-load time, so `next build` reads them during
	// page-data collection; without these the build crashes on
	// "MONGODB_URL env var is not set" even though the runtime has them.
	// Sort keys so build cache hits are deterministic across runs.
	bkeys := make([]string, 0, len(req.BuildEnv))
	for k := range req.BuildEnv {
		bkeys = append(bkeys, k)
	}
	sort.Strings(bkeys)
	for _, k := range bkeys {
		railpackArgs = append(railpackArgs, "--env", k+"="+req.BuildEnv[k])
	}
	cmd := exec.CommandContext(ctx, "railpack", railpackArgs...)
	cmd.Env = append(os.Environ(),
		"BUILDKIT_HOST=docker-container://"+buildKitContainerName,
	)
	out, runErr := cmd.CombinedOutput()
	tail := tailLog(string(out), 4*1024)
	if runErr != nil {
		log.Printf("deploy-build: sandbox=%s FAILED at railpack: %v", sandboxID, runErr)
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
			log.Printf("deploy-build: sandbox=%s FAILED — railpack returned 0 but image %s not in daemon", sandboxID, imageRef)
			writeError(w, http.StatusBadGateway,
				"railpack reported success but image not in daemon:\n"+tail)
			return
		}
	}

	log.Printf("deploy-build: sandbox=%s OK — image=%s", sandboxID, imageRef)
	writeJSON(w, http.StatusOK, DeployBuildResponse{ImageRef: imageRef})
}

// preflightBuild runs the project's build command inside the sandbox
// before we ship the tar to railpack. Returns (output, ranBuild, err):
//
//   - ranBuild=false, err=nil → not a Node project (no package.json) or
//     no "build" script in package.json; nothing to check. Caller
//     proceeds to railpack as usual.
//   - ranBuild=true,  err=nil → build succeeded; safe to ship.
//   - ranBuild=true,  err!=nil → build failed; output holds the
//     compiler error verbatim. Caller fails the deploy with output
//     surfaced to the user.
//
// Build env vars are exported into the build shell so frameworks that
// read env at module-load time (Next.js, …) get the same values that
// railpack would later inject. Output is capped at 8 KB tail so we
// don't blow up the deployment row's status_msg column.
func (p *Provisioner) preflightBuild(ctx context.Context, sandboxID, projectPath string, buildEnv map[string]string) (string, bool, error) {
	// 1. Is this a Node project with a build script? If not, skip.
	probe := fmt.Sprintf(
		"[ -f %s/package.json ] && "+
			"node -e \"process.exit((require('%s/package.json').scripts||{}).build?0:2)\" "+
			"&& echo yes",
		projectPath, projectPath)
	out, _, _ := p.runtimeShellExecExit(ctx, sandboxID, probe, 30)
	if strings.TrimSpace(out) != "yes" {
		// Not Node / no build script — railpack still runs the
		// language's native build step for non-Node stacks.
		return "", false, nil
	}

	// 2. Build with the deployment's env exposed. Cap at 5 min — a
	// healthy Next.js build is 20-60 s; anything past 5 min is a
	// real problem (memory pressure, infinite loop in postinstall)
	// and we'd rather surface "stuck" than wait forever.
	envExports := buildEnvExportLine(buildEnv)
	cmd := fmt.Sprintf("cd %s && %s npm run build 2>&1", projectPath, envExports)
	out, exit, err := p.runtimeShellExecExit(ctx, sandboxID, cmd, 300)
	if err != nil {
		return tailLog(out, 8*1024), true, fmt.Errorf("shell error: %w", err)
	}
	if exit != 0 {
		return tailLog(out, 8*1024), true, fmt.Errorf("npm run build exited %d", exit)
	}
	return "", true, nil
}

// buildEnvExportLine renders a map of build env vars into a single
// "VAR=val VAR2=val2" prefix for a shell command. Keys are sorted so
// the rendered command is deterministic (helps with diff-able logs).
// Values are single-quote-wrapped with embedded quotes escaped — same
// shape sh expects from a one-liner.
func buildEnvExportLine(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("='")
		b.WriteString(strings.ReplaceAll(env[k], "'", "'\\''"))
		b.WriteString("' ")
	}
	return b.String()
}

// runtimeShellExecExit is like runtimeShellExec but also returns the
// command's exit code (which the runtime ships in the response body).
// Used by preflightBuild where "stdout came back but exit != 0" is the
// distinguishing case for "build failed" vs "build succeeded".
func (p *Provisioner) runtimeShellExecExit(ctx context.Context, sandboxID, command string, timeoutSec int) (string, int, error) {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return "", -1, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	body, _ := json.Marshal(map[string]any{"command": command, "timeout": timeoutSec})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/shell/exec",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", -1, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return string(raw), -1, fmt.Errorf("shell exec status %d", resp.StatusCode)
	}
	var wrap struct {
		Data struct {
			Output   string `json:"output"`
			ExitCode int    `json:"exit_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return "", -1, err
	}
	return wrap.Data.Output, wrap.Data.ExitCode, nil
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

// detectSoleProject returns the single project name under
// /workspace/projects/ when exactly one exists, otherwise "". Used by
// handleDeployBuild when the caller didn't pick a project — the
// dashboard's deploy form is one-project-per-sandbox so this is the
// hot path now.
//
// We don't recurse into project dirs and we tolerate the directory
// being absent (legacy sandboxes with code at /workspace itself).
// Returning "" leaves the caller's empty-project semantics ("build
// /workspace") intact, preserving the only other supported layout.
func (p *Provisioner) detectSoleProject(ctx context.Context, sandboxID string) string {
	out, _ := p.runtimeShellExec(ctx, sandboxID,
		"ls -d /workspace/projects/*/ 2>/dev/null | xargs -n1 basename 2>/dev/null | head -10")
	candidates := splitNonEmpty(strings.TrimSpace(out), "\n")
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

// projectPathAndSlug normalizes a user-supplied project name into the
// in-sandbox path + a DNS-safe slug for the image tag.
//
//	""           → /workspace,                 slug "workspace"
//	"."          → /workspace,                 slug "workspace"
//	"sales"      → /workspace/projects/sales,  slug "sales"
//	"web/admin"  → /workspace/projects/web/admin, slug "web-admin"
//
// The returned path is the literal string interpolated into shell
// commands like `cd %s && npm run build`. Two classes of attack get
// rejected explicitly:
//
//   - Path traversal — "../etc" would resolve outside /workspace.
//     filepath.Clean normalises ".." segments; the prefix check
//     then rejects anything that escaped.
//   - Command injection — semicolons, backticks, $(), redirects,
//     spaces — would inject into the surrounding shell command
//     regardless of where the resolved path lands. Rejected before
//     interpolation so the path never reaches the shell.
//
// Returns a non-nil error on bad input; both path and slug are empty
// in that case. Callers MUST check err — silently building with an
// empty path runs the command in the provisioner's CWD, which is not
// the sandbox at all.
func projectPathAndSlug(project string) (path, slug string, err error) {
	project = strings.Trim(project, "/")
	if project == "" || project == "." {
		return "/workspace", "workspace", nil
	}
	// Shell metacharacter rejection. Even if the resolved path is
	// safe, characters with meaning in `sh -c` would inject — the
	// project name flows through `cd %s && …` and similar.
	if strings.ContainsAny(project, ";|&`$<>(){}[]'\"\\\n\r\t ") {
		return "", "", fmt.Errorf("project name %q contains unsafe characters", project)
	}
	// Path-traversal rejection. filepath.Clean normalises ".."
	// segments; we then verify the result is still under the project
	// root before letting it through.
	raw := "/workspace/projects/" + project
	clean := filepath.Clean(raw)
	if !strings.HasPrefix(clean+"/", "/workspace/projects/") {
		return "", "", fmt.Errorf("project name %q escapes /workspace/projects/", project)
	}
	return clean, sanitizeForDockerTag(project), nil
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

// ── Preflight ──────────────────────────────────────────────────────
//
// preflightBuild stays an internal step of handleDeployBuild — the
// standalone LLM-callable /preflight endpoint was removed because the
// pre-pause/cleanup choreography that makes it work safely only makes
// sense inside the full Deploy flow.
