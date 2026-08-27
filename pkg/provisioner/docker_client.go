package provisioner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Annotation key prefix used to distinguish ad-sandbox annotations
// from regular Docker labels. Matches the K8s annotation namespace so
// the TTL machinery in ttl.go is backend-agnostic.
const dockerAnnotationPrefix = "ad-sandbox.io/"

// containerHTTPPort is the port the runtime listens on inside the
// container. We publish it to a random LOOPBACK-only host port —
// the provisioner co-locates with the Docker daemon and reaches the
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

// DockerBackend implements Backend by talking to the local Docker
// daemon via the official Go SDK. Suitable for single-host deploys and
// LLM-driven integration tests; for production K8s remains canonical.
//
// Mapping from K8s concepts:
//
//	Pod        → container, named "sandbox-<sandboxID>"
//	Service    → no-op (Docker port-binding happens at container create)
//	NodePort   → the host port that 8080/tcp was mapped to
//	Annotation → label at create time + in-memory overlay for mutations
//	             (Docker labels are immutable post-creation)
//
// The struct is safe for concurrent use; all SDK calls are stateless
// and the overlay is protected by mu.
type DockerBackend struct {
	cli      *client.Client
	image    string // default image when SandboxSpec.Image is empty
	nodeHost string // hostname clients should use to reach mapped ports

	// defaultShmBytes overrides Docker's 64 MiB /dev/shm default on every
	// container we create. 0 means "use Docker's default" (only set this if
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

	// podmanCompat is set when cli is pointed at Podman's Docker-compat
	// socket rather than genuine Docker. Podman's compat layer doesn't
	// reliably honor every HostConfig field, so this flips on
	// verify-after-create guards (RuntimeClass, egress netns) that fail
	// closed instead of silently running unprotected. See
	// NewPodmanCompatClient.
	podmanCompat bool

	mu      sync.RWMutex
	overlay map[string]map[string]string // sandboxID -> mutable annotation overlay
}

// SetBuilderMode flips the security-posture switch. See the field
// docstring. Call once at startup before any sandbox is created.
func (d *DockerBackend) SetBuilderMode(on bool) {
	d.builderMode = on
}

// SetPodmanCompat marks this backend as talking to Podman's Docker-compat
// socket rather than genuine Docker, enabling verify-after-create guards.
// See the field docstring. Call once at startup before any sandbox is created.
func (d *DockerBackend) SetPodmanCompat(on bool) {
	d.podmanCompat = on
}

// Client exposes the underlying docker.Client so callers outside the
// Backend interface (like the deploy build/run handlers) can invoke
// daemon operations not modeled in the interface — ImageBuild for the
// build pipeline, ContainerCreate/Start for the cluster target. The
// type assertion in the provisioner stays local; we don't widen the
// Backend interface for things only docker can do.
func (d *DockerBackend) Client() *client.Client { return d.cli }

// SetDefaultShmBytes overrides the per-container /dev/shm size. Call once at
// startup, before any sandbox is created. Pass 0 to revert to Docker's default.
func (d *DockerBackend) SetDefaultShmBytes(n int64) {
	d.defaultShmBytes = n
}

// NewDockerBackend constructs a DockerBackend. The image and nodeHost
// are seed values; per-sandbox specs override the image, and nodeHost
// is what we put into the SandboxInfo URLs returned to clients.
//
// `cli` is exposed for tests; production callers pass nil to get the
// default env-configured client.
func NewDockerBackend(cli *client.Client, image, nodeHost string) (*DockerBackend, error) {
	if cli == nil {
		c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("docker client init: %w", err)
		}
		cli = c
	}
	// Verify the daemon is reachable so we fail fast at startup rather
	// than later on the first sandbox create.
	if _, err := cli.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	if image == "" {
		image = "agentry/runtime:latest"
	}
	if nodeHost == "" {
		nodeHost = "localhost"
	}
	return &DockerBackend{
		cli:      cli,
		image:    image,
		nodeHost: nodeHost,
		overlay:  make(map[string]map[string]string),
	}, nil
}

// ─── Lifecycle ─────────────────────────────────────────────────────────────

