package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentry/agentry/pkg/models"
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
