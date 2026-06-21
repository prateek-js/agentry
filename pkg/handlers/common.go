// Package handlers implements HTTP request handlers for the sandbox runtime API.
package handlers

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/agentry-ai/agentry/pkg/models"
)

// credsMountPath is the in-sandbox directory where the host mounts
// credentials (Trino, AWS, XDP, …). Apps that need the creds read
// the JSON files directly via os.ReadFile — the HTTP API refuses
// reads/searches/replaces under this path so an agent driving the
// runtime over HTTP can't exfiltrate tokens by name.
const credsMountPath = "/etc/sandbox/creds"

// isProtectedReadPath reports whether the given path (after cleaning)
// falls under the credentials mount. Callers should respond with 403.
//
// The check is conservative — it cleans the path and compares against
// the protected prefix, so symlinks pointing outside the mount and
// `..` traversals collapse to their canonical form before the check.
func isProtectedReadPath(p string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	return clean == credsMountPath || strings.HasPrefix(clean, credsMountPath+string(filepath.Separator))
}

// JSON writes a JSON response with the given status code.
func JSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Success writes a success response.
func Success(w http.ResponseWriter, message string, data interface{}) {
	JSON(w, http.StatusOK, models.Response{
		Success: true,
		Message: message,
		Data:    data,
	})
}

// Error writes an error response.
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, models.Response{
		Success: false,
		Message: message,
	})
}

// DecodeJSON decodes JSON request body into v.
func DecodeJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
}
