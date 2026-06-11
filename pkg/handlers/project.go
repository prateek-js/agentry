package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentry/agentry/pkg/models"
	"github.com/agentry/agentry/pkg/shell"
)

const (
	projectConfigFile  = ".sandbox-project.json"
	maxLogLines        = 500
	maxCrashesInWindow = 5
	crashWindowSeconds = 60
)

// Project represents a managed project instance.
type Project struct {
	mu           sync.Mutex
	config       models.ProjectConfig
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	status       string // "running" | "stopped" | "failed" | "starting"
	pid          int
	startedAt    time.Time
	restartCount int
	lastError    string
	logs         []string
	health       string // "healthy" | "unhealthy" | "unknown"

	// manuallyStopped is set by stopProject when an explicit Stop /
	// QuiesceProject call kills the process. watchProcess inspects this
	// when the process exits — if true, auto-restart is suppressed.
	//
	// Without this, an external "please stop the dev server during
	// preflight" intent loses to the 1-16 s auto-restart loop, and the
	// dev server reappears mid-build to fight the preflight over .next/.
	// A fresh StartProject creates a new Project struct so the flag
	// never leaks across lifecycles.
	manuallyStopped bool

	// internalPort is non-zero when the project was wrapped by the
	// authproxy sidecar (m2). It's the port the user's app actually
	// binds to (3001 by default) — INTERNAL to the wrap, never the
	// right answer for a share/deploy target. Status calls strip it
	// from the reported Ports list so anything picking "the port"
	// (LLM, dashboard port picker) gets only the public-facing one.
	internalPort int
}

// ProjectManager manages the lifecycle of all projects.
type ProjectManager struct {
	mu          sync.Mutex
	projects    map[string]*Project
	namedLocks  map[string]*sync.Mutex
	crashStates map[string]*crashState
	workDir     string
}

type crashState struct {
	mu         sync.Mutex
	timestamps []time.Time
}

// NewProjectManager creates the singleton project manager.
func NewProjectManager(workDir string) *ProjectManager {
	if workDir == "" {
		workDir = os.Getenv("WORKSPACE")
		if workDir == "" {
			workDir = "/workspace"
		}
	}
	return &ProjectManager{
		projects:    make(map[string]*Project),
		namedLocks:  make(map[string]*sync.Mutex),
		crashStates: make(map[string]*crashState),
		workDir:     workDir,
	}
}

func (pm *ProjectManager) getNamedLock(name string) *sync.Mutex {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if l, ok := pm.namedLocks[name]; ok {
		return l
	}
	l := &sync.Mutex{}
	pm.namedLocks[name] = l
	return l
}

