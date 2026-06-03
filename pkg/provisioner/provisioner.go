package provisioner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentry/agentry/pkg/auth"
	"github.com/agentry/agentry/pkg/models"
	"github.com/agentry/agentry/pkg/telemetry"
)

// APIKeyEnv is the environment variable used to enable API-key auth on the
// provisioner. When unset, the provisioner accepts unauthenticated requests.
const APIKeyEnv = "SANDBOX_API_KEY"

// SandboxInfo represents a managed sandbox.
type SandboxInfo struct {
	SandboxID  string `json:"sandbox_id"`
	SandboxURL string `json:"sandbox_url"`
	Status     string `json:"status"`

	// ExpiresAt is the RFC3339-UTC deadline after which the reaper will
	// delete this sandbox. Empty when no TTL is set.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// CreateRequest is the request body for creating a sandbox.
type CreateRequest struct {
	SandboxID string `json:"sandbox_id"`
	ThreadID  string `json:"thread_id"`

	// Resources optionally overrides the default CPU/memory/storage/GPU
	// requests and limits. Empty fields fall back to defaults.
	Resources *Resources `json:"resources,omitempty"`

	// RuntimeClass optionally selects a Kubernetes RuntimeClass for the
	// sandbox Pod, e.g. "gvisor", "kata", "firecracker". Empty = cluster
	// default runtime.
	RuntimeClass string `json:"runtime_class,omitempty"`

	// TTLSeconds, when positive, asks the reaper to delete this sandbox
	// after this many seconds. 0 or unset = no TTL (sandbox is immortal
	// until explicitly deleted).
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`

	// Volumes, when non-empty, are attached to the sandbox container.
	// See volumes.go for the per-source schemas.
	Volumes []Volume `json:"volumes,omitempty"`

	// Egress, when non-zero, installs an outbound packet filter at the
	// sandbox's netns boundary. See egress.go.
	Egress EgressPolicy `json:"egress,omitempty"`
}

// RenewRequest is the request body for POST /api/sandboxes/{id}/renew.
//
// If TTLSeconds is zero, the previous TTL recorded on the Pod is reused.
// If the sandbox had no prior TTL, the request returns 400.
type RenewRequest struct {
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// Provisioner manages sandbox pods in Kubernetes.
type Provisioner struct {
	config  Config
	backend Backend
	auth    *auth.Authenticator

	// catalog is the per-cluster directory of available services,
	// dev-deps, and skills. Served on GET /api/catalog and consulted
	// by binding / dev-dep / skill-load handlers. Populated by
	// LoadDefault at startup; can be hot-swapped later.
	catalog *Catalog

	// readyProbe is called from handleCreate to wait until the
	// freshly-spawned sandbox runtime is answering HTTP. Default:
	// HTTP-poll the sandbox URL's /health. Tests with mock backends
	// (where nothing is actually listening on NodeHost:nodePort)
	// override this with a no-op.
	readyProbe func(ctx context.Context, sandboxURL string) error
}

// Backend is the plugin seam between the provisioner control plane and a
// concrete sandbox mechanism (Docker, Kubernetes, Kata, gVisor, …).
// Anything that's policy lives on the provisioner side; anything that's
// mechanism (process isolation, networking, image management) lives behind
// this interface.
type Backend interface {
	CreatePod(ctx context.Context, namespace string, sandbox SandboxSpec) error
	DeletePod(ctx context.Context, namespace, name string, gracePeriod int64) error
	GetPodPhase(ctx context.Context, namespace, name string) (string, error)
	CreateService(ctx context.Context, namespace string, sandbox SandboxSpec) error
	DeleteService(ctx context.Context, namespace, name string) error
	GetNodePort(ctx context.Context, namespace, name string) (int32, error)
	ListSandboxes(ctx context.Context, namespace string, labels map[string]string) ([]SandboxInfo, error)
	ExecInPod(ctx context.Context, namespace, pod string, command []string) (string, error)

	// GetPodAnnotations returns all annotations on a pod, or an empty map if
	// the pod has none. Returns an error if the pod does not exist.
	GetPodAnnotations(ctx context.Context, namespace, name string) (map[string]string, error)

	// SetPodAnnotations merges the provided key/value pairs into the pod's
	// metadata.annotations. Existing keys are overwritten; unspecified keys
	// are left intact. Uses a strategic-merge patch under the hood.
	SetPodAnnotations(ctx context.Context, namespace, name string, annotations map[string]string) error
}

// SandboxSpec holds the spec for creating a sandbox pod + service.
type SandboxSpec struct {
	SandboxID string
	ThreadID  string
	Image     string
	Labels    map[string]string
	NodeHost  string

	// Resources holds the already-validated Pod resource requirements.
	// Zero-value falls back to provisioner defaults inside k8s_client.
	Resources *Resources

	// RuntimeClass, when non-empty, becomes Pod.Spec.RuntimeClassName.
	RuntimeClass string

	// Annotations applied verbatim to Pod metadata. Used by the TTL machinery
	// to attach expires-at/ttl-seconds — but generic enough for callers to
	// thread arbitrary metadata if needed.
	Annotations map[string]string

	// Volumes are mounted into the sandbox container. Already validated by
	// the handler before reaching the backend.
	Volumes []Volume

	// Egress is the outbound packet-filter policy the backend installs at
	// the sandbox's netns boundary. Zero-value = no policy.
	Egress EgressPolicy
}

// New creates a new Provisioner. Reads $SANDBOX_API_KEY for optional auth.
func New(cfg Config, backend Backend) *Provisioner {
	return NewWithKey(cfg, backend, os.Getenv(APIKeyEnv))
}

// NewWithKey is like New but takes the API key explicitly (useful for tests).
// An empty key disables auth.
func NewWithKey(cfg Config, backend Backend, apiKey string) *Provisioner {
	cat := NewCatalog()
	// LoadDefault is best-effort — a bad CATALOG_PATH logs but doesn't
	// crash the provisioner. The catalog stays empty until fixed; the
	// MCP layer surfaces a clean errcode.Internal on first read.
	if err := cat.LoadDefault(); err != nil {
		log.Printf("provisioner: catalog load failed (continuing with empty catalog): %v", err)
	}
	return &Provisioner{
		config:  cfg,
		backend: backend,
		auth:    auth.New(apiKey, "/health"),
		catalog: cat,
		readyProbe: func(ctx context.Context, url string) error {
			return waitPortReachable(ctx, url, 10*time.Second)
		},
	}
}

// SetReadyProbe overrides the post-create readiness check. Used by
// tests with mock backends — production callers should leave the
// default in place.
func (p *Provisioner) SetReadyProbe(fn func(ctx context.Context, url string) error) {
	p.readyProbe = fn
}

// Handler builds the configured HTTP handler chain
// (CONNECT-interceptor → telemetry → auth → routes). Exposed for
// tests so they can drive the provisioner via httptest without
// binding a port.
//
// The CONNECT interceptor sits at the outermost layer because
// CONNECT is the data-plane verb — it carries a "sandbox:port"
// request target that doesn't fit any HTTP route, and the upgrade
// hijack needs to happen before any middleware caches the
// ResponseWriter.
func (p *Provisioner) Handler() http.Handler {
	mux := http.NewServeMux()
	p.registerRoutes(mux)
	inner := telemetry.HTTPMiddleware(p.auth.Middleware(mux))
	return p.connectInterceptor(inner)
}

// connectInterceptor returns a handler that diverts CONNECT requests
// to handleSandboxConnect and passes everything else through to
// inner. Keeps the HTTP control plane (MCP RPCs) unaware that the
// data plane exists.
func (p *Provisioner) connectInterceptor(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			p.handleSandboxConnect(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// Run starts the HTTP server and TTL reaper, blocking until SIGTERM/SIGINT.
func (p *Provisioner) Run() error {
	auth.LogStartup("provisioner", APIKeyEnv, p.auth)

	server := &http.Server{
		Addr:         p.config.ListenAddr,
		Handler:      p.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	// Background processes (reaper, broker tunnel) share one ctx so
	// they all unwind on SIGTERM. defer cancel() guarantees nothing
	// leaks if Run is invoked from a test that doesn't signal.
	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	if p.config.ReaperInterval > 0 {
		reaper := NewReaper(p.backend, p.config.Namespace, p.config.ReaperInterval)
		go func() {
			if err := reaper.Run(bgCtx); err != nil && err != context.Canceled {
				log.Printf("reaper exited with error: %v", err)
			}
		}()
	}

	// Optional bridge tunnel. Three paths in to dialing:
	//
	//   - prod, fresh enroll: AGENTRY_ENROLL_TOKEN + AGENTRY_ENROLL_URL
	//     are set; AGENTRY_BRIDGE_URL is empty. BootstrapClusterCert
	//     enrolls, persists the cert + the bridge URL the enroll
	//     response carried, returns the bundle.
	//   - prod, restart with cert already on disk: env may not have
	//     the token; bundle.BridgeURL is loaded from disk.
	//   - dev: CertDir empty, AGENTRY_BRIDGE_URL set explicitly to a
	//     local http://… bridge in DEV_MODE.
	//
	// We start the bridge agent if EITHER a CertDir was configured
	// (prod) OR AGENTRY_BRIDGE_URL was set in env (dev override).
	if p.config.CertDir != "" || p.config.BridgeURL != "" {
		if p.config.ClusterID == "" {
			log.Fatalf("provisioner: AGENTRY_CLUSTER_NAME is required for the bridge tunnel")
		}
		bridgeURL := p.config.BridgeURL
		var tlsConf *tls.Config
		if p.config.CertDir != "" {
			bundle, err := BootstrapClusterCert(bgCtx, p.config)
			if err != nil {
				log.Fatalf("provisioner: cluster cert bootstrap: %v", err)
			}
			tlsConf, err = BuildClusterTLSConfig(bundle)
			if err != nil {
				log.Fatalf("provisioner: build mTLS config: %v", err)
			}
			go RunCertRenewer(bgCtx, p.config, bundle, tlsConf)
			// Env override wins; otherwise honour what the control
			// plane told us at enroll time (persisted in the bundle).
			if bridgeURL == "" {
				bridgeURL = bundle.BridgeURL
			}
		}
		if bridgeURL == "" {
			log.Fatalf("provisioner: cannot determine bridge URL — set AGENTRY_BRIDGE_URL or ensure the enroll response carried bridge_url")
		}
		// sandboxURL() reads p.config.BridgeURL to decide whether
		// sandbox_url should be the tunneled bridge.invalid path or
		// a direct host:port. On the cert-reuse path bridgeURL came
		// from bundle.BridgeURL, not from env, so p.config.BridgeURL
		// would otherwise stay empty and sandbox_url would fall back
		// to direct — unreachable from the device.
		p.config.BridgeURL = bridgeURL
		bc := NewBrokerClient(bridgeURL, p.config.ClusterID, p.Handler(), tlsConf)
		go func() {
			if err := bc.Run(bgCtx); err != nil && err != context.Canceled {
				log.Printf("bridge tunnel exited: %v", err)
			}
		}()
	}

	go func() {
		log.Printf("ad-sandbox provisioner listening on %s", p.config.ListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("provisioner error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down provisioner...")
	cancelBg()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(ctx)
}

func (p *Provisioner) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Per-cluster catalog — what services / dev-deps / skills this
	// cluster offers. Source of truth for bindings + skill loading.
	mux.HandleFunc("GET /api/catalog", p.handleCatalog)

	mux.HandleFunc("POST /api/sandboxes", p.handleCreate)
	mux.HandleFunc("GET /api/sandboxes", p.handleList)
	mux.HandleFunc("GET /api/sandboxes/{id}", p.handleGet)
	mux.HandleFunc("DELETE /api/sandboxes/{id}", p.handleDelete)
	mux.HandleFunc("POST /api/sandboxes/{id}/evict", p.handleEvict)
	mux.HandleFunc("POST /api/sandboxes/{id}/renew", p.handleRenew)
	mux.HandleFunc("POST /api/sandboxes/{id}/snapshot", p.handleSnapshot)
	mux.HandleFunc("POST /api/sandboxes/{id}/restore", p.handleRestore)

	// Service bindings — bind a cluster service to a sandbox. Writes
	// credential files under /etc/sandbox/creds/agentry/<service>/ inside
	// the sandbox; shell shim exports them as env vars on next shell
	// start (or project restart).
	mux.HandleFunc("POST /api/sandboxes/{id}/bindings", p.handleBindingCreate)
	mux.HandleFunc("GET /api/sandboxes/{id}/bindings", p.handleBindingList)
	// Privileged: returns full env including credential values. The
	// bridge gates this with the cluster admin cert; runtime tools
	// inside a sandbox cannot reach it. Used by the control plane at
	// deploy time to expand inherit_from_sandbox into a real env map.
	mux.HandleFunc("GET /api/sandboxes/{id}/bindings/env", p.handleBindingResolve)

	// Build — emits a deployment manifest + Dockerfile to
	// /workspace/.build/ inside the sandbox. v1 returns the artifacts
	// without pushing to a registry; deploy uses them to construct
	// the manifest sent to XDP.
	mux.HandleFunc("POST /api/sandboxes/{id}/build", p.handleBuild)
	mux.HandleFunc("POST /api/sandboxes/{id}/deploy-build", p.handleDeployBuild)

	// Deploy runtime (cluster target). A built image becomes a long-
	// lived container managed by these endpoints, addressable through
	// the bridge via the deployment proxy below.
	mux.HandleFunc("POST /api/deployments", p.handleDeploymentRun)
	mux.HandleFunc("GET /api/deployments/{id}", p.handleDeploymentGet)
	mux.HandleFunc("DELETE /api/deployments/{id}", p.handleDeploymentStop)
	mux.HandleFunc("/api/deployments/{id}/proxy/{rest...}", p.handleDeploymentProxy)

	// (Sandbox-scoped /deploy stub removed. The control-plane-driven
	// pipeline lives in agentry-app and calls /api/sandboxes/{id}/deploy-build
	// directly. MCP no longer exposes a deploy tool — users click Deploy
	// in the dashboard.)

	// User-staged secrets — writes to /etc/sandbox/creds/agentry/secrets/.
	// Set via `agentry env set` (user terminal, hidden prompt) or the
	// MCP env_set tool (which rejects secret-shaped values so they
	// don't leak into chat context). Listed by name only.
	mux.HandleFunc("POST /api/sandboxes/{id}/secrets", p.handleSecretSet)
	mux.HandleFunc("GET /api/sandboxes/{id}/secrets", p.handleSecretList)

	// Catch-all reverse-proxy: /api/sandboxes/{id}/runtime/* forwards
	// to the named sandbox's runtime. This is what makes runtime
	// tools reachable through the broker tunnel — the runtime port
	// itself is private to the cluster host.
	mux.HandleFunc("/api/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/runtime") {
			p.handleRuntimeProxy(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func (p *Provisioner) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	if req.SandboxID == "" {
		writeError(w, 400, "sandbox_id is required")
		return
	}
	if req.ThreadID == "" {
		req.ThreadID = req.SandboxID
	}

	// Validate user-supplied resource overrides early so we return 400, not
	// 500, on malformed quantities. The result is discarded here — k8s_client
	// re-runs the merge during CreatePod so the same Resources value flows
	// through SandboxSpec unchanged.
	if _, err := buildResourceRequirements(req.Resources); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	req.Volumes = mergeDefaultVolumes(req.Volumes, p.config.DefaultVolumes)
	if err := validateVolumes(req.Volumes); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if req.Egress.IsZero() {
		req.Egress = p.config.DefaultEgress
	}
	if err := req.Egress.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}

	ctx := r.Context()
	podName := "sandbox-" + req.SandboxID
	svcName := podName + "-svc"
	createStart := time.Now()
	log.Printf("provisioner: create sandbox=%s thread=%s image=%s", req.SandboxID, req.ThreadID, p.config.SandboxImage)

	// Check if already exists.
	if port, err := p.backend.GetNodePort(ctx, p.config.Namespace, svcName); err == nil && port > 0 {
		phase, _ := p.backend.GetPodPhase(ctx, p.config.Namespace, podName)
		log.Printf("provisioner: sandbox=%s already exists (phase=%s port=%d) — returning current", req.SandboxID, phase, port)
		writeJSON(w, 200, SandboxInfo{
			SandboxID:  req.SandboxID,
			SandboxURL: p.sandboxURL(req.SandboxID, port),
			Status:     phase,
		})
		return
	}

	annotations := ttlAnnotations(time.Now(), req.TTLSeconds)
	expiresAt := ""
	if annotations != nil {
		expiresAt = annotations[AnnotationExpiresAt]
	}

	spec := SandboxSpec{
		SandboxID:    req.SandboxID,
		ThreadID:     req.ThreadID,
		Image:        p.config.SandboxImage,
		Labels:       p.config.Labels,
		NodeHost:     p.config.NodeHost,
		Resources:    req.Resources,
		RuntimeClass: req.RuntimeClass,
		Annotations:  annotations,
		Volumes:      req.Volumes,
		Egress:       req.Egress,
	}

	// Create pod.
	if err := p.backend.CreatePod(ctx, p.config.Namespace, spec); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			writeError(w, 500, fmt.Sprintf("pod creation failed: %v", err))
			return
		}
	}

	// Create service.
	if err := p.backend.CreateService(ctx, p.config.Namespace, spec); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			// Rollback pod.
			_ = p.backend.DeletePod(ctx, p.config.Namespace, podName, 0)
			writeError(w, 500, fmt.Sprintf("service creation failed: %v", err))
			return
		}
	}

	// Wait for NodePort allocation.
	var nodePort int32
	for i := 0; i < 20; i++ {
		if port, err := p.backend.GetNodePort(ctx, p.config.Namespace, svcName); err == nil && port > 0 {
			nodePort = port
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if nodePort == 0 {
		writeError(w, 500, "NodePort not allocated in time")
		return
	}

	// directURL is the internal host:port we'll poll for readiness;
	// publicURL is what we publish to clients (broker-proxied path
	// when tunneled, same as directURL when not).
	directURL := fmt.Sprintf("http://%s:%d", p.config.NodeHost, nodePort)
	publicURL := p.sandboxURL(req.SandboxID, nodePort)

	// Wait until the runtime's TCP port is accept-ready inside the
	// container. There's a real race here: Docker maps the host port
	// the moment the container starts, but the runtime binary needs
	// ~200-800ms to bind it. Without this poll, every fresh sandbox's
	// first request races and many lose with ECONNRESET. We dial TCP
	// rather than HTTP-probe because the provisioner shouldn't know
	// what protocol the runtime speaks — accept-ready is the right
	// readiness contract. Tests with mock backends override via
	// SetReadyProbe (no real listener at NodeHost:nodePort).
	if p.readyProbe != nil {
		if err := p.readyProbe(ctx, directURL); err != nil {
			writeError(w, 500, fmt.Sprintf("runtime did not become ready: %v", err))
			return
		}
	}

	// Stamp AGENTRY_APP_NAME so apps in this sandbox have a stable
	// per-sandbox namespace for their data writes (mongo db name,
	// postgres schema, redis key prefix, s3 key prefix — see
	// /etc/sandbox/docs/app.md "Sharing one service across many apps").
	//
	// Matches the prod deploy convention: orchestrator stamps the
	// deployment slug; here we stamp the sandbox id. Both surface as
	// the same env var name so the same code (`process.env.AGENTRY_APP_NAME`)
	// works in dev and prod.
	//
	// Writes to /var/run/agentry/agentry/AGENTRY_APP_NAME — the shell
	// shim under /etc/profile.d/sandbox-creds.sh sources every file
	// under that tree as an env var on shell start.
	//
	// Retry: readyProbe is TCP-accept-ready, but the runtime takes
	// ~200-500ms more to wire its HTTP handlers; a write fired the
	// instant TCP accepts comes back as EOF (peer closed mid-response).
	// 5 tries × 200 ms = 1 s budget, plenty for a healthy runtime;
	// non-fatal if it ultimately misses — apps fall back to `?? "dev"`.
	stampPath := "/var/run/agentry/agentry/AGENTRY_APP_NAME"
	stampVal := []byte(req.SandboxID)
	var stampErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if stampErr = p.runtimeFileWrite(ctx, req.SandboxID, stampPath, stampVal); stampErr == nil {
			break
		}
	}
	if stampErr != nil {
		log.Printf("provisioner: stamp AGENTRY_APP_NAME for sandbox=%s: %v", req.SandboxID, stampErr)
	}

	phase, _ := p.backend.GetPodPhase(ctx, p.config.Namespace, podName)
	log.Printf("provisioner: sandbox=%s READY phase=%s port=%d url=%s elapsed=%s",
		req.SandboxID, phase, nodePort, publicURL, time.Since(createStart).Round(time.Millisecond))
	writeJSON(w, 200, SandboxInfo{
		SandboxID:  req.SandboxID,
		SandboxURL: publicURL,
		Status:     phase,
		ExpiresAt:  expiresAt,
	})
}

// waitPortReachable polls a TCP dial to the URL's host:port until a
// connection succeeds, or the budget runs out. TCP-only by design:
// the provisioner shouldn't know whether the thing inside the sandbox
// speaks HTTP, gRPC, or something custom — accept-ready is the right
// kernel-level definition of "the runtime is up". net.Listen makes
// the port accept-ready atomically with the bind, so once dial works
// any application-layer handler the runtime installed is reachable.
//
// Linear backoff (100 ms) with a 200 ms per-attempt dial timeout; the
// kernel returns ECONNREFUSED fast so retries are cheap.
func waitPortReachable(ctx context.Context, baseURL string, total time.Duration) error {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid sandbox URL %q", baseURL)
	}
	addr := u.Host
	// url.Parse normalizes "host:port" into Host; default ports are
	// elided. We never produce default-port URLs (we always include
	// the host port explicitly), so this is fine.

	deadline := time.Now().Add(total)
	var dialer net.Dialer
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dialCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, err := dialer.DialContext(dialCtx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("port %s not reachable within %s", addr, total)
}

func (p *Provisioner) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	port, err := p.backend.GetNodePort(ctx, p.config.Namespace, "sandbox-"+id+"-svc")
	if err != nil || port == 0 {
		writeError(w, 404, fmt.Sprintf("sandbox '%s' not found", id))
		return
	}

	podName := "sandbox-" + id
	phase, _ := p.backend.GetPodPhase(ctx, p.config.Namespace, podName)
	annotations, _ := p.backend.GetPodAnnotations(ctx, p.config.Namespace, podName)

	writeJSON(w, 200, SandboxInfo{
		SandboxID:  id,
		SandboxURL: p.sandboxURL(id, port),
		Status:     phase,
		ExpiresAt:  annotations[AnnotationExpiresAt],
	})
}

func (p *Provisioner) handleRenew(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	podName := "sandbox-" + id

	// Body is optional. If absent or malformed, treat as empty body.
	var req RenewRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid request body")
			return
		}
	}
	if req.TTLSeconds < 0 {
		writeError(w, 400, "ttl_seconds must be non-negative")
		return
	}

	existing, err := p.backend.GetPodAnnotations(ctx, p.config.Namespace, podName)
	if err != nil {
		writeError(w, 404, fmt.Sprintf("sandbox '%s' not found", id))
		return
	}

	// Determine effective TTL: request value if given, else stored value.
	ttl := req.TTLSeconds
	if ttl == 0 {
		stored := existing[AnnotationTTLSec]
		if stored == "" {
			writeError(w, 400, "sandbox has no prior TTL; provide ttl_seconds")
			return
		}
		var parseErr error
		ttl, parseErr = parseInt64(stored)
		if parseErr != nil || ttl <= 0 {
			writeError(w, 500, fmt.Sprintf("stored TTL invalid: %q", stored))
			return
		}
	}

	updated := ttlAnnotations(time.Now(), ttl)
	if err := p.backend.SetPodAnnotations(ctx, p.config.Namespace, podName, updated); err != nil {
		writeError(w, 500, fmt.Sprintf("renew failed: %v", err))
		return
	}

	writeJSON(w, 200, map[string]any{
		"sandbox_id":  id,
		"expires_at":  updated[AnnotationExpiresAt],
		"ttl_seconds": ttl,
	})
}

func parseInt64(s string) (int64, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func (p *Provisioner) handleList(w http.ResponseWriter, r *http.Request) {
	sandboxes, err := p.backend.ListSandboxes(r.Context(), p.config.Namespace, p.config.Labels)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("list failed: %v", err))
		return
	}
	// Rewrite each backend-returned URL to the public form. Backends
	// don't know about broker tunneling — they always return direct
	// host:port URLs — so the provisioner stamps the runtime-proxy
	// shape on top in tunneled mode. The port the backend gave us is
	// preserved (we still need it for the in-place form) but we
	// derive it from the URL rather than threading a new field.
	if p.config.BridgeURL != "" {
		for i := range sandboxes {
			sandboxes[i].SandboxURL = p.sandboxURL(sandboxes[i].SandboxID, 0)
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"sandboxes": sandboxes,
		"count":     len(sandboxes),
	})
}

func (p *Provisioner) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	log.Printf("provisioner: delete sandbox=%s", id)

	var errors []string
	if err := p.backend.DeleteService(ctx, p.config.Namespace, "sandbox-"+id+"-svc"); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			errors = append(errors, fmt.Sprintf("service: %v", err))
		}
	}
	if err := p.backend.DeletePod(ctx, p.config.Namespace, "sandbox-"+id, 0); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			errors = append(errors, fmt.Sprintf("pod: %v", err))
		}
	}

	if len(errors) > 0 {
		log.Printf("provisioner: delete sandbox=%s partial cleanup: %s", id, strings.Join(errors, ", "))
		writeError(w, 500, fmt.Sprintf("partial cleanup: %s", strings.Join(errors, ", ")))
		return
	}
	log.Printf("provisioner: sandbox=%s deleted", id)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "sandbox_id": id})
}

func (p *Provisioner) handleEvict(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	podName := "sandbox-" + id

	phase, _ := p.backend.GetPodPhase(ctx, p.config.Namespace, podName)
	if phase != "Running" && phase != "Pending" {
		writeJSON(w, 200, map[string]interface{}{"ok": true, "sandbox_id": id, "action": "already_gone"})
		return
	}

	// Delete with 30s grace period for auto-snapshot.
	if err := p.backend.DeletePod(ctx, p.config.Namespace, podName, 30); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeJSON(w, 200, map[string]interface{}{"ok": true, "sandbox_id": id, "action": "already_gone"})
			return
		}
		writeError(w, 500, fmt.Sprintf("eviction failed: %v", err))
		return
	}
	_ = p.backend.DeleteService(ctx, p.config.Namespace, "sandbox-"+id+"-svc")

	writeJSON(w, 200, map[string]interface{}{"ok": true, "sandbox_id": id, "action": "evicted"})
}

func (p *Provisioner) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	podName := "sandbox-" + id

	phase, _ := p.backend.GetPodPhase(ctx, p.config.Namespace, podName)
	if phase != "Running" {
		writeError(w, 404, fmt.Sprintf("sandbox '%s' not running (phase: %s)", id, phase))
		return
	}

	output, err := p.backend.ExecInPod(ctx, p.config.Namespace, podName, []string{
		"bash", "-c",
		"cd /workspace && tar czf - " +
			"--exclude=node_modules --exclude=.cache --exclude=dist " +
			"--exclude=build --exclude=.npm " +
			". 2>/dev/null | base64",
	})
	if err != nil {
		writeError(w, 500, fmt.Sprintf("snapshot failed: %v", err))
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"snapshot":   strings.TrimSpace(output),
		"size":       len(output),
		"sandbox_id": id,
	})
}

func (p *Provisioner) handleRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	podName := "sandbox-" + id

	var body struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Snapshot == "" {
		writeError(w, 400, "missing 'snapshot' in request body")
		return
	}

	phase, _ := p.backend.GetPodPhase(ctx, p.config.Namespace, podName)
	if phase != "Running" {
		writeError(w, 404, fmt.Sprintf("sandbox '%s' not running", id))
		return
	}

	// Write snapshot in chunks to avoid shell arg length limits.
	// First clear any previous temp file.
	_, _ = p.backend.ExecInPod(ctx, p.config.Namespace, podName, []string{
		"bash", "-c", "rm -f /tmp/snapshot.tar.gz",
	})

	// Write base64 in 50KB chunks (avoids exec buffer limits).
	chunkSize := 50000
	for i := 0; i < len(body.Snapshot); i += chunkSize {
		end := i + chunkSize
		if end > len(body.Snapshot) {
			end = len(body.Snapshot)
		}
		chunk := body.Snapshot[i:end]
		_, err := p.backend.ExecInPod(ctx, p.config.Namespace, podName, []string{
			"bash", "-c",
			fmt.Sprintf("echo '%s' | base64 -d >> /tmp/snapshot.tar.gz", chunk),
		})
		if err != nil {
			writeError(w, 500, fmt.Sprintf("restore write failed at chunk %d: %v", i/chunkSize, err))
			return
		}
	}

	// Extract snapshot.
	_, err := p.backend.ExecInPod(ctx, p.config.Namespace, podName, []string{
		"bash", "-c",
		"cd /workspace && tar xzf /tmp/snapshot.tar.gz 2>&1 && rm -f /tmp/snapshot.tar.gz",
	})
	if err != nil {
		writeError(w, 500, fmt.Sprintf("restore extract failed: %v", err))
		return
	}

	writeJSON(w, 200, map[string]interface{}{"ok": true, "sandbox_id": id})
}

// ── JSON helpers ──────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, models.Response{Success: false, Message: msg})
}

// Ensure unused import is used.
var _ = models.Response{}
