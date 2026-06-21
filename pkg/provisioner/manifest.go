package provisioner

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServiceManifest is the YAML shape every service in the catalog
// declares. One file per service under services/ at the repo root
// (embedded into the binary at build time) plus optional
// /etc/agentry/services/*.yaml overrides on the cluster.
//
// The catalog is opt-in extensible: drop a YAML file in the override
// directory, restart the provisioner, the service is available. No
// code changes required — that's the convention promise.
type ServiceManifest struct {
	Name        string         `yaml:"name" json:"name"`
	DisplayName string         `yaml:"display_name" json:"display_name"`
	Category    string         `yaml:"category" json:"category"`
	Description string         `yaml:"description" json:"description"`
	Fields      []ServiceField `yaml:"fields" json:"fields"`
	Inject      ServiceInject  `yaml:"inject" json:"inject"`
	GetStarted  string         `yaml:"get_started,omitempty" json:"get_started,omitempty"`

	// Capabilities are coarse feature tags a binding unlocks downstream —
	// e.g. smtp → ["email"]. The authproxy lights up password reset +
	// verification from SMTP env directly; this tag is what lets
	// `agentry auth enable` and the catalog UI tell the operator which
	// auth features a given bind enables. Optional; most services have none.
	Capabilities []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
}

// ServiceField describes one input the user must supply when binding
// the service. Drives both CLI prompts and dashboard forms.
type ServiceField struct {
	Name         string `yaml:"name" json:"name"`
	Label        string `yaml:"label" json:"label"`
	Placeholder  string `yaml:"placeholder,omitempty" json:"placeholder,omitempty"`
	Default      string `yaml:"default,omitempty" json:"default,omitempty"`
	Secret       bool   `yaml:"secret,omitempty" json:"secret,omitempty"`
	Required     bool   `yaml:"required,omitempty" json:"required,omitempty"`
	ProdRequired bool   `yaml:"prod_required,omitempty" json:"prod_required,omitempty"`
	Pattern      string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
}

// ServiceInject is what gets stamped into the sandbox when the
// binding succeeds. Env keys land in the sandbox's process env (and
// /etc/profile.d/agentry-services.sh for shell sessions). Creds land
// as files at the given absolute paths.
//
// Both env values and cred file contents support {field-name}
// interpolation against the user-supplied field map. Env KEYS also
// interpolate so a field can decide the variable name (see http-api).
type ServiceInject struct {
	Env   map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Creds map[string]string `yaml:"creds,omitempty" json:"creds,omitempty"`
}

//go:embed services/*.yaml
var embeddedServices embed.FS

// LoadEmbeddedServices reads every YAML in the embedded services/
// directory and returns them as parsed manifests. Files that fail
// to parse are skipped with a logged warning rather than aborting
// startup — one bad file shouldn't lose the whole catalog.
func LoadEmbeddedServices() []ServiceManifest {
	return loadServicesFromFS(embeddedServices, "services")
}

// LoadOverrideServices reads YAMLs from a cluster-local directory.
// Empty path or missing directory returns nil — the override is
// genuinely optional. Useful when an operator wants to ship a service
// definition specific to their environment without rebuilding.
func LoadOverrideServices(dir string) []ServiceManifest {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return loadServicesFromFS(os.DirFS(dir), ".")
}

func loadServicesFromFS(fsys fs.FS, root string) []ServiceManifest {
	var out []ServiceManifest
	_ = fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil
		}
		var m ServiceManifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			fmt.Fprintf(os.Stderr, "services: skip %s: %v\n", path, err)
			return nil
		}
		if m.Name == "" {
			fmt.Fprintf(os.Stderr, "services: skip %s: missing name\n", path)
			return nil
		}
		out = append(out, m)
		return nil
	})
	return out
}

// manifestEnvVars extracts the env var names this manifest will inject.
// Used to populate the legacy ServiceExtra.EnvVars list so existing
// tooling that reads the catalog by env-var names keeps working.
//
// Names that contain {placeholder} (e.g. http-api uses "{env_prefix}_URL")
// are kept verbatim — the CLI/dashboard knows to resolve them against
// supplied field values. Consumers that want the canonical list filter
// out the ones with `{`.
func manifestEnvVars(m ServiceManifest) []string {
	out := make([]string, 0, len(m.Inject.Env))
	for k := range m.Inject.Env {
		out = append(out, k)
	}
	return out
}