func (pm *ProjectManager) isCircuitOpen(name string) bool {
	pm.mu.Lock()
	cs, ok := pm.crashStates[name]
	pm.mu.Unlock()
	if !ok {
		return false
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cutoff := time.Now().Add(-time.Duration(crashWindowSeconds) * time.Second)
	count := 0
	for _, t := range cs.timestamps {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= maxCrashesInWindow
}

func (pm *ProjectManager) recordCrash(name string) {
	pm.mu.Lock()
	cs, ok := pm.crashStates[name]
	if !ok {
		cs = &crashState{}
		pm.crashStates[name] = cs
	}
	pm.mu.Unlock()
	cs.mu.Lock()
	cs.timestamps = append(cs.timestamps, time.Now())
	// Keep only recent entries.
	cutoff := time.Now().Add(-time.Duration(crashWindowSeconds) * time.Second)
	filtered := cs.timestamps[:0]
	for _, t := range cs.timestamps {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	cs.timestamps = filtered
	cs.mu.Unlock()
}

// StartProject starts a project from its config.
func (pm *ProjectManager) StartProject(name string) (*Project, error) {
	lock := pm.getNamedLock(name)
	lock.Lock()
	defer lock.Unlock()

	// Check circuit breaker.
	if pm.isCircuitOpen(name) {
		return nil, fmt.Errorf("project '%s' has crashed too many times, circuit breaker open", name)
	}

	// Stop existing if running.
	pm.mu.Lock()
	if existing, ok := pm.projects[name]; ok {
		pm.mu.Unlock()
		existing.mu.Lock()
		if existing.status == "running" {
			existing.mu.Unlock()
			pm.stopProject(existing)
		} else {
			existing.mu.Unlock()
		}
	} else {
		pm.mu.Unlock()
	}

	// Find project directory.
	projectDir := filepath.Join(pm.workDir, "projects", name)
	configPath := filepath.Join(projectDir, projectConfigFile)

	data, err := os.ReadFile(configPath)
	if err != nil {
		// Try workspace root.
		projectDir = pm.workDir
		configPath = filepath.Join(projectDir, projectConfigFile)
		data, err = os.ReadFile(configPath)
		if err != nil {
			// Error doubles as LLM steering: name the expected path
			// and the tool that creates it, so the model's next move
			// is project_create rather than guesswork.
			return nil, fmt.Errorf(
				"project config not found for %q — expected %s (scaffold it with project_create; never write the manifest by hand). Existing projects: ls /workspace/projects/",
				name, filepath.Join(pm.workDir, "projects", name, projectConfigFile))
		}
	}

	var config models.ProjectConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid project config: %v", err)
	}
	config.Name = name

	// Start dependencies first.
	for _, dep := range config.DependsOn {
		pm.mu.Lock()
		depProj, exists := pm.projects[dep]
		pm.mu.Unlock()
		if !exists || depProj.status != "running" {
			if _, err := pm.StartProject(dep); err != nil {
				return nil, fmt.Errorf("dependency '%s' failed to start: %v", dep, err)
			}
		}
	}

	// Build environment. Start from a login-shell env so operator-
	// staged bindings (/etc/profile.d/sandbox-creds.sh → TRINO_URL
	// etc.) reach the project — the runtime daemon's own env was
	// captured BEFORE any binding was applied.
	env := shell.LoginShellEnv(context.Background())
	for k, v := range config.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	if config.EnvFile != "" {
		envFilePath := filepath.Join(projectDir, config.EnvFile)
		if envData, err := os.ReadFile(envFilePath); err == nil {
			for _, line := range strings.Split(string(envData), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
					env = append(env, line)
				}
			}
		}
	}

	if len(config.StartCommand) == 0 {
		return nil, fmt.Errorf("start_command is required")
	}

	// m2: when the operator has enabled auth on this profile,
	// AGENTRY_AUTH_ENABLED=true is stamped into env via the cluster-
	// default env hook. Wrap the user's command with the authproxy
	// sidecar so it sits between the bridge and the user's process.
	// The sidecar listens on PORT (3000), spawns the user command
	// with PORT+1 (3001), and injects HMAC-signed identity headers.
	startCmd, startArgs, env, internalPort := maybeWrapAuthSidecar(config.StartCommand, env)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, startCmd, startArgs...)
	cmd.Dir = projectDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	proj := &Project{
		config:       config,
		cmd:          cmd,
		cancel:       cancel,
		status:       "starting",
		health:       "unknown",
		internalPort: internalPort,
	}

	// Set up log capture.
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start: %v", err)
	}

	proj.pid = cmd.Process.Pid
	proj.startedAt = time.Now()
	proj.status = "running"

	pm.mu.Lock()
	pm.projects[name] = proj
	pm.mu.Unlock()

	// Log capture goroutines.
	go pm.captureOutput(proj, stdout)
	go pm.captureOutput(proj, stderr)

	// Wait for exit in background.
	go pm.watchProcess(proj)

	// Health check uses an explicit port from the config (no more
	// guessing). If the user wants to wait for a port to bind without
	// running a full health check, they can call /v1/ports/wait —
	// that's a separate primitive.
	if config.HealthCheck != nil && config.HealthCheck.Port > 0 {
		go pm.healthCheckLoop(proj)
	}

	return proj, nil
}

