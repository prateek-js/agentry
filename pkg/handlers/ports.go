package handlers

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/models"
)

// PortsListHandler lists ports with listening processes.
func PortsListHandler(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("ss", "-tlnp").Output()
	if err != nil {
		// Fallback to netstat.
		out, err = exec.Command("netstat", "-tlnp").Output()
		if err != nil {
			Error(w, http.StatusInternalServerError, "cannot list ports")
			return
		}
	}

	var ports []models.PortInfo
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		fields := strings.Fields(line)
		for _, f := range fields {
			if strings.Contains(f, ":") {
				parts := strings.Split(f, ":")
				if port, err := strconv.Atoi(parts[len(parts)-1]); err == nil && port > 0 {
					ports = append(ports, models.PortInfo{
						Port:  port,
						State: "LISTEN",
					})
					break
				}
			}
		}
	}

	Success(w, "ports listed", ports)
}

// PortWaitHandler blocks until a port is listening.
func PortWaitHandler(w http.ResponseWriter, r *http.Request) {
	var req models.PortWaitRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Port <= 0 {
		Error(w, http.StatusBadRequest, "port is required")
		return
	}

	timeout := 30
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds > 0 {
		timeout = *req.TimeoutSeconds
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", req.Port)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			Success(w, fmt.Sprintf("port %d is listening", req.Port), nil)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	Error(w, http.StatusRequestTimeout, fmt.Sprintf("port %d not listening after %ds", req.Port, timeout))
}
