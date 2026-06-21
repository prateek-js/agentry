package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentry-ai/agentry/pkg/models"
)

// TestIsProtectedReadPath nails down the path-classification rules so
// later refactors can't loosen them by accident.
func TestIsProtectedReadPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/etc/sandbox/creds", true},
		{"/etc/sandbox/creds/", true},
		{"/etc/sandbox/creds/trino.json", true},
		{"/etc/sandbox/creds/aws/credentials", true},
		// Path traversal collapses — must still be blocked.
		{"/etc/sandbox/creds/../creds/trino.json", true},
		{"/etc/sandbox/creds/./aws/config", true},
		// Look-alikes that share a prefix but aren't under the mount.
		{"/etc/sandbox/creds-other", false},
		{"/etc/sandbox/credsfoo", false},
		{"/etc/sandbox", false},
		{"/workspace/creds.json", false},
		{"/tmp/secret", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isProtectedReadPath(tc.path); got != tc.want {
			t.Errorf("isProtectedReadPath(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// TestFileReadHandler_ProtectedCreds confirms the read handler refuses
// any path under /etc/sandbox/creds with 403, even when the file
// actually exists on disk — i.e. it's an authorization check, not a
// "file not found" leak.
func TestFileReadHandler_ProtectedCreds(t *testing.T) {
	// Build a real file under /etc/sandbox/creds-mirror to prove the
	// rejection is independent of the file's actual existence. We
	// can't create /etc/sandbox/creds in CI because of permissions,
	// so the test uses the canonical path and accepts that the
	// underlying ReadFile would fail with ENOENT — the 403 must
	// come first.
	req := models.FileReadRequest{File: "/etc/sandbox/creds/trino.json"}
	body, _ := json.Marshal(req)

	r := httptest.NewRequest(http.MethodPost, "/v1/file/read", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReadHandler(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want %d (body=%s)", w.Code, http.StatusForbidden, w.Body.String())
	}
	var resp models.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Success {
		t.Errorf("response.Success = true; want false")
	}
}

// TestFileReadHandler_NormalPathAllowed confirms the guard is scoped —
// a regular workspace file still reads through.
func TestFileReadHandler_NormalPathAllowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(models.FileReadRequest{File: path})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/read", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReadHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// decodeReadData pulls FileReadData out of a success response so
// individual tests don't have to repeat the same un-nesting.
func decodeReadData(t *testing.T, body []byte) models.FileReadData {
	t.Helper()
	var resp struct {
		Success bool                `json:"success"`
		Message string              `json:"message"`
		Data    models.FileReadData `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if !resp.Success {
		t.Fatalf("response not success: %s", body)
	}
	return resp.Data
}

func decodeReplaceData(t *testing.T, body []byte) models.FileReplaceData {
	t.Helper()
	var resp struct {
		Success bool                   `json:"success"`
		Data    models.FileReplaceData `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if !resp.Success {
		t.Fatalf("response not success: %s", body)
	}
	return resp.Data
}

// TestFileReadHandler_TotalLines asserts the new total_lines field
// is the FULL file's line count, even when a range was requested.
// Without this, the LLM can't tell whether the slice it got covered
// the whole file or just a window.
func TestFileReadHandler_TotalLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	content := "a\nb\nc\nd\ne\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mid := 2
	end := 4
	body, _ := json.Marshal(models.FileReadRequest{File: path, StartLine: &mid, EndLine: &end})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/read", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReadHandler(w, r)

	data := decodeReadData(t, w.Body.Bytes())
	if data.TotalLines != 5 {
		t.Errorf("TotalLines = %d; want 5", data.TotalLines)
	}
	if data.Content != "b\nc\nd" {
		t.Errorf("Content = %q; want %q", data.Content, "b\nc\nd")
	}
	if data.StartLine != 2 || data.EndLine != 4 {
		t.Errorf("Start/End = %d/%d; want 2/4", data.StartLine, data.EndLine)
	}
}

// TestFileReadHandler_NumberedFormat checks the cat -n output: 6-wide
// right-justified line number, tab, content. This is what the MCP
// layer sets by default so the LLM can refer to lines verbatim.
func TestFileReadHandler_NumberedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nl.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(models.FileReadRequest{File: path, Format: "numbered"})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/read", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReadHandler(w, r)

	data := decodeReadData(t, w.Body.Bytes())
	want := "     1\tfoo\n     2\tbar"
	if data.Content != want {
		t.Errorf("Content = %q; want %q", data.Content, want)
	}
}

// TestFileReplaceHandler_StrictDefaultRefusesMultiMatch is the
// load-bearing assertion for the inverted semantics — silent
// multi-clobber must error, not happen.
func TestFileReplaceHandler_StrictDefaultRefusesMultiMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	original := "foo bar\nfoo baz\nfoo qux\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(models.FileReplaceRequest{File: path, OldStr: "foo", NewStr: "FOO"})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/replace", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReplaceHandler(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422 (body=%s)", w.Code, w.Body.String())
	}
	// File must be untouched: the whole point is to NOT clobber.
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("file was modified despite refusal: %q", got)
	}
}

func TestFileReplaceHandler_ReplaceAllOptIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(path, []byte("foo foo foo"), 0o644); err != nil {
		t.Fatal(err)
	}

	yes := true
	body, _ := json.Marshal(models.FileReplaceRequest{
		File: path, OldStr: "foo", NewStr: "BAR", ReplaceAll: &yes,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/replace", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReplaceHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	data := decodeReplaceData(t, w.Body.Bytes())
	if data.ReplacedCount != 3 {
		t.Errorf("ReplacedCount = %d; want 3", data.ReplacedCount)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "BAR BAR BAR" {
		t.Errorf("file content = %q", got)
	}
}

// TestFileReplaceHandler_ExpectedMatchesMismatch lets the LLM declare
// "I expect exactly N matches" — and have the server confirm before
// touching the file. Wrong count → error; right count → succeed.
func TestFileReplaceHandler_ExpectedMatchesMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(path, []byte("a a a"), 0o644); err != nil {
		t.Fatal(err)
	}

	wrong := 2
	body, _ := json.Marshal(models.FileReplaceRequest{
		File: path, OldStr: "a", NewStr: "x", ExpectedMatches: &wrong,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/replace", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReplaceHandler(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422 (body=%s)", w.Code, w.Body.String())
	}
}

func TestFileReplaceHandler_UniqueMatchSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uniq.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(models.FileReplaceRequest{File: path, OldStr: "world", NewStr: "agentry"})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/replace", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileReplaceHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello agentry" {
		t.Errorf("content = %q", got)
	}
}

// TestFileMultiEditHandler_AppliesSequentially verifies each edit
// operates on the result of the previous, and the file is written
// atomically at the end.
func TestFileMultiEditHandler_AppliesSequentially(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package old\nimport \"old/pkg\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(models.FileMultiEditRequest{
		File: path,
		Edits: []models.FileEditStep{
			{OldStr: "package old", NewStr: "package new"},
			{OldStr: "\"old/pkg\"", NewStr: "\"new/pkg\""},
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/multi_edit", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileMultiEditHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(path)
	want := "package new\nimport \"new/pkg\"\n"
	if string(got) != want {
		t.Errorf("content = %q; want %q", got, want)
	}
}

// TestFileMultiEditHandler_RollsBackOnFailure: if any edit can't
// match, the whole call must fail and the file must be untouched —
// the atomic guarantee that makes batched edits safe.
func TestFileMultiEditHandler_RollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	original := "package old\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(models.FileMultiEditRequest{
		File: path,
		Edits: []models.FileEditStep{
			{OldStr: "package old", NewStr: "package new"},
			{OldStr: "definitely not present", NewStr: "x"},
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/multi_edit", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileMultiEditHandler(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 on partial failure, got 200 (body=%s)", w.Body.String())
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("file was modified despite partial failure: %q", got)
	}
}

