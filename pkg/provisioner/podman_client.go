//go:build podman

package provisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/containers/podman/v4/pkg/bindings"
	"github.com/containers/podman/v4/pkg/bindings/containers"
	"github.com/containers/podman/v4/pkg/bindings/images"
	"github.com/containers/podman/v4/pkg/bindings/system"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/specgen"
	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Annotation key prefix used to distinguish ad-sandbox annotations
// from regular Podman labels. Matches the K8s annotation namespace so
// the TTL machinery in ttl.go is backend-agnostic.
const podmanAnnotationPrefix = "ad-sandbox.io/"

// containerHTTPPort is the port the runtime listens on inside the
// container. We publish it to a random LOOPBACK-only host port —
// the provisioner co-locates with the Podman daemon and reaches the
// runtime via localhost (see runtime_proxy.go + Config.NodeHost).
// Binding to 0.0.0.0 would expose the runtime API (shell-exec,
// file-write, code-exec) to anyone who can reach the host, bypassing
// the bridge's mTLS gate + every org-scope check.
const containerHTTPPort = "8080/tcp"

// loopbackHostIP scopes published host ports to 127.0.0.1 so the
// runtime API is reachable only from co-located processes (provisioner)
// — never from the public network. External traffic always goes
// through the bridge tunnel.
const loopbackHostIP = "127.0.0.1"

// PodmanBackend implements Backend by talking to the local Podman
// daemon via the official Go SDK. Suitable for single-host deploys and
// LLM-driven integration tests; for production K8s remains canonical.
//
// Mapping from K8s concepts:
//
//	Pod        → container, named "sandbox-<sandboxID>"
//	Service    → no-op (Podman port-binding happens at container create)
//	NodePort   → the host port that 8080/tcp was mapped to
//	Annotation → label at create time + in-memory overlay for mutations
//	             (Podman labels are immutable post-creation)
//
// The struct is safe for concurrent use; all SDK calls are stateless
// and the overlay is protected by mu.
type PodmanBackend struct {
	cli      context.Context
	image    string // default image when SandboxSpec.Image is empty
	nodeHost string // hostname clients should use to reach mapped ports

	// defaultShmBytes overrides Podman's 64 MiB /dev/shm default on every
	// container we create. 0 means "use Podman's default" (only set this if
	// the operator deliberately disabled the override). Configured via
	// SANDBOX_DEFAULT_SHM_SIZE.
	defaultShmBytes int64

	// builderMode flips the security posture of created sandboxes from
	// strict (cap-drop=ALL, no-new-privileges, default seccomp) to
	// permissive (SYS_ADMIN, unconfined seccomp + apparmor, bigger
	// ulimits). The permissive posture lets `build-image` / buildah
	// run inside a sandbox; the strict default makes the sandbox
	// substantially harder to escape from.
	//
	// Set via AGENTRY_SANDBOX_BUILDER_MODE=true. Operators choose at
	// provisioner-start time — every sandbox the provisioner spawns
	// inherits the same posture. Per-sandbox flips are post-v1.
	builderMode bool

	mu      sync.RWMutex
	overlay map[string]map[string]string // sandboxID -> mutable annotation overlay
}

// SetBuilderMode flips the security-posture switch. See the field
// docstring. Call once at startup before any sandbox is created.
func (d *PodmanBackend) SetBuilderMode(on bool) {
	d.builderMode = on
}

// Client exposes the underlying podman.Client so callers outside the
// Backend interface (like the deploy build/run handlers) can invoke
// daemon operations not modeled in the interface — ImageBuild for the
// build pipeline, ContainerCreate/Start for the cluster target. The
// type assertion in the provisioner stays local; we don't widen the
// Backend interface for things only podman can do.
func (d *PodmanBackend) Client() context.Context { return d.cli }

// SetDefaultShmBytes overrides the per-container /dev/shm size. Call once at
// startup, before any sandbox is created. Pass 0 to revert to Podman's default.
func (d *PodmanBackend) SetDefaultShmBytes(n int64) {
	d.defaultShmBytes = n
}

