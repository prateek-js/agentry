// Package provisioner manages sandbox Pod/Service lifecycle in Kubernetes.
package provisioner

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// BackendKind identifies which sandbox runtime the provisioner targets.
type BackendKind string

const (
	BackendK8s    BackendKind = "k8s"
	BackendDocker BackendKind = "docker"
)

// Config holds provisioner configuration from environment variables.
type Config struct {
	Backend        BackendKind
	Namespace      string
	SandboxImage   string
	NodeHost       string
	KubeconfigPath string
	ListenAddr     string
	Labels         map[string]string

	// ReaperInterval is how often the TTL reaper sweeps for expired
	// sandboxes. 0 disables the reaper entirely.
	ReaperInterval time.Duration

	// DefaultVolumes are appended to every CreateRequest's Volumes
	// before validation. Used to inject baseline mounts that should be
	// present in every sandbox — most commonly the host credentials
	// directory (set via SANDBOX_DEFAULT_CREDS_DIR).
	//
	// Volumes a caller explicitly specifies with the SAME name or
	// mount_path take precedence; the default is dropped to avoid
	// duplicate-mount validation errors.
	DefaultVolumes []Volume

	// DefaultEgress is the policy applied to CreateRequests that don't
	// supply their own. Zero-value = no policy (default-allow at the
	// host firewall). Set via SANDBOX_DEFAULT_EGRESS_MODE plus the
	// rule env vars; see defaultEgressFromEnv.
	DefaultEgress EgressPolicy

	// RuntimeAPIKey is the X-Sandbox-API-Key the provisioner stamps on
	// direct (non-tunneled) calls to the runtime container — e.g. when
	// writing service-binding files into /etc/sandbox/creds/xdp/. Empty
	// means "runtime accepts unauthed calls", which is the dev posture.
	// Set via SANDBOX_RUNTIME_API_KEY.
	RuntimeAPIKey string

	// BridgeURL, when non-empty, makes the provisioner phone home to
	// the agentry bridge over an mTLS tunnel on startup. The local
	// HTTP listener stays up regardless so direct ops + tests still
	// work. Set via AGENTRY_BRIDGE_URL (or filled in automatically
	// from the enroll response's bridge_url field).
	BridgeURL string

	// ClusterID is the bridge-side identifier this cluster registers
	// under. Devices use it as the X-Cluster header value when they
	// want to address this cluster. Required when BridgeURL is set;
	// ignored otherwise. Set via AGENTRY_CLUSTER_NAME.
	ClusterID string

	// CertDir is where the provisioner persists its enrolled mTLS
	// bundle (cluster.crt, cluster.key, ca.crt). The directory + files
	// are owned by the provisioner user, mode 0700/0600. Empty disables
	// the cert flow — useful for local dev. Set via AGENTRY_CERT_DIR.
	CertDir string

	// EnrollURL is the control plane's /api/v1/enroll endpoint.
	// Single source of truth for first-boot enrollment + cert renewal.
	// Set via AGENTRY_ENROLL_URL.
	EnrollURL string

	// EnrollToken is the single-use bootstrap token minted by the
	// dashboard at cluster-create time. Consumed by the control plane
	// on first success; ignored once a valid cert is already on disk.
	// Set via AGENTRY_ENROLL_TOKEN.
	EnrollToken string

	// DefaultShmBytes is the size of /dev/shm inside every sandbox.
	// Docker's default is 64 MiB, which is the most common cause of
	// "Bus error" / "No space left on device" failures in pandas,
	// matplotlib, headless Chromium, and multiprocess Python workers.
	// 0 disables the override (Docker default kicks in).
	// Override via SANDBOX_DEFAULT_SHM_SIZE (e.g. "2Gi", "4Gi").
	DefaultShmBytes int64
}

// DefaultConfig loads configuration from environment with sensible defaults.
//
// Backend selection:
//
//	BACKEND=k8s     (default) — provision Pods + Services in Kubernetes
//	BACKEND=docker            — provision containers via the local Docker daemon
func DefaultConfig() Config {
	backend := BackendKind(envOr("BACKEND", string(BackendK8s)))
	// For Docker, "localhost" is the sensible default — clients call
	// the provisioner from the same host where containers run.
	defaultHost := "host.docker.internal"
	if backend == BackendDocker {
		defaultHost = "localhost"
	}
	return Config{
		Backend:        backend,
		Namespace:      envOr("K8S_NAMESPACE", "default"),
		SandboxImage:   envOr("SANDBOX_IMAGE", "agentry/runtime:latest"),
		NodeHost:       envOr("NODE_HOST", defaultHost),
		KubeconfigPath: envOr("KUBECONFIG_PATH", os.ExpandEnv("$HOME/.kube/config")),
		ListenAddr:     envOr("PROVISIONER_ADDR", ":8002"),
		Labels: map[string]string{
			"app":                          "agentry-sandbox",
			"app.kubernetes.io/name":       "agentry",
			"app.kubernetes.io/component":  "sandbox",
			"app.kubernetes.io/managed-by": "agentry-provisioner",
		},
		ReaperInterval:  envDuration("REAPER_INTERVAL_SECONDS", 60*time.Second),
		DefaultVolumes:  defaultVolumesFromEnv(),
		DefaultEgress:   defaultEgressFromEnv(),
		DefaultShmBytes: defaultShmBytesFromEnv(),
		BridgeURL:       os.Getenv("AGENTRY_BRIDGE_URL"),
		ClusterID:       os.Getenv("AGENTRY_CLUSTER_NAME"),
		CertDir:         os.Getenv("AGENTRY_CERT_DIR"),
		EnrollURL:       os.Getenv("AGENTRY_ENROLL_URL"),
		EnrollToken:     os.Getenv("AGENTRY_ENROLL_TOKEN"),
		RuntimeAPIKey:   os.Getenv("AGENTRY_RUNTIME_API_KEY"),
	}
}

