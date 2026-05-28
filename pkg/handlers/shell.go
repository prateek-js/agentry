package handlers

import (
	"fmt"
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

		// On hard_timeout, prepend an actionable hint so the LLM caller
		// retries with a bigger budget instead of misreading the partial
		// output as a transport failure ("tunnel down", "sandbox lost").
		// The hint costs ~120 bytes; without it Roo/Claude routinely
		// bails on pip-install class commands and writes files locally.
		if status == "hard_timeout" {
			hint := fmt.Sprintf(
				"[command_run: timed out after %.0fs — the tunnel and sandbox are fine, only this call's budget expired. Retry the same command with a higher `timeout` (300+ for pip/npm install, 900 for docker build). Partial stdout below.]\n",
				timeout,
			)
			output = hint + output
		}

		Success(w, "command executed", models.ShellExecData{
			SessionID: sessionID,
			Command:   req.Command,
			Status:    status,
			Output:    output,
			ExitCode:  exitCode,
		})
	}
}