// NewPodmanBackend constructs a PodmanBackend. The image and nodeHost
// are seed values; per-sandbox specs override the image, and nodeHost
// is what we put into the SandboxInfo URLs returned to clients.
//
// `cli` is exposed for tests; production callers pass nil to get the
// default env-configured client.
func NewPodmanBackend(cli context.Context, image, nodeHost string) (*PodmanBackend, error) {
	if cli == nil {
		c, err := bindings.NewConnection(context.Background(), "")
		if err != nil {
			return nil, fmt.Errorf("podman client init: %w", err)
		}
		cli = c
	}
	// Verify the daemon is reachable so we fail fast at startup rather
	// than later on the first sandbox create.
	if _, err := system.Info(cli, nil); err != nil {
		return nil, fmt.Errorf("podman daemon unreachable: %w", err)
	}
	if image == "" {
		image = "agentry/runtime:latest"
	}
	if nodeHost == "" {
		nodeHost = "localhost"
	}
	return &PodmanBackend{
		cli:      cli,
		image:    image,
		nodeHost: nodeHost,
		overlay:  make(map[string]map[string]string),
	}, nil
}

// ─── Lifecycle ─────────────────────────────────────────────────────────────

func (d *PodmanBackend) CreatePod(ctx context.Context, _ string, spec SandboxSpec) error {
	img := spec.Image
	if img == "" {
		img = d.image
	}

	// Pull the image if it's not present locally. Best-effort: if the
	// pull fails (offline air-gap with a pre-loaded image) we still try
	// to create and let Podman surface a clearer error.
	if err := d.ensureImage(ctx, img); err != nil {
		// Not fatal; just log via the error path and let create decide.
		// (We'd surface via a logger if we had one wired here.)
		_ = err
	}

	s := specgen.NewSpecGenerator(img, false)
	s.Name = "sandbox-" + spec.SandboxID

	s.Labels = map[string]string{
		"app":        "agentry-sandbox",
		"sandbox-id": spec.SandboxID,
	}
	for k, v := range spec.Labels {
		s.Labels[k] = v
	}
	if spec.ThreadID != "" {
		s.Labels["thread-id"] = spec.ThreadID
	}
	// Persist annotations as labels so a provisioner restart can recover
	// the at-creation state. SetPodAnnotations overlays on top.
	for k, v := range spec.Annotations {
		s.Labels[podmanAnnotationPrefix+strings.TrimPrefix(k, podmanAnnotationPrefix)] = v
	}

	s.PortMappings = []entities.PortMapping{
		{
			HostIP:        loopbackHostIP,
			ContainerPort: 8080,
			HostPort:      0, // random available port
			Protocol:      "tcp",
		},
	}
	s.RestartPolicy = "on-failure"
	restartRetries := uint(3)
	s.RestartTries = &restartRetries

	if d.builderMode {
		s.SeccompProfilePath = "unconfined"
		s.ApparmorProfile = "unconfined"
		s.CapAdd = []string{"SYS_ADMIN"}
		s.Rlimits = []string{
			"nproc=1048576:1048576",
			"nofile=1048576:1048576",
		}
		s.Devices = []string{"/dev/fuse"}
	} else {
		s.CapDrop = []string{"ALL"}
		s.SeccompProfilePath = "" // default
		s.Rlimits = []string{
			"nproc=65536:65536",
			"nofile=65536:65536",
		}
	}

	pidLimit := int64(4096)
	s.ResourceLimits = &specs.LinuxResources{
		Pids: &specs.LinuxPids{
			Limit: pidLimit,
		},
	}

	if err := applyPodmanResources(s, spec.Resources); err != nil {
		return fmt.Errorf("resources: %w", err)
	}

	if d.defaultShmBytes > 0 {
		s.ShmSize = d.defaultShmBytes
	}
	mounts, err := podmanMountsFromVolumes(spec.Volumes)
	if err != nil {
		return fmt.Errorf("volumes: %w", err)
	}
	s.Mounts = mounts
	if spec.RuntimeClass != "" {
		s.OCIRuntime = spec.RuntimeClass
	}

	s.Env = map[string]string{
		"SANDBOX_ID": spec.SandboxID,
	}
	if spec.RuntimeAPIKey != "" {
		s.Env["SANDBOX_API_KEY"] = spec.RuntimeAPIKey
	}

	resp, err := containers.CreateWithSpec(d.cli, s, nil)
	if err != nil {
		if isConflictErr(err) {
			return nil
		}
		return fmt.Errorf("container create: %w", err)
	}

	if err := containers.Start(d.cli, resp.ID, nil); err != nil {
		_, _ = containers.Remove(d.cli, resp.ID, &containers.RemoveOptions{Force: &[]bool{true}[0]})
		return fmt.Errorf("container start: %w", err)
	}

	if !spec.Egress.IsZero() {
		// TODO: implement egress for podman
	}
	return nil
}

