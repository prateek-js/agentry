package provisioner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCatalogLoadsDefault(t *testing.T) {
	c := NewCatalog()
	if err := c.LoadDefault(); err != nil {
		t.Fatal(err)
	}
	all := c.All()
	if len(all) == 0 {
		t.Fatal("default catalog should have entries")
	}
	// Spot-check: trino service present with the canonical env vars.
	tr := c.Find("service", "trino", "")
	if tr == nil {
		t.Fatal("trino entry missing from default catalog")
	}
	extra, _ := tr.Extra["env_vars"].([]any)
	if len(extra) == 0 {
		t.Errorf("trino entry missing env_vars; got %+v", tr.Extra)
	}
}

func TestCatalogByKind(t *testing.T) {
	c := NewCatalog()
	_ = c.LoadDefault()

	services := c.ByKind("service")
	devDeps := c.ByKind("dev_dep")
	if len(services) == 0 || len(devDeps) == 0 {
		t.Errorf("expected at least one service and one dev_dep; got %d/%d",
			len(services), len(devDeps))
	}
	for _, e := range services {
		if e.Kind != "service" {
			t.Errorf("ByKind('service') returned %q", e.Kind)
		}
	}
}

func TestCatalogFindByVersion(t *testing.T) {
	c := NewCatalog()
	c.Load([]CatalogEntry{
		{Kind: "service", Name: "trino", Version: "1"},
		{Kind: "service", Name: "trino", Version: "2"},
	})
	if e := c.Find("service", "trino", "2"); e == nil || e.Version != "2" {
		t.Errorf("Find(version=2) got %+v", e)
	}
	if e := c.Find("service", "trino", ""); e == nil || e.Version != "1" {
		t.Errorf("Find(version='') should return first; got %+v", e)
	}
	if e := c.Find("service", "trino", "99"); e != nil {
		t.Errorf("Find(version=99) should miss; got %+v", e)
	}
}

func TestCatalogPathOverride(t *testing.T) {
	// Write a custom catalog to a temp file, point CATALOG_PATH at it,
	// verify LoadDefault picks it up over the baked default.
	tmp := t.TempDir() + "/catalog.json"
	custom := []CatalogEntry{
		{Kind: "service", Name: "only-this-one", Version: "1", Description: "custom"},
	}
	raw, _ := json.Marshal(custom)
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CATALOG_PATH", tmp)

	c := NewCatalog()
	if err := c.LoadDefault(); err != nil {
		t.Fatal(err)
	}
	all := c.All()
	if len(all) != 1 || all[0].Name != "only-this-one" {
		t.Errorf("CATALOG_PATH override didn't take effect; got %+v", all)
	}
}

func TestCatalogHandlerReturnsAll(t *testing.T) {
	p := NewWithKey(Config{Namespace: "test"}, NewMockBackend(), "")
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/catalog")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Entries []CatalogEntry `json:"entries"`
		Count   int            `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Count == 0 || len(out.Entries) != out.Count {
		t.Errorf("unexpected response shape: %+v", out)
	}
}

func TestCatalogHandlerFiltersByKind(t *testing.T) {
	p := NewWithKey(Config{Namespace: "test"}, NewMockBackend(), "")
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/catalog?kind=service")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Entries []CatalogEntry `json:"entries"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	for _, e := range out.Entries {
		if e.Kind != "service" {
			t.Errorf("?kind=service returned non-service entry: %+v", e)
		}
	}
	if len(out.Entries) == 0 {
		t.Errorf("?kind=service returned no entries")
	}
}
