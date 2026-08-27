package provisioner

import (
	"os"

	"github.com/docker/docker/client"
)

// podmanCompatSocket is Podman's default rootful Docker-compatible API
// socket. Rootless Podman uses a UID-scoped path instead
// (/run/user/<uid>/podman/podman.sock) — set DOCKER_HOST to override.
const podmanCompatSocket = "unix:///run/podman/podman.sock"

// podmanCompatHost resolves the socket address for a Podman-compat client:
// DOCKER_HOST wins when set (the escape hatch for rootless Podman, whose
// socket path is UID-dependent), else the default rootful socket.
func podmanCompatHost(dockerHostEnv string) string {
	if dockerHostEnv != "" {
		return dockerHostEnv
	}
	return podmanCompatSocket
}

// NewPodmanCompatClient builds a Docker SDK client pointed at Podman's
// Docker-compatible API socket, so the existing DockerBackend can drive
// Podman unchanged. Only a local Unix socket is supported (no TLS/remote
// Podman); ping happens later in NewDockerBackend.
func NewPodmanCompatClient() (*client.Client, error) {
	host := podmanCompatHost(os.Getenv("DOCKER_HOST"))
	return client.NewClientWithOpts(
		client.WithHost(host),
		client.WithAPIVersionNegotiation(),
	)
}