func (pm *ProjectManager) captureOutput(proj *Project, pipe interface{ Read([]byte) (int, error) }) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		line := scanner.Text()
		proj.mu.Lock()
		proj.logs = append(proj.logs, line)
		if len(proj.logs) > maxLogLines {
			proj.logs = proj.logs[len(proj.logs)-maxLogLines:]
		}
		proj.mu.Unlock()
	}
}

func (pm *ProjectManager) watchProcess(proj *Project) {
	_ = proj.cmd.Wait()

	proj.mu.Lock()
	name := proj.config.Name
	// Suppress auto-restart when the exit was triggered by an explicit
	// stop — stop should mean stop. Crashes still get auto-restarted as
	// before. Without this gate, callers (the deploy-preflight pause
	// among them) can't reliably keep a project off.
	autoRestart := proj.config.AutoRestart && !proj.manuallyStopped
	proj.status = "stopped"
	if proj.cmd.ProcessState != nil && !proj.cmd.ProcessState.Success() {
		proj.status = "failed"
		proj.lastError = fmt.Sprintf("exit code %d", proj.cmd.ProcessState.ExitCode())
	}
	proj.mu.Unlock()

	pm.recordCrash(name)

	if autoRestart && !pm.isCircuitOpen(name) {
		proj.mu.Lock()
		proj.restartCount++
		proj.mu.Unlock()

		// Exponential backoff: 1s, 2s, 4s, 8s, max 16s.
		delay := time.Duration(1<<min(proj.restartCount-1, 4)) * time.Second
		time.Sleep(delay)

		if _, err := pm.StartProject(name); err != nil {
			proj.mu.Lock()
			proj.status = "failed"
			proj.lastError = fmt.Sprintf("auto-restart failed: %v", err)
			proj.mu.Unlock()
		}
	}
}

func (pm *ProjectManager) healthCheckLoop(proj *Project) {
	hc := proj.config.HealthCheck
	interval := 10
	if hc.Interval > 0 {
		interval = hc.Interval
	}
	timeout := 3
	if hc.Timeout > 0 {
		timeout = hc.Timeout
	}
	retries := 3
	if hc.Retries > 0 {
		retries = hc.Retries
	}

	failures := 0
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		proj.mu.Lock()
		if proj.status != "running" {
			proj.mu.Unlock()
			return
		}
		port := hc.Port
		proj.mu.Unlock()

		url := fmt.Sprintf("http://127.0.0.1:%d%s", port, hc.Path)
		client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
		resp, err := client.Get(url)

		if err != nil || resp.StatusCode >= 500 {
			failures++
			if failures >= retries {
				proj.mu.Lock()
				proj.health = "unhealthy"
				proj.mu.Unlock()
			}
		} else {
			failures = 0
			proj.mu.Lock()
			proj.health = "healthy"
			proj.mu.Unlock()
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
}

func (pm *ProjectManager) stopProject(proj *Project) {
	proj.mu.Lock()
	if proj.cancel != nil {
		proj.cancel()
	}
	// Mark as manually stopped BEFORE the kill so watchProcess sees the
	// flag when the wait returns. Otherwise there's a race: process
	// exits, watchProcess reads autoRestart=true, schedules restart —
	// then we set the flag too late.
	proj.manuallyStopped = true
	pid := proj.pid
	proj.mu.Unlock()

	// Kill process group.
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}

	proj.mu.Lock()
	proj.status = "stopped"
	proj.mu.Unlock()
}

// StopProject stops a project by name.
func (pm *ProjectManager) StopProject(name string) error {
	lock := pm.getNamedLock(name)
	lock.Lock()
	defer lock.Unlock()

	pm.mu.Lock()
	proj, ok := pm.projects[name]
	pm.mu.Unlock()
	if !ok {
		return fmt.Errorf("project '%s' not found", name)
	}
	pm.stopProject(proj)
	return nil
}

