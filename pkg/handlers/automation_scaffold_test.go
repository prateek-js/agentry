package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyTreeInto(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Build a small tree with artifacts that must be skipped.
	mustWrite(t, filepath.Join(src, "package.json"), `{"name":"x"}`)
	mustWrite(t, filepath.Join(src, "app", "page.tsx"), "export default null")
	mustWrite(t, filepath.Join(src, "node_modules", "pg", "index.js"), "module.exports={}")
	mustWrite(t, filepath.Join(src, ".next", "trace"), "junk")
	mustWrite(t, filepath.Join(src, ".git", "HEAD"), "ref: x")
	// A file that already exists at the destination must NOT be clobbered.
	mustWrite(t, filepath.Join(dst, "package.json"), "PRE-EXISTING")

	written, err := copyTreeInto(src, dst)
	if err != nil {
		t.Fatalf("copyTreeInto: %v", err)
	}

	if got := readFile(t, filepath.Join(dst, "package.json")); got != "PRE-EXISTING" {
		t.Errorf("clobbered existing file: got %q", got)
	}
	if readFile(t, filepath.Join(dst, "app", "page.tsx")) != "export default null" {
		t.Errorf("nested file not copied")
	}
	for _, skip := range []string{"node_modules/pg/index.js", ".next/trace", ".git/HEAD"} {
		if _, err := os.Stat(filepath.Join(dst, skip)); !os.IsNotExist(err) {
			t.Errorf("artifact %q should have been skipped", skip)
		}
	}
	// written must include the nested file but not the pre-existing one or artifacts.
	joined := strings.Join(written, "\n")
	if !strings.Contains(joined, "page.tsx") || strings.Contains(joined, "node_modules") {
		t.Errorf("written list wrong: %v", written)
	}
}

func TestBuildAutomationScaffoldMissingTemplate(t *testing.T) {
	// automationTemplateDir won't exist in CI; the scaffold must fail with
	// an actionable "older runtime" message rather than a generic error.
	if _, err := os.Stat(automationTemplateDir); err == nil {
		t.Skip("automation template present; skipping missing-template assertion")
	}
	_, err := buildProjectScaffold("x", "automation", nil, 0)
	if err == nil || !strings.Contains(err.Error(), "older runtime") {
		t.Fatalf("want 'older runtime' error, got %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
