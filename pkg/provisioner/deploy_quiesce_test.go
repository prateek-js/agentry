package provisioner

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Tests for the deploy-time pause/resume choreography. The race we're
// guarding against (`next dev` and `next build` clobbering each other
// in .next/) is invisible to unit tests because both processes have to
// be real to deadlock — but the orchestration that prevents it (list →
// stop matching → resume on the way out) is observable. We pin that
// here.

// fakeRuntime stands in for the sandbox runtime's project endpoints.
// Records every call, lets the test pre-seed which projects are
// "running," and answers /v1/project/{list,start,stop} with the
// matching status shape.
type fakeRuntime struct {
	mu sync.Mutex

	// projects keyed by name → "running" | "stopped"
	projects map[string]string

	// call log for assertions
	stops  []string
	starts []string
	lists  int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		projects: make(map[string]string),
	}
}

func (f *fakeRuntime) seed(name, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[name] = status
}

func (f *fakeRuntime) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/project/list":
		f.mu.Lock()
		f.lists++
		out := make([]runtimeProject, 0, len(f.projects))
		for name, status := range f.projects {
			out = append(out, runtimeProject{Name: name, Status: status})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	case "/v1/project/stop":
		var body struct{ Name string `json:"name"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.projects[body.Name] = "stopped"
		f.stops = append(f.stops, body.Name)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case "/v1/project/start":
		var body struct{ Name string `json:"name"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.projects[body.Name] = "running"
		f.starts = append(f.starts, body.Name)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	default:
		http.NotFound(w, r)
	}
}

// newQuiesceTestProvisioner wires a provisioner + mock backend pointing
// at our fake runtime. Returns the provisioner and the fake runtime so
// the test can seed / assert.
func newQuiesceTestProvisioner(t *testing.T) (*Provisioner, *fakeRuntime, string) {
	t.Helper()

	rt := newFakeRuntime()
	srv := httptest.NewServer(http.HandlerFunc(rt.handle))
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split URL: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	mock := NewMockBackend()
	sandboxID := "test-sb"
	mock.preSeed(sandboxID, host, int32(port))

	cfg := Config{
		Namespace:  "test-ns",
		NodeHost:   host,
		ListenAddr: ":0",
	}
	p := NewWithKey(cfg, mock, "secret")
	p.SetReadyProbe(nil)

	return p, rt, sandboxID
}

// 1. The dev server matching the build's project name gets paused.
//    Unrelated projects (e.g. a sidecar API server) are left alone.
func TestPauseProjectsAt_MatchesByBasename(t *testing.T) {
	p, rt, sid := newQuiesceTestProvisioner(t)
	rt.seed("app", "running")   // the dev server we WANT to pause
	rt.seed("worker", "running") // unrelated background worker

	paused := p.pauseProjectsAt(context.Background(), sid, "/workspace/projects/app")

	if len(paused) != 1 || paused[0] != "app" {
		t.Errorf("paused = %v; want [app]", paused)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.projects["app"] != "stopped" {
		t.Errorf("app status = %q; want stopped", rt.projects["app"])
	}
	if rt.projects["worker"] != "running" {
		t.Errorf("worker status = %q; want untouched (running)", rt.projects["worker"])
	}
	if len(rt.stops) != 1 || rt.stops[0] != "app" {
		t.Errorf("stops = %v; want [app]", rt.stops)
	}
}

// 2. Already-stopped projects aren't stopped again. The race is only
//    against running processes; stopped projects can't be writing to
//    .next/, so we don't return them in the resume list.
func TestPauseProjectsAt_SkipsAlreadyStopped(t *testing.T) {
	p, rt, sid := newQuiesceTestProvisioner(t)
	rt.seed("app", "stopped")

	paused := p.pauseProjectsAt(context.Background(), sid, "/workspace/projects/app")

	if len(paused) != 0 {
		t.Errorf("paused = %v; want empty (project was already stopped)", paused)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.stops) != 0 {
		t.Errorf("stops = %v; want no stop calls", rt.stops)
	}
}

// 3. Workspace-root build (single-project, projectPath == /workspace)
//    falls back to pausing every running project. This is rare today
//    but the fallback is what keeps us safe when the name convention
//    doesn't apply.
func TestPauseProjectsAt_WorkspaceRoot(t *testing.T) {
	p, rt, sid := newQuiesceTestProvisioner(t)
	rt.seed("app", "running")
	rt.seed("worker", "running")

	paused := p.pauseProjectsAt(context.Background(), sid, "/workspace")

	if len(paused) != 2 {
		t.Errorf("paused = %v; want both projects paused at /workspace root", paused)
	}
}

// 4. resumeProjects calls /v1/project/start for each name in the list
//    and ignores empty input cheaply.
func TestResumeProjects_StartsEach(t *testing.T) {
	p, rt, sid := newQuiesceTestProvisioner(t)
	rt.seed("app", "stopped")
	rt.seed("worker", "stopped")

	p.resumeProjects(sid, []string{"app", "worker"})

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.starts) != 2 {
		t.Errorf("starts = %v; want two start calls", rt.starts)
	}
	if rt.projects["app"] != "running" || rt.projects["worker"] != "running" {
		t.Errorf("post-resume state = %v; want both running", rt.projects)
	}
}

// 5. Empty resume list short-circuits without touching the runtime —
//    the common case (no projects to pause, nothing to bring back) must
//    not waste an HTTP call.
func TestResumeProjects_EmptyIsNoop(t *testing.T) {
	p, rt, sid := newQuiesceTestProvisioner(t)
	p.resumeProjects(sid, nil)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.starts) != 0 {
		t.Errorf("starts = %v; want zero (empty input must be a no-op)", rt.starts)
	}
}
