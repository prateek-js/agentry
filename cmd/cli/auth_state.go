package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AuthState is the on-disk record of a cluster + profile's auth
// posture: which DB binding the sidecar reads, the HMAC secret used
// to sign session cookies + bridge-stamped identity headers, and the
// OAuth providers the sidecar should surface on the login page.
//
// Stored at:
//
//	~/.agentry/auth/<cluster>/<profile>.json
//
// Separate top-level "auth/" tree so the namespace doesn't collide
// with the service-bind files under "services/" (which use one JSON
// per service). Also keeps the legacy → default-profile migration
// in profile.go from accidentally moving auth files.
//
// File mode 0600. Anyone with read access to the file gets the
// HMAC secret + every provider's client_secret, so don't soften this.
type AuthState struct {
	Enabled   bool                         `json:"enabled"`
	DBBinding string                       `json:"db_binding,omitempty"` // which service bind we'll point the sidecar at ("postgres", "mysql", "mongodb")
	Secret    string                       `json:"secret,omitempty"`     // 32-byte hex, used for HMAC signing
	Providers map[string]AuthProviderState `json:"providers,omitempty"`  // upstream OAuth providers (google, github, …)
}

// AuthProviderState is what we keep per OAuth provider. ClientID is
// readable in any logs; ClientSecret should never leave the JSON file
// except into a sandbox env (where the operator already trusts the
// runtime). Scopes are optional — when empty the sidecar uses the
// provider's defaults.
type AuthProviderState struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Scopes       []string `json:"scopes,omitempty"`
}

// authDir returns the per-cluster auth directory. Honours
// $AGENTRY_CONFIG the same way the rest of the CLI does.
func authDir(cluster string) string {
	if cluster == "" {
		return ""
	}
	base := filepath.Dir(ConfigPath())
	return filepath.Join(base, "auth", cluster)
}

// authFilePath is the canonical "where does this (cluster, profile)
// pair's auth state live" answer.
func authFilePath(cluster, profile string) string {
	if profile == "" {
		profile = defaultProfile
	}
	return filepath.Join(authDir(cluster), profile+".json")
}

// loadAuthState reads the on-disk state. Returns (nil, nil) when the
// file doesn't exist — "auth not enabled" is a valid state, not an
// error. Callers should treat nil-result as "auth.Enabled = false".
func loadAuthState(cluster, profile string) (*AuthState, error) {
	raw, err := os.ReadFile(authFilePath(cluster, profile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s AuthState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", authFilePath(cluster, profile), err)
	}
	return &s, nil
}

// saveAuthState writes atomically with 0600. Creates the parent dir
// with 0700 so the secret + provider client_secrets are scoped to the
// current user.
func saveAuthState(cluster, profile string, s *AuthState) error {
	if cluster == "" {
		return fmt.Errorf("cluster is empty")
	}
	if s == nil {
		return fmt.Errorf("auth state is nil")
	}
	dir := authDir(cluster)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := authFilePath(cluster, profile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// deleteAuthState removes the on-disk file. Missing file is not an
// error — the user wanted it gone, it's gone.
func deleteAuthState(cluster, profile string) error {
	err := os.Remove(authFilePath(cluster, profile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// mintAuthSecret generates the HMAC key used to sign session cookies
// + the bridge-pre-signed identity headers the sidecar trusts. 32
// random bytes (256 bits) hex-encoded — overkill for HMAC-SHA256 but
// the extra bytes cost nothing.
func mintAuthSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// authStateProviderEnv returns the env vars that the AGENTRY-driven
// PostCreateHook should stamp on every sandbox so the sidecar inside
// can read the provider config at startup. Always returns a fresh
// map; callers can mutate.
//
// Format: <PROVIDER>_CLIENT_ID and <PROVIDER>_CLIENT_SECRET, upper-
// cased — the standard convention oauth2-proxy + every other
// header-injection auth proxy reads.
func authStateProviderEnv(s *AuthState) map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	for name, p := range s.Providers {
		upper := upperEnvSafe(name)
		out[upper+"_CLIENT_ID"] = p.ClientID
		out[upper+"_CLIENT_SECRET"] = p.ClientSecret
	}
	return out
}

// upperEnvSafe converts a provider name like "google" or "github" to
// the env-var-safe shape used in stamped env vars. Hyphens become
// underscores; everything else passes through upper-cased.
func upperEnvSafe(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == ' ' {
			out = append(out, '_')
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
