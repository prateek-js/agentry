package provisioner

import "testing"

func TestIsMutableTag(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		// Mutable: :latest or no tag (Docker implies :latest).
		{"ghcr.io/agentry-ai/runtime:latest", true},
		{"agentry/runtime:latest", true},
		{"agentry/runtime", true},
		{"runtime", true},
		{"localhost:5000/runtime", true}, // registry port, no image tag
		// Stable: explicit version tag or digest pin.
		{"ghcr.io/agentry-ai/runtime:2026.06.17", false},
		{"agentry/runtime:v1.2.3", false},
		{"localhost:5000/runtime:stable", false},
		{"ghcr.io/agentry-ai/runtime@sha256:deadbeef", false},
		{"runtime:latest@sha256:deadbeef", false}, // digest wins
	}
	for _, c := range cases {
		if got := isMutableTag(c.ref); got != c.want {
			t.Errorf("isMutableTag(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}
