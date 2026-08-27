package provisioner

import "testing"

func TestPodmanCompatHost(t *testing.T) {
	if got := podmanCompatHost(""); got != podmanCompatSocket {
		t.Errorf("podmanCompatHost(\"\") = %q, want %q (default rootful socket)", got, podmanCompatSocket)
	}
	const override = "unix:///run/user/1000/podman/podman.sock"
	if got := podmanCompatHost(override); got != override {
		t.Errorf("podmanCompatHost(%q) = %q, want %q (DOCKER_HOST wins)", override, got, override)
	}
}