func podmanMountsFromVolumes(vols []Volume) ([]specs.Mount, error) {
	if len(vols) == 0 {
		return nil, nil
	}
	out := make([]specs.Mount, 0, len(vols))
	for i := range vols {
		v := &vols[i]
		switch {
		case v.HostPath != nil:
			out = append(out, specs.Mount{
				Type:        "bind",
				Source:      v.HostPath.Path,
				Destination: v.MountPath,
				Options:     []string{"ro"}, // if v.ReadOnly
			})
		case v.EmptyDir != nil:
			out = append(out, specs.Mount{
				Type:        "volume",
				Destination: v.MountPath,
			})
		case v.PVC != nil, v.ConfigMap != nil, v.Secret != nil:
			return nil, fmt.Errorf("volumes[%d] %q: pvc/config_map/secret sources are not supported by the podman backend", i, v.Name)
		default:
			return nil, fmt.Errorf("volumes[%d] %q: no source set", i, v.Name)
		}
	}
	return out, nil
}

func (d *PodmanBackend) DeletePod(ctx context.Context, _ string, name string, gracePeriod int64) error {
	timeout := uint(gracePeriod)
	_, err := containers.Remove(d.cli, name, &containers.RemoveOptions{Force: &[]bool{true}[0], Timeout: &timeout})
	if err != nil {
		if strings.Contains(err.Error(), "no such container") {
			d.dropOverlay(name)
			return nil
		}
		return fmt.Errorf("container remove: %w", err)
	}
	d.dropOverlay(name)
	return nil
}

func (d *PodmanBackend) GetPodPhase(ctx context.Context, _ string, name string) (string, error) {
	insp, err := containers.Inspect(d.cli, name, nil)
	if err != nil {
		if strings.Contains(err.Error(), "no such container") {
			return "NotFound", nil
		}
		return "Unknown", fmt.Errorf("container inspect: %w", err)
	}
	return podmanStateToPhase(insp.State), nil
}

func podmanStateToPhase(s *entities.ContainerState) string {
	if s == nil {
		return "Unknown"
	}
	switch strings.ToLower(s.Status) {
	case "running":
		return "Running"
	case "created", "restarting":
		return "Pending"
	case "paused":
		return "Paused"
	case "exited", "dead", "removing":
		if s.ExitCode == 0 {
			return "Succeeded"
		}
		return "Failed"
	default:
		return "Unknown"
	}
}

func (d *PodmanBackend) CreateService(_ context.Context, _ string, _ SandboxSpec) error {
	return nil
}

func (d *PodmanBackend) DeleteService(_ context.Context, _ string, _ string) error {
	return nil
}

func (d *PodmanBackend) GetNodePort(ctx context.Context, _ string, name string) (int32, error) {
	containerName := strings.TrimSuffix(name, "-svc")
	insp, err := containers.Inspect(d.cli, containerName, nil)
	if err != nil {
		return 0, fmt.Errorf("container inspect: %w", err)
	}
	if insp.NetworkSettings == nil {
		return 0, errors.New("no NetworkSettings on container")
	}
	for _, p := range insp.NetworkSettings.Ports {
		if p.ContainerPort == 8080 {
			return int32(p.HostPort), nil
		}
	}
	return 0, fmt.Errorf("no host binding for %s", containerHTTPPort)
}