// TestFileFindHandler_DoubleStarAgainstRelPath proves the bug from
// the LLM transcript is fixed: "**/*.py" must match files at any
// depth, not just files literally named "**/*.py".
func TestFileFindHandler_DoubleStarAgainstRelPath(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.py", "")
	mustWrite("sub/b.py", "")
	mustWrite("sub/deep/c.py", "")
	mustWrite("ignored.txt", "")

	body, _ := json.Marshal(models.FileFindRequest{Path: dir, Glob: "**/*.py"})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/find", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileFindHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data models.FileFindData `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data.Files) != 3 {
		t.Errorf("matched %d files; want 3 (got %v)", len(resp.Data.Files), resp.Data.Files)
	}
}

// TestFileFindHandler_BraceAlternation confirms doublestar handles
// the {ts,tsx,json} pattern the LLM keeps reaching for. Old
// filepath.Match silently returned nothing for this.
func TestFileFindHandler_BraceAlternation(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.ts")
	mustWrite("b.tsx")
	mustWrite("c.json")
	mustWrite("d.md")

	body, _ := json.Marshal(models.FileFindRequest{Path: dir, Glob: "*.{ts,tsx,json}"})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/find", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileFindHandler(w, r)

	var resp struct {
		Data models.FileFindData `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data.Files) != 3 {
		t.Errorf("matched %d files; want 3 (got %v)", len(resp.Data.Files), resp.Data.Files)
	}
}

// TestFileGrepHandler_MultiFileWithContext: multi-file regex sweep
// returns matches + context lines, structured.
func TestFileGrepHandler_MultiFileWithContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("a\nfindme\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("x\nfindme\nz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	one := 1
	body, _ := json.Marshal(models.FileGrepRequest{
		Path: dir, Regex: "findme", ContextBefore: &one, ContextAfter: &one,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/grep", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileGrepHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data models.FileGrepData `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data.Matches) != 2 {
		t.Fatalf("matches = %d; want 2", len(resp.Data.Matches))
	}
	for _, m := range resp.Data.Matches {
		if m.Line != 2 || m.Text != "findme" {
			t.Errorf("unexpected match: %+v", m)
		}
		if len(m.ContextBefore) != 1 || len(m.ContextAfter) != 1 {
			t.Errorf("context not populated: %+v", m)
		}
	}
}

