package handlers

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/agentry/agentry/pkg/models"
)

// ProcessListHandler lists running processes.
// Uses `ps -eo` which works on both Ubuntu and Alpine (BusyBox).
func ProcessListHandler(w http.ResponseWriter, r *http.Request) {
	// Format: PID %CPU RSS STARTED COMMAND
	// RSS is in KB. Works on GNU ps and BusyBox ps.
	out, err := exec.Command("ps", "-eo", "pid,%cpu,rss,start,args", "--no-headers").Output()
	if err != nil {
		// BusyBox fallback (Alpine): different format.
		out, err = exec.Command("ps", "-o", "pid,pcpu,rss,comm").Output()
		if err != nil {
			Error(w, http.StatusInternalServerError, fmt.Sprintf("ps failed: %v", err))
			return
		}
	}

	var procs []models.ProcessInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		pid, _ := strconv.Atoi(fields[0])
		if pid <= 1 {
			continue
		}

		cpuPct, _ := strconv.ParseFloat(fields[1], 64)
		rssKB, _ := strconv.ParseFloat(fields[2], 64)
		started := fields[3]
		cmd := strings.Join(fields[4:], " ")

		// Skip kernel threads and internal processes.
		if strings.HasPrefix(cmd, "[") || strings.Contains(cmd, "sandbox-runtime") {
			continue
		}

		// Extract process name from command (first path component).
		name := cmd
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			name = name[idx+1:]
		}
		if idx := strings.Index(name, " "); idx >= 0 {
			name = name[:idx]
		}

		procs = append(procs, models.ProcessInfo{
			PID:     pid,
			Name:    name,
			CPUPct:  cpuPct,
			MemMB:   rssKB / 1024.0,
			Command: cmd,
			Started: started,
		})
	}

	Success(w, "processes listed", procs)
}

// ProcessStopHandler stops a process by PID or name.
func ProcessStopHandler(w http.ResponseWriter, r *http.Request) {
	var req models.ProcessStopRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.PID != nil {
		proc, err := os.FindProcess(*req.PID)
		if err != nil {
			Error(w, http.StatusNotFound, "process not found")
			return
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			Error(w, http.StatusInternalServerError, fmt.Sprintf("kill failed: %v", err))
			return
		}
		Success(w, fmt.Sprintf("sent SIGTERM to PID %d", *req.PID), nil)
		return
	}

	if req.Name != nil {
		name := *req.Name
		if !validProcessName(name) {
			Error(w, http.StatusBadRequest,
				"name must be 1..64 chars of [A-Za-z0-9_.+@/-] — no regex meta, no shell quoting tricks")
			return
		}
		// pkill --exact matches against the comm field only (no -f regex
		// over the full argv). Combined with the validProcessName guard,
		// the worst a caller can do is kill processes with a literal
		// matching name they could have spawned themselves.
		out, err := exec.Command("pkill", "--exact", name).CombinedOutput()
		if err != nil {
			// pkill exits 1 when no processes matched — not an error
			// from the caller's POV. Treat as "nothing to kill".
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				Success(w, fmt.Sprintf("no processes matched '%s'", name), nil)
				return
			}
			Error(w, http.StatusInternalServerError, fmt.Sprintf("pkill failed: %s", string(out)))
			return
		}
		Success(w, fmt.Sprintf("stopped processes named '%s'", name), nil)
		return
	}

	Error(w, http.StatusBadRequest, "pid or name is required")
}

// validProcessName guards the pkill --exact path. POSIX comm is
// generally 15 chars max for the literal value, but argv[0] in /proc
// can be longer — 64 chars covers anything users actually name a
// process. Allow the conservative shell-and-grep-safe character set
// only.
func validProcessName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '+' || c == '@' || c == '/' || c == '-':
		default:
			return false
		}
	}
	return true
}
