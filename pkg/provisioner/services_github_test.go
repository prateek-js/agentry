package provisioner

import (
	"testing"
)

// The github manifest is load-bearing: the LLM's "work on existing
// repo" skill only kicks in when GITHUB_TOKEN is in the sandbox env,
// and that env var only appears after a successful service_bind. So
// the YAML's env-var names + required fields shouldn't drift quietly.
// Pin them here; a change to the manifest now has to update the test
// too, which forces the change-author to think about it.

func TestGitHubManifest_LoadsAndStampsKnownEnvVars(t *testing.T) {
	manifests := LoadEmbeddedServices()
	var m *ServiceManifest
	for i := range manifests {
		if manifests[i].Name == "github" {
			m = &manifests[i]
			break
		}
	}
	if m == nil {
		t.Fatal("github manifest missing from embedded services/")
	}

	if m.DisplayName != "GitHub" {
		t.Errorf("DisplayName = %q; want GitHub", m.DisplayName)
	}
	if m.Category != "developer" {
		t.Errorf("Category = %q; want developer (groups github alongside future dev-tool services)", m.Category)
	}

	// Required fields the bind form must prompt for. PAT and username
	// are mandatory. api_url is optional (GHE only).
	gotRequired := map[string]bool{}
	for _, f := range m.Fields {
		if f.Required {
			gotRequired[f.Name] = true
		}
	}
	for _, want := range []string{"token", "username"} {
		if !gotRequired[want] {
			t.Errorf("field %q should be required", want)
		}
	}
	if gotRequired["api_url"] {
		t.Errorf("field api_url should NOT be required (GHE-only)")
	}

	// env-var contract — these are what every GitHub SDK and `gh`
	// CLI read from process env. Changing the names breaks the skill
	// silently; pin them.
	for _, want := range []string{"GITHUB_TOKEN", "GITHUB_USERNAME", "GITHUB_API_URL"} {
		if _, ok := m.Inject.Env[want]; !ok {
			t.Errorf("inject.env missing %q (every GitHub SDK + gh CLI reads it)", want)
		}
	}

	// The token field should be marked secret AND have a pattern that
	// catches PATs from both old (ghp_) and fine-grained (github_pat_)
	// flavors. Wrong tokens would otherwise sail through validation
	// and break on first push with a confusing 403.
	var tokenField *ServiceField
	for i := range m.Fields {
		if m.Fields[i].Name == "token" {
			tokenField = &m.Fields[i]
			break
		}
	}
	if tokenField == nil {
		t.Fatal("token field missing")
	}
	if !tokenField.Secret {
		t.Errorf("token field should be marked secret (so the CLI prompts hidden)")
	}
	if tokenField.Pattern == "" {
		t.Errorf("token field should have a regex pattern to reject obvious typos")
	}
}

// TestCatalog_GitHubAppearsAsService confirms the manifest reaches
// the catalog as a kind=service entry — that's what
// `agentry service ls` reads and what the dashboard renders in its
// service-bind picker.
func TestCatalog_GitHubAppearsAsService(t *testing.T) {
	c := NewCatalog()
	if err := c.LoadDefault(); err != nil {
		t.Fatal(err)
	}
	entry := c.Find("service", "github", "")
	if entry == nil {
		t.Fatal("github not in catalog after LoadDefault")
	}
	if entry.Kind != "service" {
		t.Errorf("Kind = %q; want service", entry.Kind)
	}
	// Tag the category for catalog filtering.
	var hasDev bool
	for _, tg := range entry.Tags {
		if tg == "developer" {
			hasDev = true
		}
	}
	if !hasDev {
		t.Errorf("Tags missing 'developer'; got %v", entry.Tags)
	}
}
