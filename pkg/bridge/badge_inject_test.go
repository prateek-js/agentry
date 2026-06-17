package bridge

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func htmlResp(status int, ct, body string) *http.Response {
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func readBody(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestInjectBadge_BeforeBodyClose(t *testing.T) {
	resp := htmlResp(200, "text/html; charset=utf-8", "<html><body><h1>hi</h1></body></html>")
	injectBuiltWithBadge(resp)
	out := readBody(t, resp)
	if !strings.Contains(out, "Built with agentry") {
		t.Fatal("badge not injected")
	}
	if strings.Index(out, "Built with agentry") > strings.Index(out, "</body>") {
		t.Fatal("badge must come before </body>")
	}
	if got := resp.Header.Get("Content-Length"); got != "" && got == "37" {
		t.Fatalf("Content-Length not updated after injection: %s", got)
	}
}

func TestInjectBadge_AppendsWhenNoBodyTag(t *testing.T) {
	resp := htmlResp(200, "text/html", "<div>fragment</div>")
	injectBuiltWithBadge(resp)
	out := readBody(t, resp)
	if !strings.HasSuffix(out, "</div>") && !strings.Contains(out, "Built with agentry") {
		t.Fatal("expected badge appended")
	}
	if !strings.Contains(out, "Built with agentry") {
		t.Fatal("badge not appended when </body> absent")
	}
}

func TestInjectBadge_SkipsNonHTMLAndErrors(t *testing.T) {
	// non-HTML
	r := htmlResp(200, "application/json", `{"ok":true}`)
	injectBuiltWithBadge(r)
	if strings.Contains(readBody(t, r), "Built with agentry") {
		t.Fatal("must not inject into JSON")
	}
	// non-2xx
	r = htmlResp(404, "text/html", "<body>nope</body>")
	injectBuiltWithBadge(r)
	if strings.Contains(readBody(t, r), "Built with agentry") {
		t.Fatal("must not inject into a 404")
	}
	// still-compressed
	r = htmlResp(200, "text/html", "<body>x</body>")
	r.Header.Set("Content-Encoding", "gzip")
	injectBuiltWithBadge(r)
	if strings.Contains(readBody(t, r), "Built with agentry") {
		t.Fatal("must not inject into a compressed body")
	}
}
