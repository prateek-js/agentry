package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/agentry/agentry/pkg/models"
)

var startTime = time.Now()

// WorkspaceStatusHandler returns composite workspace status.
func WorkspaceStatusHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projects := pm.ListProjects()

		appRunning := false
		for _, p := range projects {
			if p.Status == "running" {
				appRunning = true
				break
			}
		}

		workDir := os.Getenv("WORKSPACE")
		if workDir == "" {
			workDir = "/workspace"
		}

		fileCount := 0
		_ = filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				fileCount++
			}
			return nil
		})

		Success(w, "workspace status", models.WorkspaceStatus{
			AppRunning:      appRunning,
			Projects:        projects,
			ActiveProcesses: len(projects),
			WorkspaceFiles:  fileCount,
		})
	}
}

// ActivityHandler returns activity data for lifecycle management.
func ActivityHandler(w http.ResponseWriter, r *http.Request) {
	Success(w, "activity", models.ActivityData{
		UptimeSeconds: int64(time.Since(startTime).Seconds()),
	})
}