// defaultEgressFromEnv builds the baseline egress policy from environment.
// All knobs are optional; an empty mode means "no policy".
//
//	SANDBOX_DEFAULT_EGRESS_MODE   = "allow" | "deny"
//	SANDBOX_DEFAULT_EGRESS_CIDRS  = "10.0.0.0/8,192.168.0.0/16"
//	SANDBOX_DEFAULT_EGRESS_PORTS  = "80,443"   (applied to every CIDR)
//	SANDBOX_DEFAULT_EGRESS_PROTO  = "tcp" | "udp" | "" (both)
//
// In allow-mode the rules express explicit blocks; in deny-mode they
// express explicit allows. (Both modes share the rule shape; the mode
// flips the verdict at render time.) An unparseable cidr or port logs
// and drops that entry rather than failing startup — operators tweak
// these without restarting the cluster.
func defaultEgressFromEnv() EgressPolicy {
	mode := EgressMode(os.Getenv("SANDBOX_DEFAULT_EGRESS_MODE"))
	if mode == "" {
		return EgressPolicy{}
	}
	rule := EgressRule{Proto: os.Getenv("SANDBOX_DEFAULT_EGRESS_PROTO")}
	for _, c := range splitCSV(os.Getenv("SANDBOX_DEFAULT_EGRESS_CIDRS")) {
		rule.CIDR = c
		break // first CIDR seeds the rule; extras get their own EgressRule
	}
	ports := splitCSV(os.Getenv("SANDBOX_DEFAULT_EGRESS_PORTS"))
	for _, p := range ports {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			log.Printf("provisioner: SANDBOX_DEFAULT_EGRESS_PORTS skipping %q: not a valid port", p)
			continue
		}
		rule.Ports = append(rule.Ports, n)
	}

	var rules []EgressRule
	cidrs := splitCSV(os.Getenv("SANDBOX_DEFAULT_EGRESS_CIDRS"))
	for _, c := range cidrs {
		r := rule
		r.CIDR = c
		rules = append(rules, r)
	}

	pol := EgressPolicy{Mode: mode, Rules: rules}
	if err := pol.Validate(); err != nil {
		log.Printf("provisioner: SANDBOX_DEFAULT_EGRESS invalid, ignoring: %v", err)
		return EgressPolicy{}
	}
	return pol
}

// splitCSV splits "a,b, c" into ["a","b","c"], dropping empties and trimming
// whitespace. Lives next to the env helpers so the per-call allocation is
// obvious in profiles.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// defaultShmBytesFromEnv parses SANDBOX_DEFAULT_SHM_SIZE (e.g. "2Gi", "512Mi",
// "1073741824") into a byte count. Empty / unparsable / "0" → 0 (no override,
// Docker's 64 MiB default applies). Sandbox workloads typically want at least
// 2 GiB so pandas/matplotlib/Chromium don't hit "Bus error" on /dev/shm.
func defaultShmBytesFromEnv() int64 {
	v := os.Getenv("SANDBOX_DEFAULT_SHM_SIZE")
	if v == "" {
		// Sensible default for sandbox workloads. Operators can set
		// the env var to "0" to opt out and get Docker's 64 MiB.
		v = "2Gi"
	}
	if v == "0" {
		return 0
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0
	}
	return q.Value()
}

// defaultVolumesFromEnv builds the baseline volume list from environment.
//
// The only knob today is SANDBOX_DEFAULT_CREDS_DIR — when set to a host
// directory that EXISTS, it's bind-mounted read-only into every sandbox
// at /etc/sandbox/creds (the path /etc/profile.d/sandbox-creds.sh +
// build-image both look for).
//
// If the env var is unset, OR set to a path that doesn't exist, we
// emit no default volume. The "set but missing" case is logged so the
// operator gets feedback without sandbox creation failing hard on a
// Docker bind-mount error.
func defaultVolumesFromEnv() []Volume {
	credsDir := os.Getenv("SANDBOX_DEFAULT_CREDS_DIR")
	if credsDir == "" {
		return nil
	}
	info, err := os.Stat(credsDir)
	if err != nil || !info.IsDir() {
		log.Printf("provisioner: SANDBOX_DEFAULT_CREDS_DIR=%q does not exist (or is not a directory); skipping default creds mount", credsDir)
		return nil
	}
	return []Volume{{
		Name:      "sandbox-creds",
		MountPath: "/etc/sandbox/creds",
		ReadOnly:  true,
		HostPath:  &HostPathSource{Path: credsDir, Type: "Directory"},
	}}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envDuration reads an integer-seconds env var; a value of "0" disables the
// associated feature, and an unset/invalid value yields the fallback.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return time.Duration(n) * time.Second
}
