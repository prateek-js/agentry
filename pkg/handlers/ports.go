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

// PortsListHandler lists ports with listening processes and splits
// them by ownership: ports that belong to a running project's pgid
// (Managed: true) vs anything else (returned ALSO in
// unmanaged_listeners as a separate slice). The unmanaged list is the
// in-band signal that catches the LLM falling back to bare
// command_run / command_start when it should be using project_create.
//
// We preserve the bind address from each LISTEN row so the dashboard
// can distinguish ports that are reachable from outside the sandbox
// (0.0.0.0:* or a specific interface) from loopback-only sockets
// (Jupyter kernels' 5 ZMQ ports per kernel, internal IPC, etc.).
// Without this, every kernel a code_exec context spawns shows up in
// the dashboard's port table as an unattributed mystery port — looks
// like a bug to the user, isn't.
func PortsListHandler(pm *ProjectManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// `-tlnpHO` is the same call portsForPGID uses: one-line
		// format, no header, includes the `users:(...)` block we need
		// for pid extraction. Fall back to plain `-tlnp` if that
		// flavour of ss isn't available, then netstat — without pids
		// we can still report the port but not classify it.
		out, err := exec.Command("ss", "-tlnpHO").Output()
		withPIDs := err == nil
		if !withPIDs {
			out, err = exec.Command("ss", "-tlnp").Output()
			if err != nil {
				out, err = exec.Command("netstat", "-tlnp").Output()
				if err != nil {
					Error(w, http.StatusInternalServerError, "cannot list ports")
					return
				}
			}
		}

		ports := parsePortListing(string(out))

		// Classify by project pgid: a listener is managed iff at
		// least one of its PIDs has a pgid that matches a running
		// project's leader pid. When we couldn't extract pids
		// (netstat fallback), Managed stays false for everything —
		// the LLM treats every port as suspect, which is better than
		// false-positive ownership.
		var unmanaged []models.PortInfo
		if withPIDs && pm != nil {
			projectPGIDs := pm.RunningPGIDs()
			for i := range ports {
				pids := ports[i].pids
				ports[i].pids = nil // never serialise
				if isManagedListener(pids, projectPGIDs) {
					ports[i].Managed = true
				} else {
					// Skip the loopback ZMQ ports a Jupyter kernel
					// spawns — they're "unmanaged" but also expected
					// noise, not the user-facing servers we want the
					// LLM to wire into a project. Loopback => boring;
					// 0.0.0.0/interface => actually reachable.
					if ports[i].Loopback {
						continue
					}
					unmanaged = append(unmanaged, ports[i].PortInfo)
				}
			}
		}

		Success(w, "ports listed", models.PortsListData{
			Ports:              dropPIDsForJSON(ports),
			UnmanagedListeners: unmanaged,
		})
	}
}

// portInfoWithPIDs is the internal carrier — parsePortListing now
// captures pids so the handler can classify. Wire format stays
// models.PortInfo (no pids field), so we strip before serialising.
type portInfoWithPIDs struct {
	models.PortInfo
	pids []int
}

// isManagedListener returns true when any pid in the listener has a
// pgid in the set of project pgids.
func isManagedListener(pids []int, projectPGIDs map[int]struct{}) bool {
	if len(projectPGIDs) == 0 {
		return false
	}
	for _, pid := range pids {
		if _, ok := projectPGIDs[procPGID(pid)]; ok {
			return true
		}
	}
	return false
}

// dropPIDsForJSON strips the internal pids field after classification.
// We use a type alias internally and the wire model on the way out so
// existing dashboard consumers don't see new fields they don't expect.
func dropPIDsForJSON(in []portInfoWithPIDs) []models.PortInfo {
	out := make([]models.PortInfo, len(in))
	for i, p := range in {
		out[i] = p.PortInfo
	}
	return out
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
func parsePortListing(raw string) []portInfoWithPIDs {
	var ports []portInfoWithPIDs
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
		// PIDs come from the trailing `users:(...)` blob when the ss
		// invocation included `-p`. Best-effort: absent on netstat
		// fallback, harmless for the wire response (Managed stays
		// false), useful for classification.
		var pids []int
		for i := 4; i < len(fields); i++ {
			if strings.HasPrefix(fields[i], "users:(") {
				pids = parsePIDsFromSSUsers(strings.Join(fields[i:], " "))
				break
			}
		}
		ports = append(ports, portInfoWithPIDs{
			PortInfo: models.PortInfo{
				Port:     port,
				State:    "LISTEN",
				Address:  host,
				Loopback: isLoopbackHost(host),
			},
			pids: pids,
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
