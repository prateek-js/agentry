package provisioner

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// Deploy run + proxy endpoints — the cluster target's runtime side.
//
// One long-lived container per deployment, addressable through the
// bridge via /api/deployments/{id}/proxy. Mirrors the sandbox model:
// the container has no host-published ports, the provisioner talks to
// it over the daemon's internal docker network.
//
// Container naming:    deployment-<id>
// Internal port:       whatever the user's app binds inside the image
// Public URL:          bridge route Kind=deployment routes here

// DeploymentRunRequest is the body for POST /api/deployments.
type DeploymentRunRequest struct {
	ID       string            `json:"id"`        // dep_xxx — what agentry-app issued
	ImageRef string            `json:"image_ref"` // from deploy-build
	Port     int               `json:"port"`      // port the app listens on inside the image
	Env      map[string]string `json:"env,omitempty"`
}

// DeploymentRunResponse echoes the running container.
type DeploymentRunResponse struct {
	ID          string `json:"id"`
	Container   string `json:"container"`
	Port        int    `json:"port"`
	Status      string `json:"status"`
}

// DeploymentInfo is returned by GET /api/deployments/{id}.
type DeploymentInfo struct {
	ID        string    `json:"id"`
	Container string    `json:"container"`
	Image     string    `json:"image"`
	State     string    `json:"state"` // running | exited | not_found
	Port      int       `json:"port"`
	StartedAt time.Time `json:"started_at"`
}

const (
	deploymentContainerPrefix = "deployment-"
	deploymentPortLabel       = "agentry.deployment.port"
	deploymentIDLabel         = "agentry.deployment.id"
)

func (p *Provisioner) handleDeploymentRun(w http.ResponseWriter, r *http.Request) {
	var req DeploymentRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if req.ID == "" || req.ImageRef == "" || req.Port < 1 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "id, image_ref, valid port required")
		return
	}

	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker client unavailable: "+err.Error())
		return
	}
	containerName := deploymentContainerPrefix + req.ID

	ctx := r.Context()

	// Stop + remove any existing container with this name. Mirrors the
	// "redeploy is just rerun with a new image" UX — atomically swap
	// the previous container for the new one. Idempotent.
	_ = dockerCli.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true})

	envList := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		envList = append(envList, k+"="+v)
	}

	created, err := dockerCli.ContainerCreate(ctx,
		&container.Config{
			Image: req.ImageRef,
			Env:   envList,
			Labels: map[string]string{
				deploymentIDLabel:   req.ID,
				deploymentPortLabel: strconv.Itoa(req.Port),
			},
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 5},
		},
		&network.NetworkingConfig{},
		nil, containerName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "container create: "+err.Error())
		return
	}
	if err := dockerCli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		writeError(w, http.StatusBadGateway, "container start: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, DeploymentRunResponse{
		ID:        req.ID,
		Container: containerName,
		Port:      req.Port,
		Status:    "running",
	})
}

func (p *Provisioner) handleDeploymentGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id missing")
		return
	}
	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	containerName := deploymentContainerPrefix + id
	info, err := dockerCli.ContainerInspect(r.Context(), containerName)
	if err != nil {
		writeJSON(w, http.StatusOK, DeploymentInfo{ID: id, Container: containerName, State: "not_found"})
		return
	}
	port, _ := strconv.Atoi(info.Config.Labels[deploymentPortLabel])
	state := "exited"
	if info.State != nil && info.State.Running {
		state = "running"
	}
	started, _ := time.Parse(time.RFC3339Nano, info.State.StartedAt)
	writeJSON(w, http.StatusOK, DeploymentInfo{
		ID:        id,
		Container: containerName,
		Image:     info.Config.Image,
		State:     state,
		Port:      port,
		StartedAt: started,
	})
}

func (p *Provisioner) handleDeploymentStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id missing")
		return
	}
	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	containerName := deploymentContainerPrefix + id
	if err := dockerCli.ContainerRemove(r.Context(), containerName,
		container.RemoveOptions{Force: true}); err != nil {
		// Not-found = already stopped; treat as success for idempotency.
		if !strings.Contains(err.Error(), "No such container") {
			writeError(w, http.StatusBadGateway, "container remove: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "stopped"})
}

// handleDeploymentProxy is the runtime hop for the bridge.
//
// Bridge has Kind=deployment routes — when one matches a request, the
// bridge proxies through the cluster tunnel here. We look up the
// deployment container's IP on the docker network and reverse-proxy
// the request to {container_ip}:{port}{rest}. Same shape as the
// sandbox runtime proxy, but the target is a deployment container
// rather than a sandbox runtime.
func (p *Provisioner) handleDeploymentProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rest := r.PathValue("rest")
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	dockerCli, err := p.docker()
	if err != nil {
		http.Error(w, "docker client unavailable", http.StatusServiceUnavailable)
		return
	}
	info, err := dockerCli.ContainerInspect(r.Context(), deploymentContainerPrefix+id)
	if err != nil {
		http.Error(w, "deployment not found", http.StatusNotFound)
		return
	}
	if info.State == nil || !info.State.Running {
		http.Error(w, "deployment not running", http.StatusBadGateway)
		return
	}
	port, _ := strconv.Atoi(info.Config.Labels[deploymentPortLabel])
	if port == 0 {
		http.Error(w, "deployment port label missing", http.StatusInternalServerError)
		return
	}

	// Pick the container's bridge-network IP. For the default bridge
	// the container has one network entry; for compose / custom
	// networks we'd pick the first non-loopback. Cluster target only
	// uses default bridge today.
	var ip string
	if info.NetworkSettings != nil {
		for _, n := range info.NetworkSettings.Networks {
			if n.IPAddress != "" {
				ip = n.IPAddress
				break
			}
		}
	}
	if ip == "" {
		http.Error(w, "deployment container has no network address", http.StatusBadGateway)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, port))
	originalQuery := r.URL.RawQuery
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = rest
			req.URL.RawQuery = originalQuery
			req.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "deployment "+id+" unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// writeJSON is a thin helper. Lives here so the deploy_* files can use
// it without depending on the file the sibling handler lives in.
//
// Kept identical to the unexported writer used elsewhere; consolidate
// once the codebase has reasons to vary the shape.
var _ = io.Discard // keep io import for future use