func (d *DockerBackend) CreatePod(ctx context.Context, _ string, spec SandboxSpec) error {
	img := spec.Image
	if img == "" {
		img = d.image
	}

	// Pull the image if it's not present locally. Best-effort: if the
	// pull fails (offline air-gap with a pre-loaded image) we still try
	// to create and let Docker surface a clearer error.
	if err := d.ensureImage(ctx, img); err != nil {
		// Not fatal; just log via the error path and let create decide.
		// (We'd surface via a logger if we had one wired here.)
		_ = err
	}

	labels := map[string]string{
		"app":        "agentry-sandbox",
		"sandbox-id": spec.SandboxID,
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	if spec.ThreadID != "" {
		labels["thread-id"] = spec.ThreadID
	}
	// Persist annotations as labels so a provisioner restart can recover
	// the at-creation state. SetPodAnnotations overlays on top.
	for k, v := range spec.Annotations {
		labels[dockerAnnotationPrefix+strings.TrimPrefix(k, dockerAnnotationPrefix)] = v
	}

	hostCfg := &container.HostConfig{
		PortBindings: nat.PortMap{
			containerHTTPPort: []nat.PortBinding{{HostIP: loopbackHostIP, HostPort: ""}},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3},
	}
	// Apply security posture. Default = strict (drop all caps except the
	// few we need for the runtime daemon, seccomp+apparmor default,
	// no-new-privileges). Builder mode = permissive (SYS_ADMIN + seccomp
	// unconfined) so in-sandbox `build-image` / buildah can do its
	// user-namespace + overlay-fs work. Pick at provisioner start time
	// via AGENTRY_SANDBOX_BUILDER_MODE; opt-out by default.
	if d.builderMode {
		hostCfg.SecurityOpt = []string{
			"seccomp=unconfined",
			"apparmor=unconfined",
			"no-new-privileges:true",
		}
		hostCfg.CapAdd = []string{"SYS_ADMIN"}
		// Buildah hard-codes RLIMIT_NPROC=RLIMIT_NOFILE=1048576 on the
		// containers it creates for `RUN` steps and bails with "operation
		// not permitted" if the outer process can't set those values.
		hostCfg.Resources.Ulimits = append(hostCfg.Resources.Ulimits,
			&container.Ulimit{Name: "nproc", Soft: 1048576, Hard: 1048576},
			&container.Ulimit{Name: "nofile", Soft: 1048576, Hard: 1048576},
		)
		// /dev/fuse for fuse-overlayfs storage in buildah.
		hostCfg.Resources.Devices = append(hostCfg.Resources.Devices,
			container.DeviceMapping{
				PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse",
				CgroupPermissions: "rwm",
			})
	} else {
		// Strict default. Drop every capability, then add back ONLY
		// what the runtime daemon's known users need:
		//   - nothing required for shell-exec, file-write, code-exec,
		//     project-start. Modern Linux lets unprivileged processes
		//     bind() to ports >1024 and read most of /proc/self.
		// No SYS_ADMIN ⇒ buildah inside the sandbox won't work. That's
		// why builder mode exists.
		hostCfg.CapDrop = []string{"ALL"}
		hostCfg.SecurityOpt = []string{"no-new-privileges:true"}
		// Reasonable nofile / nproc — generous for the workloads we
		// run (python+jupyter+npm) but bounded.
		hostCfg.Resources.Ulimits = append(hostCfg.Resources.Ulimits,
			&container.Ulimit{Name: "nproc", Soft: 65536, Hard: 65536},
			&container.Ulimit{Name: "nofile", Soft: 65536, Hard: 65536},
		)
	}
	// Fork-bomb defense — applies in both modes. 4k pids is plenty for
	// a python/node/jupyter mix; well under Docker's 32k default.
	pidLimit := int64(4096)
	hostCfg.Resources.PidsLimit = &pidLimit
	if err := applyDockerResources(hostCfg, spec.Resources); err != nil {
		return fmt.Errorf("resources: %w", err)
	}
	if d.defaultShmBytes > 0 {
		// Docker's /dev/shm default is 64 MiB. That's the most common
		// source of OOM-like failures in sandbox workloads (pandas
		// joins, matplotlib renders, headless Chromium, multiprocess
		// Python). Apply the operator-configured size unless the
		// CreateRequest set it explicitly elsewhere.
		hostCfg.ShmSize = d.defaultShmBytes
	}
	mounts, err := dockerMountsFromVolumes(spec.Volumes)
	if err != nil {
		return fmt.Errorf("volumes: %w", err)
	}
	hostCfg.Mounts = mounts
	if spec.RuntimeClass != "" {
		// Docker doesn't have RuntimeClassName; it has Runtime (e.g. "runc"
		// or "runsc" for gVisor). We map directly.
		hostCfg.Runtime = spec.RuntimeClass
	}

	envVars := []string{
		"SANDBOX_ID=" + spec.SandboxID,
	}
	if spec.RuntimeAPIKey != "" {
		// Locks the runtime's shell/file/code-exec API to this provisioner:
		// the runtime reads $SANDBOX_API_KEY and rejects calls without it.
		envVars = append(envVars, "SANDBOX_API_KEY="+spec.RuntimeAPIKey)
	}

	containerCfg := &container.Config{
		Image:        img,
		Labels:       labels,
		ExposedPorts: nat.PortSet{containerHTTPPort: struct{}{}},
		Env:          envVars,
	}

	name := "sandbox-" + spec.SandboxID
	resp, err := d.cli.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, name)
	if err != nil {
		if isConflictErr(err) {
			// Container already exists with this name — treat as success
			// so the handler can short-circuit to the existing sandbox
			// (mirrors the K8s "AlreadyExists" path).
			return nil
		}
		return fmt.Errorf("container create: %w", err)
	}

	if err := d.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Best-effort cleanup of the half-created container so a retry
		// doesn't trip the "conflict" branch with a corpse.
		_ = d.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("container start: %w", err)
	}

	// Podman's docker-compat layer has been observed to silently drop
	// HostConfig.Runtime, which would mean a sandbox that asked for
	// gVisor/Kata isolation actually runs on the default runtime with no
	// indication anything went wrong. Verify it stuck; fail closed if not.
	if d.podmanCompat && spec.RuntimeClass != "" {
		insp, err := d.cli.ContainerInspect(ctx, resp.ID)
		if err != nil {
			_ = d.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return fmt.Errorf("runtime verify: inspect after create: %w", err)
		}
		got := ""
		if insp.HostConfig != nil {
			got = insp.HostConfig.Runtime
		}
		if got != spec.RuntimeClass {
			_ = d.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return fmt.Errorf(
				"runtime_class %q was requested but podman reports runtime %q — "+
					"podman's docker-compat layer does not reliably honor HostConfig.Runtime; "+
					"refusing to run this sandbox on an unverified runtime", spec.RuntimeClass, got)
		}
	}

	// Apply egress policy via a transient CAP_NET_ADMIN sidecar that
	// shares the main container's netns. The main container itself
	// never gets CAP_NET_ADMIN, so the LLM inside can't rewrite the
	// rules even with the SYS_ADMIN we grant for buildah (SYS_ADMIN
	// and NET_ADMIN are disjoint).
	if !spec.Egress.IsZero() {
		if err := d.applyEgress(ctx, name, img, spec.Egress); err != nil {
			_ = d.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return fmt.Errorf("egress apply: %w", err)
		}
	}
	return nil
}

