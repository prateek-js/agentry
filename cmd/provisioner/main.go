// ad-sandbox provisioner — manages sandbox Pods in Kubernetes.
//
// Usage:
//
//	sandbox-provisioner                        # Listen on :8002
//	SANDBOX_IMAGE=my-sandbox:v1 sandbox-provisioner
//	K8S_NAMESPACE=sandboxes sandbox-provisioner
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentry/agentry/pkg/provisioner"
	"github.com/agentry/agentry/pkg/telemetry"
)

const provisionerVersion = "1.0.0"

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Printf("ad-sandbox provisioner starting (pid=%d)", os.Getpid())

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

	backend, err := newBackend(cfg)
	if err != nil {
		log.Fatalf("init backend %q: %v", cfg.Backend, err)
	}

	p := provisioner.New(cfg, backend)
	if err := p.Run(); err != nil {
		log.Fatalf("provisioner shutdown error: %v", err)
	}
	log.Println("ad-sandbox provisioner stopped")
}

// newBackend constructs the sandbox backend the user selected via the
// BACKEND env var. Defaults to Kubernetes; Docker is the canonical
// single-host alternative.
func newBackend(cfg provisioner.Config) (provisioner.Backend, error) {
	switch cfg.Backend {
	case provisioner.BackendDocker:
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
	case provisioner.BackendK8s, "":
		log.Printf("provisioner: backend=k8s (kubeconfig=%s)", cfg.KubeconfigPath)
		return provisioner.NewK8sClient(cfg.KubeconfigPath)
	default:
		return nil, fmt.Errorf("unknown backend %q (want k8s or docker)", cfg.Backend)
	}
}