func (d *PodmanBackend) ListSandboxes(ctx context.Context, _ string, _ map[string]string) ([]SandboxInfo, error) {
	list, err := containers.List(d.cli, &containers.ListOptions{
		All: &[]bool{true}[0],
		Filters: map[string][]string{
			"label": {"app=agentry-sandbox"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}

	out := make([]SandboxInfo, 0, len(list))
	for _, c := range list {
		sid := c.Labels["sandbox-id"]
		if sid == "" {
			continue
		}
		var hostPort int32
		for _, p := range c.Ports {
			if p.ContainerPort == 8080 && p.HostPort > 0 {
				hostPort = int32(p.HostPort)
				break
			}
		}
		if hostPort == 0 {
			continue
		}
		out = append(out, SandboxInfo{
			SandboxID:  sid,
			SandboxURL: fmt.Sprintf("http://%s:%d", d.nodeHost, hostPort),
			Status:     podmanStateToPhase(&entities.ContainerState{Status: c.State}),
			ExpiresAt:  d.expiresAtFor(sid, c.Labels),
		})
	}
	return out, nil
}

func (d *PodmanBackend) ExecInPod(ctx context.Context, _ string, name string, cmd []string) (string, error) {
	execConfig := new(entities.ExecConfig)
	execConfig.Cmd = cmd
	execConfig.AttachStdout = true
	execConfig.AttachStderr = true

	execID, err := containers.ExecCreate(d.cli, name, execConfig)
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}

	var stdout, stderr bytes.Buffer
	streams := new(containers.ExecStartAndAttachOptions)
	streams.SetStdout(&stdout)
	streams.SetStderr(&stderr)

	err = containers.ExecStartAndAttach(d.cli, execID, streams)
	if err != nil {
		return "", fmt.Errorf("exec attach: %w", err)
	}

	inspect, err := containers.ExecInspect(d.cli, execID, nil)
	if err != nil {
		return "", fmt.Errorf("exec inspect: %w", err)
	}

	if inspect.ExitCode != 0 {
		return stdout.String(), fmt.Errorf("exec exit %d: %s", inspect.ExitCode, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

func (d *PodmanBackend) GetPodAnnotations(ctx context.Context, _ string, name string) (map[string]string, error) {
	insp, err := containers.Inspect(d.cli, name, nil)
	if err != nil {
		if strings.Contains(err.Error(), "no such container") {
			return nil, fmt.Errorf("container %s not found", name)
		}
		return nil, fmt.Errorf("container inspect: %w", err)
	}

	out := make(map[string]string)
	for k, v := range insp.Config.Labels {
		if strings.HasPrefix(k, podmanAnnotationPrefix) {
			out[k] = v
		}
	}

	sandboxID := strings.TrimPrefix(name, "sandbox-")
	d.mu.RLock()
	for k, v := range d.overlay[sandboxID] {
		out[k] = v
	}
	d.mu.RUnlock()
	return out, nil
}

func (d *PodmanBackend) SetPodAnnotations(_ context.Context, _ string, name string, annotations map[string]string) error {
	if len(annotations) == 0 {
		return nil
	}
	sandboxID := strings.TrimPrefix(name, "sandbox-")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.overlay[sandboxID] == nil {
		d.overlay[sandboxID] = make(map[string]string, len(annotations))
	}
	for k, v := range annotations {
		d.overlay[sandboxID][k] = v
	}
	return nil
}

func (d *PodmanBackend) expiresAtFor(sandboxID string, labels map[string]string) string {
	d.mu.RLock()
	if ov := d.overlay[sandboxID]; ov != nil {
		if v, ok := ov[AnnotationExpiresAt]; ok {
			d.mu.RUnlock()
			return v
		}
	}
	d.mu.RUnlock()
	return labels[AnnotationExpiresAt]
}

func (d *PodmanBackend) dropOverlay(name string) {
	sandboxID := strings.TrimPrefix(name, "sandbox-")
	d.mu.Lock()
	delete(d.overlay, sandboxID)
	d.mu.Unlock()
}

func (d *PodmanBackend) ensureImage(ctx context.Context, ref string) error {
	_, err := images.Get(d.cli, ref, nil)
	present := err == nil

	if present && !isMutableTag(ref) {
		return nil
	}

	_, err = images.Pull(d.cli, ref, nil)
	if err != nil {
		if present {
			log.Printf("ensureImage: refresh of %s failed (%v); using cached image", ref, err)
			return nil
		}
		return fmt.Errorf("image pull: %w", err)
	}
	return nil
}

func isMutableTag(ref string) bool {
	if strings.Contains(ref, "@") {
		return false
	}
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	colon := strings.LastIndex(name, ":")
	if colon < 0 {
		return true
	}
	return name[colon+1:] == "latest"
}

func isConflictErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "already in use") || strings.Contains(strings.ToLower(err.Error()), "conflict")
}

func applyPodmanResources(s *specgen.SpecGenerator, r *Resources) error {
	if r == nil || r.Limits == nil {
		return nil
	}
	if s.ResourceLimits == nil {
		s.ResourceLimits = &specs.LinuxResources{}
	}
	if cpu := r.Limits.CPU; cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return fmt.Errorf("cpu %q: %w", cpu, err)
		}
		period := uint64(100000)
		quota := q.MilliValue() * 100
		s.ResourceLimits.CPU = &specs.LinuxCPU{
			Quota:  &quota,
			Period: &period,
		}
	}
	if mem := r.Limits.Memory; mem != "" {
		q, err := resource.ParseQuantity(mem)
		if err != nil {
			return fmt.Errorf("memory %q: %w", mem, err)
		}
		limit := q.Value()
		s.ResourceLimits.Memory = &specs.LinuxMemory{
			Limit: &limit,
		}
	}
	return nil
}
