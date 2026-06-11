package provisioner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tailLines is the last-N-lines helper behind both the healthgate
// failure capture and the /logs endpoint. A railpack/runtime tail can
// be thousands of lines; getting the slice math wrong either truncates
// the useful end or returns the whole firehose. Pin the edges.
func TestTailLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"n<=0 returns all", "a\nb\nc", 0, "a\nb\nc"},
		{"negative returns all", "a\nb\nc", -1, "a\nb\nc"},
		{"fewer lines than n", "a\nb", 10, "a\nb"},
		{"exactly n lines", "a\nb\nc", 3, "a\nb\nc"},
		{"more lines than n keeps tail", "a\nb\nc\nd\ne", 2, "d\ne"},
		{"trailing newline ignored in count", "a\nb\nc\n", 2, "b\nc"},
		{"single line", "only", 5, "only"},
		{"empty string", "", 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tailLines(tc.in, tc.n)
			// Compare on trimmed-right since the "want" for the all-cases
			// path keeps the original (which may carry a trailing \n).
			if strings.TrimRight(got, "\n") != strings.TrimRight(tc.want, "\n") {
				t.Errorf("tailLines(%q, %d) = %q; want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// The logs handler validates the {id} segment BEFORE touching docker, so
// a missing id is a clean 400 even with no daemon available — pin it so
// the cheap guard isn't reordered behind the docker dial.
func TestHandleDeploymentLogs_MissingID(t *testing.T) {
	p := &Provisioner{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/deployments/{id}/logs", p.handleDeploymentLogs)
	// An empty path segment can't be expressed through the mux, so call
	// the handler directly with no PathValue set — id resolves to "".
	r := httptest.NewRequest(http.MethodGet, "/api/deployments//logs", nil)
	w := httptest.NewRecorder()
	p.handleDeploymentLogs(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing id should 400; got %d (body=%s)", w.Code, w.Body.String())
	}
}
