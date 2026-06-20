package main

import (
	"path/filepath"
	"testing"
)

// Guards the account-switch fix: a config from a previous account must
// not leak its server pin (or PAT) into the next setup.

func TestConfigConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil", nil, false},
		{"empty", &Config{}, false},
		{"has app", &Config{AppURL: "https://app.agentry.run"}, true},
		{"has device", &Config{DeviceID: "my-mac"}, true},
		{"has token", &Config{APIToken: "tok"}, true},
		{"only server pin", &Config{Cluster: "hetzner-poc"}, true},
	}
	for _, tc := range cases {
		if got := tc.cfg.configured(); got != tc.want {
			t.Errorf("%s: configured()=%v want %v", tc.name, got, tc.want)
		}
	}
}

// logout must clear the account-scoped server pin, otherwise a later
// init/login for a different account inherits a stale `server:`.
func TestLogoutClearsServerPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentry.json")
	t.Setenv("AGENTRY_CONFIG", path)

	seed := &Config{
		AppURL:   "https://app.agentry.run",
		DeviceID: "my-mac",
		Cluster:  "hetzner-poc", // pin from the old account
		// no APIToken/AppURL-auth pairing → logout skips the network revoke
	}
	if err := seed.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	if rc := cmdLogout(nil); rc != 0 {
		t.Fatalf("cmdLogout rc=%d", rc)
	}

	got, _, err := LoadConfig()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Cluster != "" {
		t.Errorf("logout left stale server pin: %q", got.Cluster)
	}
}
