package handlers

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/agentry-ai/agentry/pkg/models"
)

// ArchiveCreateHandler creates a tar.gz archive.
func ArchiveCreateHandler(w http.ResponseWriter, r *http.Request) {
	var req models.ArchiveCreateRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Files) == 0 || req.Output == "" {
		Error(w, http.StatusBadRequest, "files and output are required")
		return
	}

	// GNU tar applies --exclude patterns positionally — they must come
	// BEFORE the input files. Place them right after the output spec.
	args := []string{"czf", req.Output}
	for _, p := range req.Exclude {
		if p == "" {
			continue
		}
		args = append(args, "--exclude="+p)
	}
	args = append(args, req.Files...)
	out, err := exec.Command("tar", args...).CombinedOutput()
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("tar create failed: %s", strings.TrimSpace(string(out))))
		return
	}
	Success(w, "archive created", map[string]string{"output": req.Output})
}

// ArchiveExtractHandler extracts an archive.
func ArchiveExtractHandler(w http.ResponseWriter, r *http.Request) {
	var req models.ArchiveExtractRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Archive == "" || req.Destination == "" {
		Error(w, http.StatusBadRequest, "archive and destination are required")
		return
	}
	// tar -xC fails if the destination doesn't exist; create it first so
	// callers don't have to mkdir as a separate step.
	if err := os.MkdirAll(req.Destination, 0o755); err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("mkdir destination: %v", err))
		return
	}

	out, err := exec.Command("tar", "xzf", req.Archive, "-C", req.Destination).CombinedOutput()
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("tar extract failed: %s", strings.TrimSpace(string(out))))
		return
	}
	Success(w, "archive extracted", map[string]string{"destination": req.Destination})
}
