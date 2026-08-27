// Command provisioner manages sandbox lifecycle on a host.
//
// It creates/destroys sandboxes via a backend (Docker today) and
// reverse-proxies to each sandbox's runtime. Listens on 127.0.0.1:8002
// by default.
//
// Usage:
//
//	provisioner                                # listen on 127.0.0.1:8002
//	SANDBOX_IMAGE=my-runtime:v1 provisioner    # override the sandbox image
//	PROVISIONER_ADDR=0.0.0.0:8002 provisioner  # override the listen address
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentry-ai/agentry/pkg/provisioner"
	"github.com/agentry-ai/agentry/pkg/telemetry"
)

// provisionerVersion is injected at build time via ldflags
// (-X main.provisionerVersion=<release>). Defaults to "dev" for local
// builds. Reported on GET /api/version and used by the dashboard's
// "Update server" panel.
var provisionerVersion = "dev"

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// Self-update swapper mode: when started by an in-progress update
	// (AGENTRY_SELFUPDATE_SWAP=1) this process is the detached updater,
	// not the server — it recreates the provisioner container on the new
	// image and exits. Must run BEFORE any normal startup.
	if provisioner.SelfUpdateSwapRequested() {
		provisioner.RunSelfUpdateSwap()
		return
	}

	provisioner.Version = provisionerVersion
	log.Printf("agentry provisioner starting (pid=%d version=%s)", os.Getpid(), provisionerVersion)

	telCtx, telCancel := context.WithTimeout(context.Background(), 10*time.Second)
	shutdown, err := telemetry.Init(telCtx, telemetry.ConfigFromEnv("agentry-provisioner", provisionerVersion))
	telCancel()
	if err != nil {
		log.Printf("telemetry init failed (continuing without): %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()

	cfg := provisioner.DefaultConfig()

	// Auto-provision the runtime API key (persisted under CertDir) so
	// sandbox runtimes are locked to this provisioner without the operator
	// setting anything — the install flow stays "paste the command, done".
	provisioner.EnsureRuntimeAPIKey(&cfg)
	if cfg.RuntimeAPIKey != "" {
		log.Printf("provisioner: runtime auth ENABLED (sandbox API key auto-managed)")
	} else {
		log.Printf("provisioner: runtime auth DISABLED (no AGENTRY_CERT_DIR; local-dev posture)")
	}

	backend, err := newBackend(cfg)
	if err != nil {
		log.Fatalf("init backend %q: %v", cfg.Backend, err)
	}

	p := provisioner.New(cfg, backend)
	if err := p.Run(); err != nil {
		log.Fatalf("provisioner shutdown error: %v", err)
	}
	log.Println("agentry provisioner stopped")
}

// newBackend constructs the sandbox backend the user selected via the
// BACKEND env var. Docker and Podman (routed through the Docker-compat
// socket, reusing the same hardened DockerBackend) are supported today;
// Kubernetes/Kata/gVisor are stubbed (see the switch below).
func newBackend(cfg provisioner.Config) (provisioner.Backend, error) {
	switch cfg.Backend {
	case provisioner.BackendDocker, "":
		posture := "strict"
		if cfg.BuilderMode {
			posture = "builder (SYS_ADMIN, unconfined seccomp)"
		}
		log.Printf("provisioner: backend=docker (image=%s host=%s shm=%dB security=%s)",
			cfg.SandboxImage, cfg.NodeHost, cfg.DefaultShmBytes, posture)
		b, err := provisioner.NewDockerBackend(nil, cfg.SandboxImage, cfg.NodeHost)
		if err != nil {
			return nil, err
		}
		b.SetDefaultShmBytes(cfg.DefaultShmBytes)
		b.SetBuilderMode(cfg.BuilderMode)
		return b, nil
	case provisioner.BackendPodman:
		posture := "strict"
		if cfg.BuilderMode {
			posture = "builder (SYS_ADMIN, unconfined seccomp)"
		}
		log.Printf("provisioner: backend=podman (docker-compat socket; image=%s host=%s shm=%dB security=%s)",
			cfg.SandboxImage, cfg.NodeHost, cfg.DefaultShmBytes, posture)
		cli, err := provisioner.NewPodmanCompatClient()
		if err != nil {
			return nil, fmt.Errorf("podman-compat client init: %w", err)
		}
		b, err := provisioner.NewDockerBackend(cli, cfg.SandboxImage, cfg.NodeHost)
		if err != nil {
			return nil, err
		}
		b.SetDefaultShmBytes(cfg.DefaultShmBytes)
		b.SetBuilderMode(cfg.BuilderMode)
		b.SetPodmanCompat(true)
		return b, nil

	// Kubernetes — and the stronger-isolation runtimes that ride on it,
	// Kata + gVisor — are on the roadmap but NOT ready: the current pod
	// builder has no security-context hardening, no egress policy, no
	// /dev/shm sizing, no builder mode, and the deploy/build path is
	// Docker-only. Rather than present a half-built, unhardened backend as
	// real, we stub it out. The WIP stays reachable for contributors behind
	// an explicit experimental opt-in (BACKEND=k8s-experimental).
	case provisioner.BackendK8s, "kata", "gvisor":
		return nil, fmt.Errorf("the %q backend isn't available yet — Kubernetes, Kata, and "+
			"gVisor support are coming soon. Use BACKEND=docker today (the supported backend)", cfg.Backend)
	case "k8s-experimental":
		log.Printf("provisioner: WARNING backend=k8s is EXPERIMENTAL and INCOMPLETE — " +
			"no security-context hardening, no egress policy, no /dev/shm sizing, no deploy/build. " +
			"Development only; do not run untrusted workloads on it.")
		return provisioner.NewK8sClient(cfg.KubeconfigPath)

	default:
		return nil, fmt.Errorf("unknown backend %q (supported: docker, podman)", cfg.Backend)
	}
}
