package provisioner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
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
	ImageRef string            `json:"image_ref"` // from deploy-build OR a registry ref
	Port     int               `json:"port"`      // port the app listens on inside the image
	Env      map[string]string `json:"env,omitempty"`

	// RegistryAuth is set when ImageRef points at a remote registry the
	// daemon needs to docker-pull before running. Absent means the image
	// is already present locally — the build-then-run path on the
	// SAME cluster as the build doesn't need to pull. Set for rollback
	// (re-running an older image from the org's registry) and for
	// cross-server deploys (the image was built elsewhere). The token
	// lives in r.Body for the duration of this call and is never
	// persisted or logged.
	RegistryAuth *DeploymentRegistryAuth `json:"registry_auth,omitempty"`
}

// DeploymentRegistryAuth mirrors the docker daemon's auth shape so we
// can hand it straight to ImagePull. Same fields used by deploy-push.
type DeploymentRegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
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

	// Pull from the registry if the caller handed us creds. The control
	// plane sets RegistryAuth for two cases — rollback (re-running an
	// older image from the org's registry) and cross-server (the image
	// was built on a different cluster). For the same-cluster build-
	// then-run path RegistryAuth is nil and we skip straight to
	// ContainerCreate, since the image is already in the daemon.
	//
	// Failure to pull is a hard error: if the daemon doesn't have the
	// image and we couldn't fetch it, ContainerCreate will fail with a
	// less clear error a few lines down. Fail loudly here.
	if req.RegistryAuth != nil {
		if err := pullForDeploy(ctx, dockerCli, req.ImageRef, req.RegistryAuth); err != nil {
			log.Printf("deploy-run: id=%s pull failed: %v", req.ID, err)
			writeError(w, http.StatusBadGateway, "pull image: "+err.Error())
			return
		}
	}

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

// pullForDeploy authenticates against the registry and pulls ImageRef
// into the local daemon. We drain the streaming event log and surface
// the first error frame the daemon emits — an HTTP 200 from ImagePull
// is NOT a success signal on its own (the failure for e.g. a bad token
// is delivered as a JSON event mid-stream). Same shape as drainPushStream.
func pullForDeploy(ctx context.Context, dockerCli imagePuller, ref string, auth *DeploymentRegistryAuth) error {
	authJSON, _ := json.Marshal(registry.AuthConfig{
		Username:      auth.Username,
		Password:      auth.Token,
		ServerAddress: auth.Host,
	})
	stream, err := dockerCli.ImagePull(ctx, ref, image.PullOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(authJSON),
	})
	if err != nil {
		return fmt.Errorf("start pull: %w", err)
	}
	defer stream.Close()
	return drainPullStream(stream)
}

// imagePuller is the slice of the docker client surface pullForDeploy
// needs. Keeping it small lets the unit test below stand a fake without
// reaching for a full docker daemon.
type imagePuller interface {
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
}

// drainPullStream parses the daemon's JSON-line stream and returns the
// first error frame. Identical contract to drainPushStream — we want
// the stream fully consumed (otherwise the underlying conn leaks) AND
// the first error surfaced (otherwise auth failures look like success).
func drainPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for dec.More() {
		var ev struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			ErrorD struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&ev); err != nil {
			return fmt.Errorf("decode pull event: %w", err)
		}
		if ev.Error != "" {
			return fmt.Errorf("%s", ev.Error)
		}
		if ev.ErrorD.Message != "" {
			return fmt.Errorf("%s", ev.ErrorD.Message)
		}
	}
	return nil
}

// writeJSON is a thin helper. Lives here so the deploy_* files can use
// it without depending on the file the sibling handler lives in.
//
// Kept identical to the unexported writer used elsewhere; consolidate
// once the codebase has reasons to vary the shape.
var _ = io.Discard // keep io import for future use