// dockerMountsFromVolumes translates the API-facing Volume slice into
// Docker's mount.Mount shape. We support the two sources that have a
// natural Docker analog:
//
//   - HostPath  → mount.TypeBind to the host path
//   - EmptyDir  → mount.TypeVolume (anonymous; cleaned up on remove)
//
// PVC / ConfigMap / Secret have no first-class equivalent in plain
// Docker, so we reject them with a clear error rather than silently
// dropping the mount. Validator (validateVolumes) has already enforced
// exactly-one-source and path canonicality, so the cases here are
// authoritative.
func dockerMountsFromVolumes(vols []Volume) ([]mount.Mount, error) {
	if len(vols) == 0 {
		return nil, nil
	}
	out := make([]mount.Mount, 0, len(vols))
	for i := range vols {
		v := &vols[i]
		switch {
		case v.HostPath != nil:
			out = append(out, mount.Mount{
				Type:     mount.TypeBind,
				Source:   v.HostPath.Path,
				Target:   v.MountPath,
				ReadOnly: v.ReadOnly,
			})
		case v.EmptyDir != nil:
			out = append(out, mount.Mount{
				Type:   mount.TypeVolume,
				Target: v.MountPath,
			})
		case v.PVC != nil, v.ConfigMap != nil, v.Secret != nil:
			return nil, fmt.Errorf("volumes[%d] %q: pvc/config_map/secret sources are not supported by the docker backend", i, v.Name)
		default:
			return nil, fmt.Errorf("volumes[%d] %q: no source set", i, v.Name)
		}
	}
	return out, nil
}