// TestFileGrepHandler_SkipsBinaryFiles asserts the binary sniff —
// otherwise a sweep over a /workspace with a .git/objects/pack file
// would dump megabytes of noise into the response.
func TestFileGrepHandler_SkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	// Real text file with the pattern.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "Binary" file: contains a null byte in the first 8 KiB.
	bin := append([]byte("hello world"), 0x00, 0x01, 0x02)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(models.FileGrepRequest{Path: dir, Regex: "hello"})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/grep", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileGrepHandler(w, r)

	var resp struct {
		Data models.FileGrepData `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data.Matches) != 1 {
		t.Errorf("matches = %d; want 1 (binary file should be skipped)", len(resp.Data.Matches))
	}
	if len(resp.Data.Matches) > 0 && resp.Data.Matches[0].File == filepath.Join(dir, "blob.bin") {
		t.Errorf("binary file leaked into matches")
	}
}

// TestFileGrepHandler_MaxResultsCap: with a low cap, total_found is
// still the real count but matches is truncated and the flag is set.
func TestFileGrepHandler_MaxResultsCap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte("x\nx\nx\nx\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cap := 2
	body, _ := json.Marshal(models.FileGrepRequest{Path: dir, Regex: "x", MaxResults: &cap})
	r := httptest.NewRequest(http.MethodPost, "/v1/file/grep", bytes.NewReader(body))
	w := httptest.NewRecorder()
	FileGrepHandler(w, r)

	var resp struct {
		Data models.FileGrepData `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data.Matches) != 2 {
		t.Errorf("matches = %d; want 2 (capped)", len(resp.Data.Matches))
	}
	if resp.Data.TotalFound != 5 {
		t.Errorf("TotalFound = %d; want 5", resp.Data.TotalFound)
	}
	if !resp.Data.Truncated {
		t.Errorf("Truncated = false; want true")
	}
}
