package handlers

import (
	"net/http"

	"github.com/agentry/agentry/pkg/models"
	"github.com/agentry/agentry/pkg/shell"
)

// ShellHandler creates a handler for shell command execution.
func ShellHandler(mgr *shell.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.ShellExecRequest
		if err := DecodeJSON(r, &req); err != nil {
			Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Command == "" {
			Error(w, http.StatusBadRequest, "command is required")
			return
		}

		sessionID := "default"
		if req.ID != nil && *req.ID != "" {
			sessionID = *req.ID
		}

		execDir := ""
		if req.ExecDir != nil {
			execDir = *req.ExecDir
		}

		timeout := shell.DefaultTimeout
		if req.Timeout != nil && *req.Timeout > 0 {
			timeout = *req.Timeout
		}

		output, exitCode, status := mgr.Execute(sessionID, req.Command, execDir, timeout)

		Success(w, "command executed", models.ShellExecData{
			SessionID: sessionID,
			Command:   req.Command,
			Status:    status,
			Output:    output,
			ExitCode:  exitCode,
		})
	}
}
