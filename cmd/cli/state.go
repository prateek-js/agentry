package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// State is per-machine ephemeral selection: which sandbox the user
// has "pinned" so commands like `agentry env` and `agentry service`
// don't need an explicit --sandbox every time.
//
// Lives at ~/.agentry/state.json. Separate from agentry.json so we
// can wipe it without losing enrollment + cert paths. The current
// cluster still lives on Config — it's set at enroll time and
// changing it is a deliberate act; current sandbox flips many times
// a day and belongs in cheaper-to-reset state.
type State struct {
	CurrentSandbox string `json:"current_sandbox,omitempty"`
}

// StatePath returns the canonical state path. $AGENTRY_STATE overrides.
func StatePath() string {
	if v := os.Getenv("AGENTRY_STATE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agentry-state.json"
	}
	return filepath.Join(home, ".agentry", "state.json")
}

// LoadState reads state.json. Missing file = zero-value State; we
// don't surface the "file doesn't exist yet" error because the steady
// state on a fresh machine is exactly that.
func LoadState() *State {
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		return &State{}
	}
	var s State
	_ = json.Unmarshal(raw, &s)
	return &s
}

// Save writes state.json atomically (write-tmp + rename). 0600.
func (s *State) Save() error {
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolveSandbox picks the sandbox id for a command:
//   - explicit --sandbox flag wins
//   - otherwise: state.current_sandbox
//   - otherwise: empty (caller decides whether that's an error)
func resolveSandbox(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return LoadState().CurrentSandbox
}