func (d *DockerBackend) DeletePod(ctx context.Context, _ string, name string, gracePeriod int64) error {
	timeout := int(gracePeriod)
	err := d.cli.ContainerRemove(ctx, name, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if err != nil {
		if errdefs.IsNotFound(err) {
			d.dropOverlay(name)
			return nil
		}
		return fmt.Errorf("container remove: %w", err)
	}
	_ = timeout // RemoveOptions.Force makes the grace period implicit
	d.dropOverlay(name)
	return nil
}

func (d *DockerBackend) GetPodPhase(ctx context.Context, _ string, name string) (string, error) {
	insp, err := d.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "NotFound", nil
		}
		return "Unknown", fmt.Errorf("container inspect: %w", err)
	}
	return dockerStateToPhase(insp.State), nil
}

// dockerStateToPhase maps Docker's per-container state string into the
// K8s phase vocabulary so /api/sandboxes responses look the same
// regardless of backend.
func dockerStateToPhase(s *container.State) string {
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

// ─── Services (no-ops for Docker; ports are bound at container create) ────

func (d *DockerBackend) CreateService(_ context.Context, _ string, _ SandboxSpec) error {
	return nil
}

func (d *DockerBackend) DeleteService(_ context.Context, _ string, _ string) error {
	return nil
}

// GetNodePort returns the host port that 8080/tcp was mapped to. The
// `name` argument follows the K8s convention "sandbox-<id>-svc"; we
// strip the suffix to get the container name.
func (d *DockerBackend) GetNodePort(ctx context.Context, _ string, name string) (int32, error) {
	containerName := strings.TrimSuffix(name, "-svc")
	insp, err := d.cli.ContainerInspect(ctx, containerName)
	if err != nil {
		return 0, fmt.Errorf("container inspect: %w", err)
	}
	if insp.NetworkSettings == nil {
		return 0, errors.New("no NetworkSettings on container")
	}
	bindings, ok := insp.NetworkSettings.Ports[containerHTTPPort]
	if !ok || len(bindings) == 0 {
		return 0, fmt.Errorf("no host binding for %s", containerHTTPPort)
	}
	p, err := strconv.ParseInt(bindings[0].HostPort, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse host port %q: %w", bindings[0].HostPort, err)
	}
	return int32(p), nil
}

// ─── Listing ───────────────────────────────────────────────────────────────

func (d *DockerBackend) ListSandboxes(ctx context.Context, _ string, _ map[string]string) ([]SandboxInfo, error) {
	f := filters.NewArgs()
	f.Add("label", "app=agentry-sandbox")
	containers, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}

	out := make([]SandboxInfo, 0, len(containers))
	for _, c := range containers {
		sid := c.Labels["sandbox-id"]
		if sid == "" {
			continue
		}
		var hostPort int32
		for _, p := range c.Ports {
			if p.PrivatePort == 8080 && p.PublicPort > 0 {
				hostPort = int32(p.PublicPort)
				break
			}
		}
		if hostPort == 0 {
			continue
		}
		out = append(out, SandboxInfo{
			SandboxID:  sid,
			SandboxURL: fmt.Sprintf("http://%s:%d", d.nodeHost, hostPort),
			Status:     dockerListStateToPhase(c.State),
			ExpiresAt:  d.expiresAtFor(sid, c.Labels),
		})
	}
	return out, nil
}

// dockerListStateToPhase converts the abbreviated state returned by
// ContainerList (e.g. "running", "exited") into the K8s phase string.
func dockerListStateToPhase(state string) string {
	switch strings.ToLower(state) {
	case "running":
		return "Running"
	case "created", "restarting":
		return "Pending"
	case "paused":
		return "Paused"
	case "exited", "dead":
		return "Failed"
	default:
		return "Unknown"
	}
}

// ─── Exec ──────────────────────────────────────────────────────────────────