// StopAll stops all projects in parallel.
func (pm *ProjectManager) StopAll() {
	pm.mu.Lock()
	names := make([]string, 0, len(pm.projects))
	for name := range pm.projects {
		names = append(names, name)
	}
	pm.mu.Unlock()

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_ = pm.StopProject(n)
		}(name)
	}
	wg.Wait()
}

// ListProjects returns all project statuses.
func (pm *ProjectManager) ListProjects() []models.ProjectStatus {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var result []models.ProjectStatus
	for _, proj := range pm.projects {
		proj.mu.Lock()
		uptime := ""
		var ports []int
		if proj.status == "running" {
			uptime = time.Since(proj.startedAt).Round(time.Second).String()
			ports = filterInternalPort(portsForPGID(proj.pid), proj.internalPort)
		}
		result = append(result, models.ProjectStatus{
			Name:         proj.config.Name,
			Type:         proj.config.Type,
			Status:       proj.status,
			Ports:        ports,
			PID:          proj.pid,
			Uptime:       uptime,
			RestartCount: proj.restartCount,
			Health:       proj.health,
			LastError:    proj.lastError,
		})
		proj.mu.Unlock()
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// GetLogs returns buffered logs for a project.
func (pm *ProjectManager) GetLogs(name string, lines int) ([]string, error) {
	pm.mu.Lock()
	proj, ok := pm.projects[name]
	pm.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("project '%s' not found", name)
	}

	proj.mu.Lock()
	defer proj.mu.Unlock()

	if lines <= 0 || lines > len(proj.logs) {
		lines = len(proj.logs)
	}
	start := len(proj.logs) - lines
	result := make([]string, lines)
	copy(result, proj.logs[start:])
	return result, nil
}

// AutoStartProjects starts all projects with auto_restart=true.
func (pm *ProjectManager) AutoStartProjects() {
	projectsDir := filepath.Join(pm.workDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		configPath := filepath.Join(projectsDir, entry.Name(), projectConfigFile)
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}
		var config models.ProjectConfig
		if err := json.Unmarshal(data, &config); err != nil {
			continue
		}
		if config.AutoRestart {
			go func(name string) {
				if _, err := pm.StartProject(name); err != nil {
					fmt.Fprintf(os.Stderr, "auto-start failed for %s: %v\n", name, err)
				}
			}(entry.Name())
		}
	}
}

// ── HTTP Handlers ─────────────────────────────────────────────────────────

// ProjectStartHandler starts a project.
func ProjectStartHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.ProjectStartRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			Error(w, http.StatusBadRequest, "name is required")
			return
		}
		proj, err := pm.StartProject(req.Name)
		if err != nil {
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		proj.mu.Lock()
		pid := proj.pid
		internalPort := proj.internalPort
		status := models.ProjectStatus{
			Name:   proj.config.Name,
			Type:   proj.config.Type,
			Status: proj.status,
			PID:    pid,
		}
		proj.mu.Unlock()
		if status.Status == "running" {
			status.Ports = filterInternalPort(portsForPGID(pid), internalPort)
		}
		Success(w, fmt.Sprintf("project '%s' started", req.Name), status)
	}
}

// ProjectStopHandler stops a project.
func ProjectStopHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.ProjectStopRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			Error(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := pm.StopProject(req.Name); err != nil {
			Error(w, http.StatusNotFound, err.Error())
			return
		}
		Success(w, fmt.Sprintf("project '%s' stopped", req.Name), nil)
	}
}

// ProjectRestartHandler restarts a project.
func ProjectRestartHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.ProjectStartRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			Error(w, http.StatusBadRequest, "name is required")
			return
		}
		_ = pm.StopProject(req.Name)
		proj, err := pm.StartProject(req.Name)
		if err != nil {
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		proj.mu.Lock()
		pid := proj.pid
		internalPort := proj.internalPort
		status := models.ProjectStatus{
			Name:   proj.config.Name,
			Type:   proj.config.Type,
			Status: proj.status,
			PID:    pid,
		}
		proj.mu.Unlock()
		if status.Status == "running" {
			status.Ports = filterInternalPort(portsForPGID(pid), internalPort)
		}
		Success(w, fmt.Sprintf("project '%s' restarted", req.Name), status)
	}
}

