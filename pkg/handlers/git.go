package handlers

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"github.com/agentry/agentry/pkg/models"
)

func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// GitInitHandler initializes a git repository.
func GitInitHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitInitRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	dir := req.Path
	if dir == "" {
		dir = "/workspace"
	}
	out, err := gitCmd(dir, "init")
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("git init failed: %s", out))
		return
	}
	Success(w, "repository initialized", map[string]string{"output": out})
}

// GitCommitHandler commits changes.
func GitCommitHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitCommitRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Message == "" {
		Error(w, http.StatusBadRequest, "message is required")
		return
	}
	dir := req.Path
	if dir == "" {
		dir = "/workspace"
	}
	if req.AddAll != nil && *req.AddAll {
		if out, err := gitCmd(dir, "add", "-A"); err != nil {
			Error(w, http.StatusInternalServerError, fmt.Sprintf("git add failed: %s", out))
			return
		}
	}
	out, err := gitCmd(dir, "commit", "-m", req.Message)
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("git commit failed: %s", out))
		return
	}
	Success(w, "committed", map[string]string{"output": out})
}

// GitDiffHandler shows git diff.
func GitDiffHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitPathRequest
	_ = DecodeJSON(r, &req)
	dir := req.Path
	if dir == "" {
		dir = "/workspace"
	}
	out, _ := gitCmd(dir, "diff")
	Success(w, "diff", map[string]string{"output": out})
}

// GitStatusHandler shows git status.
func GitStatusHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitPathRequest
	_ = DecodeJSON(r, &req)
	dir := req.Path
	if dir == "" {
		dir = "/workspace"
	}
	out, _ := gitCmd(dir, "status", "--short")
	Success(w, "status", map[string]string{"output": out})
}

// GitLogHandler shows git log.
func GitLogHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitPathRequest
	_ = DecodeJSON(r, &req)
	dir := req.Path
	if dir == "" {
		dir = "/workspace"
	}
	out, _ := gitCmd(dir, "log", "--oneline", "-20")
	Success(w, "log", map[string]string{"output": out})
}

// GitCloneHandler clones a repository.
func GitCloneHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitCloneRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.URL == "" {
		Error(w, http.StatusBadRequest, "url is required")
		return
	}
	dest := req.Path
	if dest == "" {
		dest = "/workspace"
	}
	out, err := gitCmd("", "clone", "--depth", "1", req.URL, dest)
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("git clone failed: %s", out))
		return
	}
	Success(w, "cloned", map[string]string{"output": out})
}

// GitCheckoutHandler checks out a branch.
func GitCheckoutHandler(w http.ResponseWriter, r *http.Request) {
	var req models.GitCheckoutRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Branch == "" {
		Error(w, http.StatusBadRequest, "branch is required")
		return
	}
	dir := req.Path
	if dir == "" {
		dir = "/workspace"
	}
	args := []string{"checkout"}
	if req.Create != nil && *req.Create {
		args = append(args, "-b")
	}
	args = append(args, req.Branch)
	out, err := gitCmd(dir, args...)
	if err != nil {
		Error(w, http.StatusInternalServerError, fmt.Sprintf("git checkout failed: %s", out))
		return
	}
	Success(w, "checked out", map[string]string{"output": out})
}
