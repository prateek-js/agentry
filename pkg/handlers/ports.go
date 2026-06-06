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
//
// We preserve the bind address from each LISTEN row so the dashboard
// can distinguish ports that are reachable from outside the sandbox
// (0.0.0.0:* or a specific interface) from loopback-only sockets
// (Jupyter kernels' 5 ZMQ ports per kernel, internal IPC, etc.).
// Without this, every kernel a code_exec context spawns shows up in
// the dashboard's port table as an unattributed mystery port — looks
// like a bug to the user, isn't.
func PortsListHandler(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("ss", "-tlnp").Output()
	if err != nil {
		// Fallback to netstat. Both formats put the local address
		// column at field index 3 on a LISTEN row, so the same parser
		// handles both.
		out, err = exec.Command("netstat", "-tlnp").Output()
		if err != nil {
			Error(w, http.StatusInternalServerError, "cannot list ports")
			return
		}
	}

	ports := parsePortListing(string(out))
	Success(w, "ports listed", ports)
}

// parsePortListing extracts one PortInfo per LISTEN row from `ss
// -tlnp` or `netstat -tlnp` output. Header rows + non-LISTEN entries
// are skipped. Returns nil for parse misses so the response stays
// well-formed even when ss output drifts (kernel update, distro
// quirks, etc.).
//
// Both ss and netstat write the local address as field index 3 on a
// LISTEN row:
//
//	ss:      LISTEN 0 511 0.0.0.0:3000 0.0.0.0:* users:(...)
//	netstat: tcp    0 0   0.0.0.0:3000 0.0.0.0:* LISTEN 1234/node
//
// Pulled out as a pure function so it's unit-testable without spawning
// real processes.
func parsePortListing(raw string) []models.PortInfo {
	var ports []models.PortInfo
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		host, port, ok := parseListenAddress(fields[3])
		if !ok || port == 0 {
			continue
		}
		ports = append(ports, models.PortInfo{
			Port:     port,
			State:    "LISTEN",
			Address:  host,
			Loopback: isLoopbackHost(host),
		})
	}
	return ports
}

// parseListenAddress splits a ss/netstat local-address column into
// host + port. Accepts the four shapes ss writes:
//
//	"0.0.0.0:3000"   → ("0.0.0.0",  3000, true)
//	"127.0.0.1:5432" → ("127.0.0.1", 5432, true)
//	"[::]:8080"      → ("::",       8080, true)
//	"[::1]:9000"     → ("::1",      9000, true)
//
// IPv6 always wears brackets in this output, so the bracket check is
// load-bearing — splitting on ':' for "::1:9000" would otherwise grab
// "1" as the port and ":" as the host.
func parseListenAddress(s string) (host string, port int, ok bool) {
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 || end+2 > len(s) || s[end+1] != ':' {
			return "", 0, false
		}
		host = s[1:end]
		p, err := strconv.Atoi(s[end+2:])
		if err != nil {
			return "", 0, false
		}
		return host, p, true
	}
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", 0, false
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return s[:i], p, true
}

// isLoopbackHost mirrors net.IP.IsLoopback over a string host. Returns
// false for "0.0.0.0" and "::" — those are wildcards that mean "all
// interfaces" and therefore include external reachability.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
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