// ProjectStartAllHandler starts all projects respecting dependency order.
func ProjectStartAllHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pm.AutoStartProjects()
		Success(w, "all projects starting", pm.ListProjects())
	}
}

// ProjectStopAllHandler stops all projects in parallel.
func ProjectStopAllHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pm.StopAll()
		Success(w, "all projects stopped", nil)
	}
}

// ProjectListHandler lists all projects.
func ProjectListHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Success(w, "projects listed", pm.ListProjects())
	}
}

// projectCreateScaffold is the materialised template for one kind.
// Keeping the manifest + stub files together as a flat tuple makes the
// switch below readable without sprouting per-kind helper funcs.
type projectCreateScaffold struct {
	config models.ProjectConfig
	files  map[string]string // relpath under projectDir → content
}

// buildProjectScaffold returns the manifest and starter files for one
// of the supported kinds. The starter files exist for two reasons:
//  1. `project_start` would fail on an empty dir for stacks that need
//     a specific entrypoint (streamlit run app.py, etc.).
//  2. Giving the LLM a known path to edit ("public/index.html") is
//     less brittle than "write whatever HTML you want somewhere."
//
// The LLM is expected to overwrite the stub immediately after create.
func buildProjectScaffold(name, kind string, customStart []string, port int) (projectCreateScaffold, error) {
	switch kind {
	case "nextjs":
		return projectCreateScaffold{
			config: models.ProjectConfig{
				Name:         name,
				Type:         "app",
				StartCommand: []string{"npm", "run", "dev"},
				AutoRestart:  true,
			},
			// No stub: app.md drives the actual Next.js scaffold via
			// `npx create-next-app` and then file_write fills it in.
			// project_start only works after that scaffold lands.
			files: nil,
		}, nil

	case "static-html":
		p := port
		if p == 0 {
			p = 8000
		}
		// sh -c + exec: substitutes ${PORT} when authproxy is wrapping
		// us (PORT=3001 shifted from the sidecar's 3000), falls back to
		// the kind's default when running directly. `exec` replaces sh
		// so SIGTERM to the pgid reaches python directly — no orphan.
		return projectCreateScaffold{
			config: models.ProjectConfig{
				Name: name,
				Type: "app",
				StartCommand: []string{"sh", "-c",
					fmt.Sprintf(`exec python3 -m http.server "${PORT:-%d}"`, p)},
				AutoRestart: true,
			},
			files: map[string]string{
				"index.html": staticHTMLStub(name),
			},
		}, nil

	case "streamlit":
		p := port
		if p == 0 {
			p = 8501
		}
		return projectCreateScaffold{
			config: models.ProjectConfig{
				Name: name,
				Type: "app",
				StartCommand: []string{"sh", "-c",
					fmt.Sprintf(`exec streamlit run app.py --server.port "${PORT:-%d}" --server.address 0.0.0.0 --server.headless true`, p),
				},
				AutoRestart: true,
			},
			files: map[string]string{
				"app.py":           streamlitStub(name),
				"requirements.txt": "streamlit\n",
				// Procfile is read by railpack at deploy time to
				// pick the production CMD. $PORT is set by the
				// container runtime — sandbox dev uses port 8501
				// (or 3001 when authproxy is wrapping), production
				// uses agentry's deploy port. Same code, different
				// runtimes.
				"Procfile": "web: streamlit run app.py --server.port $PORT --server.address 0.0.0.0 --server.headless true\n",
			},
		}, nil

	case "fastapi":
		p := port
		if p == 0 {
			p = 8000
		}
		return projectCreateScaffold{
			config: models.ProjectConfig{
				Name: name,
				Type: "app",
				StartCommand: []string{"sh", "-c",
					fmt.Sprintf(`exec uvicorn app:app --host 0.0.0.0 --port "${PORT:-%d}" --reload`, p),
				},
				AutoRestart: true,
			},
			files: map[string]string{
				"app.py":           fastapiStub(name),
				"requirements.txt": "fastapi\nuvicorn[standard]\n",
				// Procfile drives production deploy via railpack.
				// No --reload for production; binds whatever PORT
				// the container runtime sets.
				"Procfile": "web: uvicorn app:app --host 0.0.0.0 --port $PORT\n",
			},
		}, nil

	case "python-script":
		return projectCreateScaffold{
			config: models.ProjectConfig{
				Name:         name,
				Type:         "service",
				StartCommand: []string{"python3", "main.py"},
				AutoRestart:  true,
			},
			files: map[string]string{
				"main.py": pythonScriptStub(name),
			},
		}, nil

	case "custom":
		if len(customStart) == 0 {
			return projectCreateScaffold{}, fmt.Errorf("custom kind requires start_command")
		}
		return projectCreateScaffold{
			config: models.ProjectConfig{
				Name:         name,
				Type:         "service",
				StartCommand: customStart,
				AutoRestart:  true,
			},
			files: nil,
		}, nil

	default:
		return projectCreateScaffold{}, fmt.Errorf("unknown kind %q — supported: nextjs, static-html, streamlit, fastapi, python-script, custom", kind)
	}
}

