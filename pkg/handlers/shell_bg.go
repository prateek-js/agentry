package handlers

import (
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/agentry/agentry/pkg/shell"
)

// BgStartRequest is the JSON body for POST /v1/shell/background.
type BgStartRequest struct {
	Command string            `json:"command"`
	ExecDir string            `json:"exec_dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// BgStartResponse is the body returned by POST /v1/shell/background.
// The ID is what every subsequent endpoint expects.
type BgStartResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// BgLogsResponse is returned by GET /v1/shell/background/{id}/logs.
//
// `content` holds raw bytes — UTF-8 if the program emits valid UTF-8,
// otherwise base64. `encoding` tells the caller which it got.
// `cursor` is the new absolute-byte offset; pass it back next call.
// `dropped` is the number of bytes that fell out of the ring before
// this read could see them.
type BgLogsResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // "utf-8" | "base64"
	Cursor   int64  `json:"cursor"`
	Dropped  int64  `json:"dropped,omitempty"`
}

// BgStartHandler starts a new background command.
func BgStartHandler(mgr *shell.BackgroundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req BgStartRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Command == "" {
			Error(w, http.StatusBadRequest, "command is required")
			return
		}

		env := buildEnv(req.Env)

		id, err := mgr.Start(r.Context(), req.Command, req.ExecDir, env)
		if err != nil {
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		Success(w, "background command started", BgStartResponse{
			ID: id, Status: shell.BgStatusRunning,
		})
	}
}

// BgListHandler returns the status of every tracked background command.
func BgListHandler(mgr *shell.BackgroundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		Success(w, "ok", map[string]any{
			"commands": mgr.List(),
		})
	}
}

// BgStatusHandler returns one command's status.
func BgStatusHandler(mgr *shell.BackgroundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		st, ok := mgr.Status(id)
		if !ok {
			Error(w, http.StatusNotFound, "command not found")
			return
		}
		Success(w, "ok", st)
	}
}

// BgLogsHandler returns bytes written since `cursor` (query param).
func BgLogsHandler(mgr *shell.BackgroundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		var cursor int64
		if v := r.URL.Query().Get("cursor"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				Error(w, http.StatusBadRequest, "cursor must be a non-negative integer")
				return
			}
			cursor = n
		}

		data, newCursor, dropped, ok := mgr.Logs(id, cursor)
		if !ok {
			Error(w, http.StatusNotFound, "command not found")
			return
		}

		out := BgLogsResponse{Cursor: newCursor, Dropped: dropped}
		// Send raw UTF-8 when possible — agents reading the logs much
		// prefer text. Fall back to base64 for binary payloads so we
		// never produce invalid JSON.
		if utf8.Valid(data) {
			out.Content = string(data)
			out.Encoding = "utf-8"
		} else {
			out.Content = base64.StdEncoding.EncodeToString(data)
			out.Encoding = "base64"
		}
		Success(w, "ok", out)
	}
}

// BgInterruptHandler sends SIGTERM (then SIGKILL after grace) to the
// command's process group.
func BgInterruptHandler(mgr *shell.BackgroundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := mgr.Interrupt(id); err != nil {
			if shell.IsNotFound(err) || errors.Is(err, os.ErrNotExist) {
				Error(w, http.StatusNotFound, "command not found")
				return
			}
			Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		Success(w, "interrupt sent", map[string]string{"id": id})
	}
}

// BgForgetHandler removes a finished command's bookkeeping. Returns 409
// when the command is still running.
func BgForgetHandler(mgr *shell.BackgroundManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := mgr.Status(id); !ok {
			Error(w, http.StatusNotFound, "command not found")
			return
		}
		if !mgr.Forget(id) {
			Error(w, http.StatusConflict, "command is still running; interrupt first")
			return
		}
		Success(w, "forgotten", map[string]string{"id": id})
	}
}

// buildEnv merges the request's env map onto the current process env
// (so users can shadow specific vars but still inherit PATH, HOME, …).
// Returns nil when the user supplied nothing — exec.Cmd treats nil as
// "inherit parent env" which is what we want.
func buildEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	base := os.Environ()
	for k, v := range extra {
		base = append(base, k+"="+v)
	}
	return base
}
