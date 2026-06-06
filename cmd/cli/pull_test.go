package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `agentry pull` is the user-facing "give me my code" command. The
// load-bearing pieces are:
//
//   - extractStripOne: drops the leading workspace/ prefix on every
//     tar entry, refuses ../ escapes, recreates dirs + symlinks.
//   - runtimeArchiveCreate: emits the right POST body and propagates
//     non-2xx as a useful error.
//   - runtimeDownload: streams 2xx, surfaces non-2xx with body text.
//
// Each gets pinned below. The full cmdPull flow is covered indirectly:
// the pieces are deterministic, and the integration glue is one
// chain of three calls.

// extractStripOne is the trust boundary — it writes attacker-supplied
// paths to disk. The /workspace/ prefix must be stripped, the entries
// must land under dest, and a malicious "../etc/passwd" entry must be
// rejected instead of escaping.
func TestExtractStripOne_StripsWorkspacePrefix(t *testing.T) {
	tgz := buildTarGz(t, []tarEntry{
		{Name: "workspace/README.md", Body: "hello"},
		{Name: "workspace/src/main.go", Body: "package main"},
		{Name: "workspace/src/sub/", IsDir: true},
		{Name: "workspace/empty.txt", Body: ""},
	})
	dest := t.TempDir()
	files, _, err := extractStripOne(bytes.NewReader(tgz), dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if files != 3 {
		t.Errorf("files=%d; want 3 (README.md, main.go, empty.txt)", files)
	}
	mustRead(t, filepath.Join(dest, "README.md"), "hello")
	mustRead(t, filepath.Join(dest, "src/main.go"), "package main")
	if _, err := os.Stat(filepath.Join(dest, "workspace")); err == nil {
		t.Error("leading 'workspace/' segment not stripped")
	}
	if _, err := os.Stat(filepath.Join(dest, "src/sub")); err != nil {
		t.Errorf("empty dir not created: %v", err)
	}
}

// A tarball that names a parent-traversal entry would write outside
// dest if we didn't check. Refuse with an error; do not let any file
// be written.
func TestExtractStripOne_RejectsTraversal(t *testing.T) {
	tgz := buildTarGz(t, []tarEntry{
		// After strip-one this is "../etc/passwd" relative to dest.
		{Name: "workspace/../../etc/passwd", Body: "bad"},
	})
	dest := t.TempDir()
	_, _, err := extractStripOne(bytes.NewReader(tgz), dest)
	if err == nil {
		t.Fatal("expected error on ../-escaping entry")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error %v should mention escape", err)
	}
	// Nothing should have been written outside dest.
	if _, err := os.Stat("/tmp/passwd"); err == nil {
		t.Error("file written outside dest")
	}
}

func TestExtractStripOne_SymlinksPreserved(t *testing.T) {
	tgz := buildTarGzMixed(t, []tarMixedEntry{
		{Name: "workspace/target.txt", Body: "real"},
		{Name: "workspace/link.txt", Linkname: "target.txt"},
	})
	dest := t.TempDir()
	if _, _, err := extractStripOne(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	link, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != "target.txt" {
		t.Errorf("symlink target = %q; want target.txt", link)
	}
}

// runtimeArchiveCreate hits POST /api/sandboxes/{id}/runtime/v1/archive/create
// with body {"files":[<path>], "output":<tarpath>}. Pin the wire shape
// so an accidental rename of either field breaks here, not in prod.
func TestRuntimeArchiveCreate_WireShape(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := runtimeArchiveCreate(srv.Client(), srv.URL, "sb_demo", "/workspace", "/tmp/x.tgz",
		[]string{"node_modules", ".next"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if gotPath != "/api/sandboxes/sb_demo/runtime/v1/archive/create" {
		t.Errorf("path = %q", gotPath)
	}
	files, _ := gotBody["files"].([]any)
	if len(files) != 1 || files[0] != "/workspace" {
		t.Errorf("files = %v; want [/workspace]", gotBody["files"])
	}
	if gotBody["output"] != "/tmp/x.tgz" {
		t.Errorf("output = %v", gotBody["output"])
	}
	// Exclude patterns make it onto the wire — without this, the
	// runtime's tar runs without --exclude flags and a pull on a
	// 6 GB node_modules tree silently ships 6 GB to the laptop.
	excl, _ := gotBody["exclude"].([]any)
	if len(excl) != 2 || excl[0] != "node_modules" || excl[1] != ".next" {
		t.Errorf("exclude = %v; want [node_modules .next]", gotBody["exclude"])
	}
}

// Sanity-check the default exclude list. These are the patterns every
// pull ships by default; adding "src" or "package.json" here would
// silently drop the user's actual source code.
func TestDefaultPullExcludes_AreReproducibleArtifactsOnly(t *testing.T) {
	def := defaultPullExcludes()
	// Hard-required (the originally-reported issue): node_modules.
	got := strings.Join(def, " ")
	for _, must := range []string{"node_modules", ".next", "dist", "build", "__pycache__", ".venv"} {
		if !strings.Contains(got, must) {
			t.Errorf("default excludes missing %q (would not skip the reported bloat)", must)
		}
	}
	// Hard-banned: anything that would drop real source.
	for _, banned := range []string{"src", "package.json", "tsconfig.json", "next.config.mjs", ".sandbox-project.json"} {
		for _, p := range def {
			if p == banned {
				t.Errorf("default excludes contains %q — would drop the user's source", banned)
			}
		}
	}
}

// 5xx from the runtime — the user must see the error body, not just
// the status code. Without this they can't tell "tar failed because
// path doesn't exist" from "tar failed because disk full".
func TestRuntimeArchiveCreate_PropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "tar: /workspace: No such file or directory", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := runtimeArchiveCreate(srv.Client(), srv.URL, "sb", "/workspace", "/tmp/x.tgz", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Errorf("err %v should include server body", err)
	}
}

// runtimeDownload streams happy path; pin the path + query shape and
// that we get the bytes back unmangled. Failure mode below.
func TestRuntimeDownload_StreamsAndPropagates(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tarball-bytes"))
	}))
	defer srv.Close()

	rc, err := runtimeDownload(srv.Client(), srv.URL, "sb_demo", "/tmp/x.tgz")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "tarball-bytes" {
		t.Errorf("body = %q", got)
	}
	if gotPath != "/api/sandboxes/sb_demo/runtime/v1/file/download" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "file=/tmp/x.tgz") {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestRuntimeDownload_404PropagatesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such file", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := runtimeDownload(srv.Client(), srv.URL, "sb", "/tmp/nope.tgz")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error %v should include 'no such file'", err)
	}
}

// humanBytes is a small render but it's everywhere the user sees pull
// output; bad formatting reads sloppy. Pin the unit boundaries.
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{2 * 1024 * 1024, "2.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	}
	for _, c := range cases {
		got := humanBytes(c.in)
		if got != c.want {
			t.Errorf("humanBytes(%d) = %q; want %q", c.in, got, c.want)
		}
	}
}

// ── tarball helpers ────────────────────────────────────────────────

type tarEntry struct {
	Name  string
	Body  string
	IsDir bool
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name, Mode: 0o644}
		if e.IsDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !e.IsDir {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarMixedEntry struct {
	Name     string
	Body     string
	Linkname string // non-empty → symlink
}

func buildTarGzMixed(t *testing.T, entries []tarMixedEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name, Mode: 0o644}
		if e.Linkname != "" {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.Linkname
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.Linkname == "" {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustRead(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(raw) != want {
		t.Errorf("read %s = %q; want %q", path, raw, want)
	}
}