func staticHTMLStub(name string) string {
	return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>` + name + `</title>
    <style>
      /* placeholder — replace with content from skills/frontend-design + a theme */
      body { font: 16px/1.5 system-ui, sans-serif; margin: 0; padding: 4rem; color: #111; }
    </style>
  </head>
  <body>
    <h1>` + name + `</h1>
    <p>Stub from project_create. Before editing, read /etc/sandbox/docs/skills/frontend-design/SKILL.md and pick a theme.</p>
  </body>
</html>
`
}

func streamlitStub(name string) string {
	return `import streamlit as st

st.set_page_config(page_title="` + name + `", layout="wide")
st.title("` + name + `")
st.caption("Stub from project_create — replace with real content.")
`
}

func fastapiStub(name string) string {
	return `from fastapi import FastAPI

app = FastAPI(title="` + name + `")


@app.get("/")
def root():
    return {"app": "` + name + `", "status": "stub from project_create"}
`
}

func pythonScriptStub(name string) string {
	return `import time

print("[` + name + `] starting (stub from project_create)", flush=True)
while True:
    time.sleep(60)
`
}

// CreateProject materialises a project scaffold under workDir/projects/<name>.
// Returns the manifest path + the list of files written so the handler
// can echo them. Returns an error if the project dir already has a
// manifest — overwriting would silently clobber the LLM's prior work.
func (pm *ProjectManager) CreateProject(req models.ProjectCreateRequest) (models.ProjectCreateData, error) {
	if req.Name == "" {
		return models.ProjectCreateData{}, fmt.Errorf("name is required")
	}
	if strings.ContainsAny(req.Name, "/\\") || req.Name == "." || req.Name == ".." {
		return models.ProjectCreateData{}, fmt.Errorf("name must be a single path segment")
	}
	if req.Kind == "" {
		return models.ProjectCreateData{}, fmt.Errorf("kind is required (nextjs, static-html, streamlit, fastapi, python-script, custom)")
	}

	scaffold, err := buildProjectScaffold(req.Name, req.Kind, req.StartCommand, req.Port)
	if err != nil {
		return models.ProjectCreateData{}, err
	}

	projectDir := filepath.Join(pm.workDir, "projects", req.Name)
	configPath := filepath.Join(projectDir, projectConfigFile)

	if _, err := os.Stat(configPath); err == nil {
		return models.ProjectCreateData{}, fmt.Errorf("project '%s' already exists at %s — pick a different name or delete the existing manifest first", req.Name, projectDir)
	}

	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return models.ProjectCreateData{}, fmt.Errorf("cannot create project dir: %w", err)
	}

	manifest, err := json.MarshalIndent(scaffold.config, "", "  ")
	if err != nil {
		return models.ProjectCreateData{}, err
	}
	if err := os.WriteFile(configPath, append(manifest, '\n'), 0o644); err != nil {
		return models.ProjectCreateData{}, err
	}

	written := []string{configPath}
	for rel, content := range scaffold.files {
		full := filepath.Join(projectDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return models.ProjectCreateData{}, err
		}
		// Don't clobber files that already exist — the LLM may have
		// pre-populated the dir with file_write before reaching this
		// tool. Silent skip; the manifest is still definitive.
		if _, err := os.Stat(full); err == nil {
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return models.ProjectCreateData{}, err
		}
		written = append(written, full)
	}

	nextStep := fmt.Sprintf("Project created. Next: edit files under %s as needed (start with content, NOT styling — read skills/frontend-design/SKILL.md before any CSS), then call project_start with name=%q.", projectDir, req.Name)
	return models.ProjectCreateData{
		Name:         scaffold.config.Name,
		Kind:         req.Kind,
		Path:         projectDir,
		StartCommand: scaffold.config.StartCommand,
		NextStep:     nextStep,
		FilesWritten: written,
	}, nil
}

// RunningPGIDs returns the set of process-group ids for every project
// currently in "running" state. PortsListHandler uses this to classify
// each LISTEN socket as managed (in a project's pgid) or unmanaged.
func (pm *ProjectManager) RunningPGIDs() map[int]struct{} {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make(map[int]struct{}, len(pm.projects))
	for _, proj := range pm.projects {
		proj.mu.Lock()
		if proj.status == "running" && proj.pid > 0 {
			// The project's pgid is its own pid because StartProject
			// always sets Setpgid:true with no parent pgid override
			// (see exec.SysProcAttr in startProject), so the leader
			// itself owns the new process group.
			out[proj.pid] = struct{}{}
		}
		proj.mu.Unlock()
	}
	return out
}

// ProjectCreateHandler scaffolds a project's manifest + starter files.
func ProjectCreateHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.ProjectCreateRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		data, err := pm.CreateProject(req)
		if err != nil {
			// Bad input vs server error: name/kind/conflict are 400.
			if strings.Contains(err.Error(), "required") ||
				strings.Contains(err.Error(), "already exists") ||
				strings.Contains(err.Error(), "must be a single") ||
				strings.Contains(err.Error(), "unknown kind") {
				Error(w, http.StatusBadRequest, err.Error())
				return
			}
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		Success(w, "project created", data)
	}
}

// ProjectLogsHandler returns project logs.
func ProjectLogsHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			Error(w, http.StatusBadRequest, "name query parameter is required")
			return
		}
		lines := 100
		if l := r.URL.Query().Get("lines"); l != "" {
			if v, err := fmt.Sscanf(l, "%d", &lines); err == nil && v > 0 {
				_ = v
			}
		}
		logs, err := pm.GetLogs(name, lines)
		if err != nil {
			Error(w, http.StatusNotFound, err.Error())
			return
		}
		Success(w, "logs", map[string]interface{}{
			"name":  name,
			"lines": logs,
		})
	}
}

// ProjectLogStreamHandler streams logs via SSE.
func ProjectLogStreamHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			Error(w, http.StatusBadRequest, "name query parameter is required")
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			Error(w, http.StatusInternalServerError, "streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		lastLen := 0
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}

			pm.mu.Lock()
			proj, ok := pm.projects[name]
			pm.mu.Unlock()
			if !ok {
				fmt.Fprintf(w, "event: error\ndata: project not found\n\n")
				flusher.Flush()
				return
			}

			proj.mu.Lock()
			currentLen := len(proj.logs)
			if currentLen > lastLen {
				for _, line := range proj.logs[lastLen:] {
					fmt.Fprintf(w, "data: %s\n\n", line)
				}
				lastLen = currentLen
				flusher.Flush()
			}
			proj.mu.Unlock()

			time.Sleep(500 * time.Millisecond)
		}
	}
}