func (d *DockerBackend) ExecInPod(ctx context.Context, _ string, name string, cmd []string) (string, error) {
	createResp, err := d.cli.ContainerExecCreate(ctx, name, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}

	attach, err := d.cli.ContainerExecAttach(ctx, createResp.ID, container.ExecStartOptions{})
	if err != nil {
		return "", fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	// Multiplexed stdout/stderr — stdcopy demuxes into separate buffers.
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil && err != io.EOF {
		return stdout.String(), fmt.Errorf("read exec output: %w (stderr=%s)", err, stderr.String())
	}

	// Surface non-zero exits as errors with stderr in the message, the
	// same way the K8s backend does.
	insp, err := d.cli.ContainerExecInspect(ctx, createResp.ID)
	if err == nil && insp.ExitCode != 0 {
		return stdout.String(), fmt.Errorf("exec exit %d: %s", insp.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ─── Annotations ───────────────────────────────────────────────────────────
//
// Strategy: at CreatePod time we write annotation values as Docker
// labels under the ad-sandbox.io/ prefix. After that, labels are
// immutable, so SetPodAnnotations writes to a process-local overlay.
// GetPodAnnotations merges label values with the overlay (overlay wins).

func (d *DockerBackend) GetPodAnnotations(ctx context.Context, _ string, name string) (map[string]string, error) {
	insp, err := d.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("container %s not found", name)
		}
		return nil, fmt.Errorf("container inspect: %w", err)
	}

	out := make(map[string]string)
	for k, v := range insp.Config.Labels {
		if strings.HasPrefix(k, dockerAnnotationPrefix) {
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

func (d *DockerBackend) SetPodAnnotations(_ context.Context, _ string, name string, annotations map[string]string) error {
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

// expiresAtFor returns the live expires-at value for a sandbox, taking
// the in-memory overlay into account so the reaper sees TTL renewals.
func (d *DockerBackend) expiresAtFor(sandboxID string, labels map[string]string) string {
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

func (d *DockerBackend) dropOverlay(name string) {
	sandboxID := strings.TrimPrefix(name, "sandbox-")
	d.mu.Lock()
	delete(d.overlay, sandboxID)
	d.mu.Unlock()
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func (d *DockerBackend) ensureImage(ctx context.Context, ref string) error {
	_, inspectErr := d.cli.ImageInspect(ctx, ref)
	present := inspectErr == nil

	// A pinned tag or digest is content-stable, so a local copy is
	// authoritative — skip the registry round-trip. But a mutable tag
	// (:latest) on disk may be stale: a freshly launched sandbox must not
	// silently reuse an old :latest after a new image was pushed. Always
	// re-pull mutable tags so new sandboxes land on the current image.
	if present && !isMutableTag(ref) {
		return nil
	}

	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		// Best-effort refresh: if we already have a copy (e.g. offline
		// air-gap or a transient registry hiccup), use it rather than
		// failing sandbox creation. Only a missing image is fatal.
		if present {
			log.Printf("ensureImage: refresh of %s failed (%v); using cached image", ref, err)
			return nil
		}
		return fmt.Errorf("image pull: %w", err)
	}
	defer rc.Close()
	// Drain the progress stream so the pull actually completes.
	_, err = io.Copy(io.Discard, rc)
	return err
}

// isMutableTag reports whether ref points at a tag whose contents can change
// under us — i.e. ":latest" or an untagged ref (which Docker treats as
// :latest). Digest pins ("@sha256:…") and explicit version tags are stable.
func isMutableTag(ref string) bool {
	if strings.Contains(ref, "@") {
		return false // digest-pinned
	}
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:] // drop registry host:port + path
	}
	colon := strings.LastIndex(name, ":")
	if colon < 0 {
		return true // no tag → :latest implied
	}
	return name[colon+1:] == "latest"
}

func isConflictErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return errdefs.IsConflict(err) ||
		strings.Contains(msg, "already in use") ||
		strings.Contains(msg, "already exists")
}

// applyDockerResources translates the API-facing Resources struct into
// Docker's HostConfig.Resources fields. CPU "500m" → NanoCPUs;
// memory "1Gi" → Memory bytes; GPU passthrough is not implemented for
// Docker yet (would need device requests + nvidia runtime; out of
// scope for the test backend).
func applyDockerResources(hc *container.HostConfig, r *Resources) error {
	if r == nil || r.Limits == nil {
		return nil
	}
	if cpu := r.Limits.CPU; cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return fmt.Errorf("cpu %q: %w", cpu, err)
		}
		// MilliValue * 1e6 == NanoCPUs (1 core == 1000m == 1e9 nanos).
		hc.NanoCPUs = q.MilliValue() * 1_000_000
	}
	if mem := r.Limits.Memory; mem != "" {
		q, err := resource.ParseQuantity(mem)
		if err != nil {
			return fmt.Errorf("memory %q: %w", mem, err)
		}
		hc.Memory = q.Value()
	}
	// Storage and GPU intentionally skipped — see method comment.
	_ = corev1.ResourceCPU // keep the corev1 import used in case future fields need it
	return nil
}
