package provisioner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/agentry-ai/agentry/pkg/errcode"
)

// CatalogEntry is the shared shape every catalog item has. Today
// every entry is Kind=service — dev_deps and skills were dropped when
// we moved to external-only services. Keeping the Kind field future-
// proofs the wire for skills (Phase: skill registry) without forcing
// another schema bump.
type CatalogEntry struct {
	Kind        string         `json:"kind"`        // "service"
	Name        string         `json:"name"`        // unique within Kind
	Version     string         `json:"version"`     // catalog-internal, opaque
	Description string         `json:"description"` // one-line for LLM steering
	Tags        []string       `json:"tags,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// ServiceExtra is the Extra payload for kind=service entries. It
// surfaces the rich manifest (fields, inject, get_started) so the CLI
// and dashboard can render bind forms without a second HTTP round
// trip — plus the flat list of env-var names (`env_vars`) for tooling
// that just wants to know what will be stamped into the sandbox.
type ServiceExtra struct {
	DisplayName  string         `json:"display_name,omitempty"`
	Category     string         `json:"category,omitempty"`
	EnvVars      []string       `json:"env_vars"`
	Fields       []ServiceField `json:"fields,omitempty"`
	Inject       *ServiceInject `json:"inject,omitempty"`
	GetStarted   string         `json:"get_started,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
}

// Catalog is the in-memory catalog the provisioner serves on
// GET /api/catalog. Mutable so hot-reload can swap without restart;
// reads through a RLock so the hot path doesn't contend.
type Catalog struct {
	mu      sync.RWMutex
	entries []CatalogEntry
}

// NewCatalog returns an empty Catalog. Use Load / LoadDefault to
// populate.
func NewCatalog() *Catalog { return &Catalog{} }

// Load replaces the catalog with the given entries.
func (c *Catalog) Load(entries []CatalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append([]CatalogEntry(nil), entries...)
}

// All returns a snapshot of every entry.
func (c *Catalog) All() []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CatalogEntry, len(c.entries))
	copy(out, c.entries)
	return out
}

// ByKind returns just the entries of the given kind.
func (c *Catalog) ByKind(kind string) []CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CatalogEntry, 0, len(c.entries))
	for _, e := range c.entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Find returns the entry matching kind + name + version. Version may
// be empty to match the first entry of that name. Returns nil if not
// found.
func (c *Catalog) Find(kind, name, version string) *CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.entries {
		e := &c.entries[i]
		if e.Kind != kind || e.Name != name {
			continue
		}
		if version == "" || e.Version == version {
			cp := *e
			return &cp
		}
	}
	return nil
}

// FindManifest returns the parsed ServiceManifest for one service. The
// catalog re-hydrates it from the entry's Extra payload so callers can
// run template substitution + field validation against a typed struct.
func (c *Catalog) FindManifest(name string) *ServiceManifest {
	e := c.Find("service", name, "")
	if e == nil {
		return nil
	}
	var ex ServiceExtra
	raw, _ := json.Marshal(e.Extra)
	_ = json.Unmarshal(raw, &ex)
	if ex.Inject == nil && len(ex.Fields) == 0 {
		return nil
	}
	m := ServiceManifest{
		Name:        e.Name,
		DisplayName: ex.DisplayName,
		Category:    ex.Category,
		Description: e.Description,
		Fields:      ex.Fields,
		GetStarted:  ex.GetStarted,
	}
	if ex.Inject != nil {
		m.Inject = *ex.Inject
	}
	return &m
}

// LoadDefault populates the catalog. Order of precedence (later wins):
//
//  1. Bundled — every YAML embedded under services/ in the binary.
//  2. Cluster-local — /etc/agentry/services/*.yaml on the host (env
//     SERVICES_DIR overrides the path).
//
// CATALOG_PATH (legacy JSON) is still respected as a full override —
// if it's set, only the JSON file is loaded. This is the operator's
// "nuke from orbit" knob; the YAML pipeline is the default path.
func (c *Catalog) LoadDefault() error {
	if path := strings.TrimSpace(os.Getenv("CATALOG_PATH")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read CATALOG_PATH=%s: %w", path, err)
		}
		var entries []CatalogEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		c.Load(entries)
		return nil
	}
	manifests := LoadEmbeddedServices()
	overrideDir := strings.TrimSpace(os.Getenv("SERVICES_DIR"))
	if overrideDir == "" {
		overrideDir = "/etc/agentry/services"
	}
	for _, m := range LoadOverrideServices(overrideDir) {
		// Override semantics: same name replaces bundled entry.
		manifests = mergeOverride(manifests, m)
	}
	entries := make([]CatalogEntry, 0, len(manifests))
	for _, m := range manifests {
		entries = append(entries, manifestToCatalogEntry(m))
	}
	c.Load(entries)
	return nil
}

func mergeOverride(in []ServiceManifest, m ServiceManifest) []ServiceManifest {
	for i := range in {
		if in[i].Name == m.Name {
			in[i] = m
			return in
		}
	}
	return append(in, m)
}

// manifestToCatalogEntry converts the YAML-shaped manifest into the
// generic CatalogEntry the legacy API surface speaks. The full
// manifest rides along in Extra so the dashboard can render forms
// without a second fetch.
func manifestToCatalogEntry(m ServiceManifest) CatalogEntry {
	tags := []string{m.Category}
	extra := ServiceExtra{
		DisplayName:  m.DisplayName,
		Category:     m.Category,
		EnvVars:      manifestEnvVars(m),
		Fields:       m.Fields,
		Inject:       &m.Inject,
		GetStarted:   m.GetStarted,
		Capabilities: m.Capabilities,
	}
	raw, _ := json.Marshal(extra)
	var extraMap map[string]any
	_ = json.Unmarshal(raw, &extraMap)
	return CatalogEntry{
		Kind:        "service",
		Name:        m.Name,
		Version:     "1",
		Description: strings.TrimSpace(m.Description),
		Tags:        tags,
		Extra:       extraMap,
	}
}

// handleCatalog is the HTTP handler for GET /api/catalog. Returns the
// catalog as a flat list. ?kind=service narrows.
func (p *Provisioner) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if p.catalog == nil {
		errcode.WriteJSON(w, errcode.New(errcode.Internal, "catalog not initialized"))
		return
	}
	kind := r.URL.Query().Get("kind")
	var entries []CatalogEntry
	if kind == "" {
		entries = p.catalog.All()
	} else {
		entries = p.catalog.ByKind(kind)
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "count": len(entries)})
}
