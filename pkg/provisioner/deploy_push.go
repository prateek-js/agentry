package provisioner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
)

// Deploy-push endpoint.
//
// Agentry-app, after a successful build, calls here to push the just-
// built image into the operator's external registry (GHCR, Docker
// Hub, …). The token reaches us decrypted across the bridge tunnel;
// we use it for the docker push and never persist it on disk.
//
// We use the docker daemon's HTTP API (cli.ImagePush) rather than
// shelling out to `docker push` because:
//   - We already hold a *client.Client from the build step.
//   - We can stream the registry response and stop on the first
//     error frame (the CLI swallows errors quietly without `--quiet=0`).
//   - No shell-escaping of the token; the API consumes it as a
//     structured auth header.

// DeployPushRequest is the body for POST /api/sandboxes/{id}/deploy-push.
//
// LocalImageRef is the tag the cluster's daemon already holds from a
// prior deploy-build (e.g. "deploy-myapp:dep_abc"). We retag it as
// TargetRef before push so the destination URL is independent of the
// build-time tag scheme.
type DeployPushRequest struct {
	LocalImageRef string `json:"local_image_ref"`
	TargetRef     string `json:"target_ref"` // "ghcr.io/agentry-ai/myapp:rev2"

	RegistryHost     string `json:"registry_host"`
	RegistryUsername string `json:"registry_username"`
	RegistryToken    string `json:"registry_token"`
}

// DeployPushResponse echoes the pushed image ref so the caller can
// record it on the DeploymentRevision row.
type DeployPushResponse struct {
	ImageRef string `json:"image_ref"`
}

// handleDeployPush retags + pushes the built image to an external
// registry. The token lives in r.Body for the duration of this call
// and is never logged.
func (p *Provisioner) handleDeployPush(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("id")
	if sandboxID == "" {
		writeError(w, http.StatusBadRequest, "sandbox id missing")
		return
	}

	var req DeployPushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if req.LocalImageRef == "" || req.TargetRef == "" || req.RegistryHost == "" {
		writeError(w, http.StatusBadRequest,
			"local_image_ref, target_ref, registry_host are required")
		return
	}
	if req.RegistryUsername == "" || req.RegistryToken == "" {
		writeError(w, http.StatusBadRequest,
			"registry_username + registry_token are required")
		return
	}
	// Sanity-check the target ref points at the same host we hold
	// creds for. Catches a caller bug where target_ref drifted from
	// registry_host (would otherwise push anonymously to whatever the
	// target_ref resolves to).
	if !strings.HasPrefix(req.TargetRef, req.RegistryHost+"/") {
		writeError(w, http.StatusBadRequest,
			"target_ref must start with registry_host")
		return
	}

	ctx := r.Context()
	dockerCli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "docker client unavailable: "+err.Error())
		return
	}

	// Step 1 — retag local image as the target URL.
	if err := dockerCli.ImageTag(ctx, req.LocalImageRef, req.TargetRef); err != nil {
		log.Printf("deploy-push: sandbox=%s ImageTag failed: %v", sandboxID, err)
		writeError(w, http.StatusBadGateway, "retag local image: "+err.Error())
		return
	}

	// Step 2 — push. ImagePush returns a JSON-line stream. We must
	// drain it AND check for an "error" frame: a docker push that
	// fails auth still exits the API call cleanly; the failure shows
	// up as one of the streamed event frames.
	authJSON, _ := json.Marshal(registry.AuthConfig{
		Username:      req.RegistryUsername,
		Password:      req.RegistryToken,
		ServerAddress: req.RegistryHost,
	})
	pushOpts := image.PushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(authJSON),
	}
	stream, err := dockerCli.ImagePush(ctx, req.TargetRef, pushOpts)
	if err != nil {
		log.Printf("deploy-push: sandbox=%s ImagePush start failed: %v", sandboxID, err)
		writeError(w, http.StatusBadGateway, "push start: "+err.Error())
		return
	}
	defer stream.Close()
	if pushErr := drainPushStream(stream); pushErr != nil {
		log.Printf("deploy-push: sandbox=%s push stream error: %v", sandboxID, pushErr)
		writeError(w, http.StatusBadGateway, "push: "+pushErr.Error())
		return
	}

	log.Printf("deploy-push: sandbox=%s pushed %s OK", sandboxID, req.TargetRef)
	writeJSON(w, http.StatusOK, DeployPushResponse{ImageRef: req.TargetRef})
}

// drainPushStream consumes the JSON-line stream from ImagePush and
// returns the first error event surfaced by the daemon. Docker's
// docs phrase this as "you should read this entirely" — failing to
// drain leaks the stream's underlying connection. We parse each line
// because an HTTP 200 from the start of the call is NOT a success
// signal on its own.
func drainPushStream(r io.Reader) error {
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
			// Truncated stream — caller will retry; surface as a
			// real error rather than silently ignoring.
			return fmt.Errorf("decode push event: %w", err)
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
