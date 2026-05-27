package runtime

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile drops `content` at a tempfile under t.TempDir and returns
// its absolute path.
func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDownloadFullFile(t *testing.T) {
	path := writeTestFile(t, "x.txt", "hello, world\n")
	ts := newTestServer(t, "")

	resp, err := http.Get(ts.URL + "/v1/file/download?file=" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q; want bytes", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello, world\n" {
		t.Errorf("body = %q", body)
	}
}

func TestDownloadRange(t *testing.T) {
	path := writeTestFile(t, "data.bin", "0123456789ABCDEF")
	ts := newTestServer(t, "")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/file/download?file="+path, nil)
	req.Header.Set("Range", "bytes=4-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d; want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 4-9/16" {
		t.Errorf("Content-Range = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "456789" {
		t.Errorf("body = %q; want 456789", body)
	}
}

func TestDownloadOpenSuffix(t *testing.T) {
	// "bytes=-5" means "the last 5 bytes" — http.ServeContent handles it.
	path := writeTestFile(t, "data.bin", "0123456789")
	ts := newTestServer(t, "")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/file/download?file="+path, nil)
	req.Header.Set("Range", "bytes=-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "789" {
		t.Errorf("body = %q; want 789", body)
	}
}

func TestDownloadInvalidRange(t *testing.T) {
	path := writeTestFile(t, "x.txt", "abc")
	ts := newTestServer(t, "")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/file/download?file="+path, nil)
	req.Header.Set("Range", "bytes=100-200")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d; want 416", resp.StatusCode)
	}
}

func TestDownloadValidationErrors(t *testing.T) {
	ts := newTestServer(t, "")
	cases := []struct {
		name string
		url  string
		want int
	}{
		{"missing file param", "/v1/file/download", 400},
		{"relative path", "/v1/file/download?file=relative.txt", 400},
		{"nonexistent", "/v1/file/download?file=/nope/does/not/exist", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.url)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d; want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestDownloadRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	ts := newTestServer(t, "")
	resp, err := http.Get(ts.URL + "/v1/file/download?file=" + dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
}

// multipartRequest builds a multipart/form-data body with a metadata JSON
// part and a file part. Useful in every upload test.
func multipartRequest(t *testing.T, ts *httptest.Server, metadata string, fileBody []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if metadata != "" {
		if err := mw.WriteField("metadata", metadata); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("file", "payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(fileBody); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/file/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestUploadHappyPath(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.bin")
	ts := newTestServer(t, "")

	body := bytes.Repeat([]byte{0x41}, 4096)
	metadata := fmt.Sprintf(`{"path":%q,"mode":"0640","overwrite":true}`, dest)

	resp := multipartRequest(t, ts, metadata, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("file contents differ (len got=%d want=%d)", len(got), len(body))
	}
	info, _ := os.Stat(dest)
	if mode := info.Mode().Perm(); mode != 0o640 {
		t.Errorf("mode = %#o; want 0640", mode)
	}
}

func TestUploadRefusesOverwriteByDefault(t *testing.T) {
	dest := writeTestFile(t, "existing.txt", "old")
	ts := newTestServer(t, "")
	metadata := fmt.Sprintf(`{"path":%q}`, dest)

	resp := multipartRequest(t, ts, metadata, []byte("new"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d; want 409", resp.StatusCode)
	}
	// Original content must be untouched.
	got, _ := os.ReadFile(dest)
	if string(got) != "old" {
		t.Errorf("existing file was modified: %q", got)
	}
}

func TestUploadCreatesParentDirs(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "nested", "dir", "out.txt")
	ts := newTestServer(t, "")
	metadata := fmt.Sprintf(`{"path":%q}`, dest)

	resp := multipartRequest(t, ts, metadata, []byte("hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file missing: %v", err)
	}
}

func TestUploadRequiresPath(t *testing.T) {
	ts := newTestServer(t, "")
	resp := multipartRequest(t, ts, "", []byte("hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestUploadRejectsRelativePath(t *testing.T) {
	ts := newTestServer(t, "")
	resp := multipartRequest(t, ts, `{"path":"relative.txt"}`, []byte("hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestUploadRejectsBadJSON(t *testing.T) {
	ts := newTestServer(t, "")
	resp := multipartRequest(t, ts, `{not json}`, []byte("hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestUploadRejectsBadMode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.txt")
	ts := newTestServer(t, "")
	resp := multipartRequest(t, ts,
		fmt.Sprintf(`{"path":%q,"mode":"twelve"}`, dest),
		[]byte("hi"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestUploadNoFilePart(t *testing.T) {
	ts := newTestServer(t, "")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("metadata", `{"path":"/tmp/x"}`)
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/file/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "file") {
		t.Errorf("body missing 'file' hint: %s", body)
	}
}

func TestUploadAcceptsQueryParamPath(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "via-query.txt")
	ts := newTestServer(t, "")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "x")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/v1/file/upload?path="+dest, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "hello" {
		t.Errorf("contents = %q", got)
	}
}
