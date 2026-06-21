package handlers

import (
	"net/http"
	"os"

	"github.com/agentry-ai/agentry/pkg/models"
)

// SandboxHandler returns sandbox context info.
func SandboxHandler(w http.ResponseWriter, r *http.Request) {
	id := os.Getenv("SANDBOX_ID")
	if id == "" {
		id = "local"
	}
	homeDir := os.Getenv("WORKSPACE")
	if homeDir == "" {
		homeDir = "/workspace"
	}

	Success(w, "sandbox info", models.SandboxInfo{
		ID:      id,
		HomeDir: homeDir,
		Version: "1.0.0",
	})
}
