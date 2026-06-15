package provisioner

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// selfupdate.go — in-place upgrade of the provisioner's own container.
//
// The provisioner runs as a Docker container (`agentry-provisioner`) on
// the cluster host with the docker socket mounted. "Update" means
// pulling the newest image and recreating that container on it — which
// a container can't do to itself synchronously. So the flow is:
//
//	POST /api/update  →  pull latest image; if unchanged, no-op.
//	                  →  else launch a detached "swapper" (the NEW image
//	                     in swap mode) and return immediately.
//	swapper           →  inspect the old container (env, mounts, network,
//	                     restart — the enrollment token lives in env!),
//	                     stop+rm it, run the new image with that exact
//	                     spec, verify it comes up, ROLL BACK to the old
//	                     image if it doesn't.
//
// Safety properties:
//   - Any failure BEFORE the swap (pull error, etc.) leaves the running
//     container untouched.
//   - The new container reuses the old's full HostConfig/Config, so the
//     enrollment token and all wiring survive.
//   - If the new image won't start, the swapper restores the old image,
//     so a bad release can't strand a cluster offline.
//
// We identify the container by NAME, never by hostname: the provisioner
// runs with --network=host, so os.Hostname() returns the *host's* name,
// not the container id.

// Version is the provisioner build version, injected via ldflags
// (-X main.provisionerVersion) and copied here from main at startup.
// Reported on GET /api/version; the dashboard compares it to the latest.
var Version = "dev"

const updaterContainerName = "agentry-provisioner-updater"

// updateInProgress guards against overlapping self-updates.
var updateInProgress atomic.Bool

// provisionerContainerName is the well-known name of our own container.
// Overridable for the rare operator who renamed it.
func provisionerContainerName() string {
	return envOr("AGENTRY_PROVISIONER_CONTAINER", "agentry-provisioner")
}

// SelfUpdateSwapRequested reports whether this process was started as the
// detached swapper rather than the normal server. main checks this first.
func SelfUpdateSwapRequested() bool {
	return os.Getenv("AGENTRY_SELFUPDATE_SWAP") == "1"
}

// handleVersion reports the running build version. GET /api/version.
func (p *Provisioner) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": Version})
}

