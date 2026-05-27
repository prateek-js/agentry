package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is what lives at ~/.agentry/agentry.json. Plain JSON keeps
// the dependency surface minimal; the file is small and
// human-editable.
//
// Set on `agentry init`:
//   - AppURL: the control plane the laptop enrolled against
//   - BrokerURL: dial target for the bridge (returned by enroll)
//   - DeviceID: the laptop's chosen name (hostname-ish)
//   - DeviceCertPath / DeviceKeyPath / CACertPath: cert bundle
//
// Set on `agentry cluster use`:
//   - Cluster: which X-Cluster value subsequent commands stamp
type Config struct {
	AppURL    string `json:"app_url"`
	BrokerURL string `json:"broker_url"`
	DeviceID  string `json:"device_id"`
	Cluster   string `json:"cluster,omitempty"`

	// mTLS material for the bridge tunnel.
	DeviceCertPath string `json:"device_cert_path,omitempty"`
	DeviceKeyPath  string `json:"device_key_path,omitempty"`
	CACertPath     string `json:"ca_cert_path,omitempty"`
}

// ConfigPath returns the canonical path. Uses $AGENTRY_CONFIG to
// override for tests / multi-profile setups; defaults to
// ~/.agentry/agentry.json.
func ConfigPath() string {
	if v := os.Getenv("AGENTRY_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agentry.json"
	}
	return filepath.Join(home, ".agentry", "agentry.json")
}

// LoadConfig reads the config file. Returns the parsed config + the
// path it was read from + any error. Path is returned even on error
// so the caller can include it in user-facing messages.
func LoadConfig() (*Config, string, error) {
	path := ConfigPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, path, nil
}

// Save writes the config back. Creates the parent dir with 0700, the
// file with 0600. Atomic via rename.
func (c *Config) Save() error {
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// NewDeviceID returns a random 16-hex identifier (~64 bits of
// entropy). Used when the user doesn't pass --name. Mostly cosmetic
// — the cert CN is what the bridge actually trusts.
func NewDeviceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	host, _ := os.Hostname()
	if host == "" {
		host = "device"
	}
	return host + "-" + hex.EncodeToString(b[:])
}
