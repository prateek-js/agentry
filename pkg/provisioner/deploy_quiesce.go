package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
)

// Pause/resume the sandbox's long-running projects around a deploy
// build. Without this, `next dev` (preview) and the preflight's
// `next build` race over /workspace/projects/<name>/.next/ and produce
// nonsensical "Cannot find module './chunks/vendor-chunks/<x>.js'"
// failures the LLM can't diagnose. Pausing the dev process for the
// ~30 s the build takes turns a flaky deploy into a clean one.
//
// Shares routing at the same port get the same treatment for free: the
// bridge forwards their requests to the now-not-listening port, the
// runtime returns 502, and the share URL serves a brief error window.
// When we resume the project, shares start working again — no share-
// table touch needed.

// runtimeProject is the trimmed runtime ProjectStatus the provisioner
// needs to make pause/resume decisions. Kept minimal so we don't bind
// to fields that don't matter here (logs, last error, etc.).
type runtimeProject struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// runtimeProjectList fetches the live project status from the sandbox.
// Returns nil + nil error when the runtime has no projects registered.
func (p *Provisioner) runtimeProjectList(ctx context.Context, sandboxID string) ([]runtimeProject, error) {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return nil, fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/project/list", nil)
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("project list status %d: %s", resp.StatusCode, raw)
	}
	// Runtime wraps lists as {"data": [...]}. Tolerate both shapes
	// since handlers.Success picks the key based on type.
	var wrap struct {
		Data []runtimeProject `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// runtimeProjectStop hits POST /v1/project/stop. The runtime's
// stopProject sets manuallyStopped=true so the auto-restart loop
// stays out of our way until resumeProjects calls start.
func (p *Provisioner) runtimeProjectStop(ctx context.Context, sandboxID, name string) error {
	return p.runtimeProjectAction(ctx, sandboxID, "stop", name)
}

// runtimeProjectStart hits POST /v1/project/start. Creates a fresh
// Project struct in the runtime (manuallyStopped resets implicitly).
func (p *Provisioner) runtimeProjectStart(ctx context.Context, sandboxID, name string) error {
	return p.runtimeProjectAction(ctx, sandboxID, "start", name)
}

// runtimeProjectAction is the shared POST path for stop/start: same
// body shape, same auth, same status handling.
func (p *Provisioner) runtimeProjectAction(ctx context.Context, sandboxID, verb, name string) error {
	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || port == 0 {
		return fmt.Errorf("sandbox %q not found", sandboxID)
	}
	base := fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
	body, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		base+"/v1/project/"+verb, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if p.config.RuntimeAPIKey != "" {
		req.Header.Set("X-Sandbox-API-Key", p.config.RuntimeAPIKey)
	}
	resp, err := sandboxHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("project %s status %d: %s", verb, resp.StatusCode, raw)
	}
	return nil
}

// pauseProjectsAt stops every running project whose name matches the
// build's target dir, so the preflight build can own .next/ uncontested.
// Returns the list of paused names; the caller defers resumeProjects.
//
// Matching policy: project name == basename(projectPath). That mirrors
// the runtime's ProjectManager convention (projects live at
// /workspace/projects/<name>/). For the rare single-project setup
// where projectPath == /workspace, we pause every running project —
// safer than guessing which one binds .next/.
//
// Network errors here are non-fatal: we log and return what we
// managed to pause. A bad runtime state is going to surface as a real
// failure in the build that follows; we shouldn't bail the deploy on
// a flaky /v1/project/list.
func (p *Provisioner) pauseProjectsAt(ctx context.Context, sandboxID, projectPath string) []string {
	projs, err := p.runtimeProjectList(ctx, sandboxID)
	if err != nil {
		log.Printf("deploy-pause: sandbox=%s list projects: %v (continuing without pause)", sandboxID, err)
		return nil
	}
	target := filepath.Base(projectPath)
	rootBuild := projectPath == "/workspace"

	var paused []string
	for _, proj := range projs {
		if proj.Status != "running" {
			continue
		}
		if !rootBuild && proj.Name != target {
			continue
		}
		if err := p.runtimeProjectStop(ctx, sandboxID, proj.Name); err != nil {
			log.Printf("deploy-pause: sandbox=%s stop %q: %v", sandboxID, proj.Name, err)
			continue
		}
		paused = append(paused, proj.Name)
	}
	if len(paused) > 0 {
		log.Printf("deploy-pause: sandbox=%s paused %v for the build window", sandboxID, paused)
	}
	return paused
}

// resumeProjects starts each named project back up. Uses Background
// context on purpose — the calling request may be cancelled (client
// disconnected, deploy timed out) and we still want the preview
// restored. Failures are logged but not surfaced; a stuck restart
// shouldn't block the deploy's return path.
func (p *Provisioner) resumeProjects(sandboxID string, names []string) {
	if len(names) == 0 {
		return
	}
	ctx := context.Background()
	for _, n := range names {
		if err := p.runtimeProjectStart(ctx, sandboxID, n); err != nil {
			log.Printf("deploy-resume: sandbox=%s start %q: %v (user may need to restart manually)", sandboxID, n, err)
			continue
		}
		log.Printf("deploy-resume: sandbox=%s restarted %q", sandboxID, n)
	}
}