type updateResult struct {
	Updated bool   `json:"updated"`
	Message string `json:"message"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Version string `json:"version"`
}

// handleUpdate pulls the latest image and, if it differs, launches the
// swapper. POST /api/update.
func (p *Provisioner) handleUpdate(w http.ResponseWriter, r *http.Request) {
	cli, err := p.docker()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "self-update requires the docker backend: "+err.Error())
		return
	}
	if !updateInProgress.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "an update is already in progress")
		return
	}
	res, err := p.startSelfUpdate(r.Context(), cli)
	if err != nil {
		updateInProgress.Store(false) // pre-swap failure: safe to retry
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// On a real update we leave the flag set — the swapper is about to
	// stop this process anyway. On a no-op, clear it.
	if !res.Updated {
		updateInProgress.Store(false)
	}
	writeJSON(w, http.StatusOK, res)
}

func (p *Provisioner) startSelfUpdate(ctx context.Context, cli *client.Client) (updateResult, error) {
	name := provisionerContainerName()

	self, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		return updateResult{}, fmt.Errorf("inspect own container %q (set AGENTRY_PROVISIONER_CONTAINER if it's named differently): %w", name, err)
	}
	imageRef := self.Config.Image // e.g. ghcr.io/agentry-ai/sandbox-provisioner:latest
	currentImageID := self.Image  // sha256:...

	// Best-effort: also refresh the sandbox runtime image so newly
	// created sandboxes + deploys land on the latest. Non-fatal — this
	// run is about upgrading the provisioner; a runtime pull hiccup
	// shouldn't block that.
	if p.config.SandboxImage != "" {
		if rrc, perr := cli.ImagePull(ctx, p.config.SandboxImage, image.PullOptions{}); perr == nil {
			_, _ = io.Copy(io.Discard, rrc)
			_ = rrc.Close()
		} else {
			log.Printf("selfupdate: runtime image pull (non-fatal): %v", perr)
		}
	}

	// Pull the newest image for this ref. On any failure the running
	// container is untouched — we never reach the swap.
	rc, err := cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return updateResult{}, fmt.Errorf("pull %s: %w", imageRef, err)
	}
	_, _ = io.Copy(io.Discard, rc) // ImagePull is async until the body is drained
	_ = rc.Close()

	newImageID, err := imageIDForRef(ctx, cli, imageRef)
	if err != nil {
		return updateResult{}, fmt.Errorf("resolve pulled image id: %w", err)
	}
	if newImageID == "" || newImageID == currentImageID {
		return updateResult{Updated: false, Message: "already running the latest image", Version: Version}, nil
	}

	if err := launchSwapper(ctx, cli, name, imageRef, currentImageID); err != nil {
		return updateResult{}, fmt.Errorf("launch updater: %w", err)
	}
	return updateResult{
		Updated: true,
		Message: "update started — this server will disconnect for a few seconds and reconnect on the new version",
		From:    shortImageID(currentImageID),
		To:      shortImageID(newImageID),
		Version: Version,
	}, nil
}

// imageIDForRef returns the local image id currently tagged as ref.
// Uses ImageList (present across SDK versions) rather than image-inspect.
func imageIDForRef(ctx context.Context, cli *client.Client, ref string) (string, error) {
	imgs, err := cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", ref)),
	})
	if err != nil {
		return "", err
	}
	if len(imgs) == 0 {
		return "", nil
	}
	return imgs[0].ID, nil
}

// launchSwapper starts the detached updater: the NEW image, in swap
// mode, with the docker socket so it can recreate us.
func launchSwapper(ctx context.Context, cli *client.Client, target, newImageRef, oldImageID string) error {
	// Clear any leftover updater from a previous run (kept for its logs).
	_ = cli.ContainerRemove(ctx, updaterContainerName, container.RemoveOptions{Force: true})

	cfg := &container.Config{
		Image: newImageRef,
		Env: []string{
			"AGENTRY_SELFUPDATE_SWAP=1",
			"AGENTRY_SELFUPDATE_TARGET=" + target,
			"AGENTRY_SELFUPDATE_NEW_IMAGE=" + newImageRef,
			"AGENTRY_SELFUPDATE_OLD_IMAGE=" + oldImageID,
		},
	}
	hostCfg := &container.HostConfig{
		Binds:       []string{"/var/run/docker.sock:/var/run/docker.sock"},
		NetworkMode: "host",
		AutoRemove:  false, // keep for log inspection; removed at next update
	}
	resp, err := cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, updaterContainerName)
	if err != nil {
		return err
	}
	return cli.ContainerStart(ctx, resp.ID, container.StartOptions{})
}

// RunSelfUpdateSwap is the entrypoint when the binary runs as the
// detached updater (AGENTRY_SELFUPDATE_SWAP=1). It recreates the
// provisioner on the new image, preserving the old container's full
// spec, and rolls back to the old image if the new one won't come up.
func RunSelfUpdateSwap() {
	target := envOr("AGENTRY_SELFUPDATE_TARGET", "agentry-provisioner")
	newImage := os.Getenv("AGENTRY_SELFUPDATE_NEW_IMAGE")
	oldImage := os.Getenv("AGENTRY_SELFUPDATE_OLD_IMAGE")
	log.Printf("selfupdate: swapper starting target=%s new=%s old=%s", target, newImage, shortImageID(oldImage))

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("selfupdate: docker client: %v", err)
	}
	ctx := context.Background()

	// Let the old provisioner flush its HTTP response + drain.
	time.Sleep(2 * time.Second)

	// Capture the old container's spec BEFORE we remove it. env/mounts/
	// network/restart all ride along — including the enrollment token.
	old, err := cli.ContainerInspect(ctx, target)
	if err != nil {
		log.Fatalf("selfupdate: inspect target %s: %v", target, err)
	}

	if err := swapTo(ctx, cli, target, old.Config, old.HostConfig, newImage); err != nil {
		log.Printf("selfupdate: new image failed to come up (%v) — rolling back to %s", err, shortImageID(oldImage))
		if rbErr := swapTo(ctx, cli, target, old.Config, old.HostConfig, oldImage); rbErr != nil {
			log.Printf("selfupdate: ROLLBACK FAILED (%v) — run update.sh on the host to recover", rbErr)
			os.Exit(1)
		}
		log.Printf("selfupdate: rolled back; cluster restored on the previous version")
		os.Exit(1)
	}
	log.Printf("selfupdate: done — provisioner now running %s", shortImageID(newImage))
}

// swapTo stops+removes `target` and recreates it from `img`, reusing the
// captured Config/HostConfig, then waits for it to be running.
func swapTo(ctx context.Context, cli *client.Client, target string, cfg *container.Config, hostCfg *container.HostConfig, img string) error {
	timeout := 20
	_ = cli.ContainerStop(ctx, target, container.StopOptions{Timeout: &timeout})
	if err := cli.ContainerRemove(ctx, target, container.RemoveOptions{Force: true}); err != nil {
		log.Printf("selfupdate: remove %s (continuing): %v", target, err)
	}
	cfg.Image = img // recreate on the target image; everything else preserved
	resp, err := cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, target)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return waitRunning(ctx, cli, resp.ID, 30*time.Second)
}

// waitRunning blocks until the container is running, or it exits/dies,
// or the deadline passes.
func waitRunning(ctx context.Context, cli *client.Client, id string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		insp, err := cli.ContainerInspect(ctx, id)
		if err == nil && insp.State != nil {
			if insp.State.Running {
				// Settle briefly so a crash-on-boot is caught as a failure,
				// not a false success.
				time.Sleep(2 * time.Second)
				insp2, err2 := cli.ContainerInspect(ctx, id)
				if err2 == nil && insp2.State != nil && insp2.State.Running {
					return nil
				}
			}
			if insp.State.Dead || insp.State.Status == "exited" {
				return fmt.Errorf("container exited (status=%s exit=%d)", insp.State.Status, insp.State.ExitCode)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for the new container to run")
}

func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
