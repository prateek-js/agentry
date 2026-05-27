package provisioner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/agentry/agentry/pkg/errcode"
)

// CatalogEntry is the shared shape every catalog item has. Kind
// distinguishes services / dev-deps / skills; the rest of the fields
// are kind-specific via Extra.
type CatalogEntry struct {
	Kind        string         `json:"kind"`        // "service" | "dev_dep" | "skill"
	Name        string         `json:"name"`        // unique within Kind
	Version     string         `json:"version"`     // catalog-internal version, opaque string
	Description string         `json:"description"` // one-line for LLM steering
	Tags        []string       `json:"tags,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// ServiceExtra is the Extra payload for kind=service entries.
// Documents the env vars XDP will inject when this service is bound,
// so the LLM (and skills) know what to read at runtime.
type ServiceExtra struct {
	EnvVars []string `json:"env_vars"`        // canonical env names XDP injects (e.g. "TRINO_URL")
	Skills  []string `json:"skills,omitempty"` // related skills the LLM might want next
}

// DevDepExtra is the Extra payload for kind=dev_dep entries.
type DevDepExtra struct {
	Provides       string `json:"provides"`        // e.g. "postgres" — what kind of service the dev-dep simulates
	DefaultPort    int    `json:"default_port"`    // localhost port the dev-dep listens on
	StartupSeconds int    `json:"startup_seconds"` // typical time to become ready
}

// Catalog is the in-memory catalog the provisioner serves on
// GET /api/catalog. Mutable so a hot-reload can swap it without
// restarting; reads through a RLock so the hot path doesn't contend.
type Catalog struct {
	mu      sync.RWMutex
	entries []CatalogEntry
}

// NewCatalog returns an empty Catalog. Use Load or LoadDefault to
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
// be empty to match the first version of that name; explicit version
// must match exactly. Returns nil if not found.
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

// LoadDefault populates the catalog from a file or — if no file is
// configured — a baked default. CATALOG_PATH env overrides the file
// location, useful for operators to drop a cluster-specific catalog
// without rebuilding the binary.
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
	c.Load(defaultCatalog())
	return nil
}

// defaultCatalog is the placeholder set we ship until the real
// per-cluster catalogs land. Two services, two dev-deps, no skills
// yet (skills get wired in Phase 18). Skills slot in here with
// kind="skill" + Extra describing the content URL.
func defaultCatalog() []CatalogEntry {
	mustMarshalExtra := func(v any) map[string]any {
		raw, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m
	}
	return []CatalogEntry{
		{
			Kind:        "service",
			Name:        "trino",
			Version:     "1",
			Description: "Distributed SQL query engine. Reads typically free, writes governed by XDP RBAC.",
			Tags:        []string{"sql", "analytics", "lakehouse"},
			Extra: mustMarshalExtra(ServiceExtra{
				EnvVars: []string{"TRINO_URL", "TRINO_USER", "TRINO_PASSWORD", "TRINO_CATALOG"},
				Skills:  []string{},
			}),
		},
		{
			Kind:        "service",
			Name:        "spark",
			Version:     "1",
			Description: "Spark cluster for batch jobs. Submit jobs via SparkSubmit or programmatically via SPARK_MASTER.",
			Tags:        []string{"compute", "batch", "spark"},
			Extra: mustMarshalExtra(ServiceExtra{
				EnvVars: []string{"SPARK_MASTER", "SPARK_HISTORY_URL"},
				Skills:  []string{},
			}),
		},
		{
			Kind:        "dev_dep",
			Name:        "postgres",
			Version:     "16",
			Description: "Postgres 16 running locally in the sandbox. Use for dev when you don't need a real shared database.",
			Tags:        []string{"db", "sql"},
			Extra: mustMarshalExtra(DevDepExtra{
				Provides:       "postgres",
				DefaultPort:    5432,
				StartupSeconds: 3,
			}),
		},
		{
			Kind:        "dev_dep",
			Name:        "redis",
			Version:     "7",
			Description: "Redis 7 running locally in the sandbox.",
			Tags:        []string{"cache", "kv"},
			Extra: mustMarshalExtra(DevDepExtra{
				Provides:       "redis",
				DefaultPort:    6379,
				StartupSeconds: 1,
			}),
		},
	}
}

// handleCatalog is the HTTP handler for GET /api/catalog. Returns the
// catalog as a flat list. Query param ?kind=service filters.
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
