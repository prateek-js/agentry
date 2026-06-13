package handlers

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// portCacheTTL is how long a pgid's discovered ports are reused before
// re-running `ss`. project_list is the tool LLMs poll most while waiting
// for a server to bind, and each uncached call forks `ss` + stats every
// listening pid's /proc entry. One second is short enough that a port
// appearing feels instant to the poller, long enough that a tight poll
// loop doesn't fork a subprocess per call. Var (not const) so tests can
// drop it to zero.
var portCacheTTL = 1 * time.Second

type portCacheEntry struct {
	ports []int
	at    time.Time
}

var (
	portCacheMu sync.Mutex
	portCache   = map[int]portCacheEntry{}
)

// portsForPGIDCached wraps portsForPGID with a short per-pgid TTL cache.
// The expensive `ss` call runs OUTSIDE the lock so concurrent lookups
// for different projects don't serialize; a rare double-fetch for the
// same pgid under contention is harmless (same answer). Stale entries
// are swept opportunistically on write so restarted projects (each gets
// a fresh pgid) don't leak map entries forever.
func portsForPGIDCached(pgid int) []int {
	return cachedPorts(pgid, portsForPGID)
}

// cachedPorts is the cache layer behind portsForPGIDCached, with the
// expensive fetch injected so tests can drive it without forking `ss`.
func cachedPorts(pgid int, fetch func(int) []int) []int {
	if pgid <= 0 {
		return nil
	}
	portCacheMu.Lock()
	if e, ok := portCache[pgid]; ok && time.Since(e.at) < portCacheTTL {
		ports := e.ports
		portCacheMu.Unlock()
		return ports
	}
	portCacheMu.Unlock()

	ports := fetch(pgid)

	now := time.Now()
	portCacheMu.Lock()
	portCache[pgid] = portCacheEntry{ports: ports, at: now}
	// Cheap GC: when the map grows, drop anything well past its TTL.
	if len(portCache) > 32 {
		for k, e := range portCache {
			if now.Sub(e.at) > 60*time.Second {
				delete(portCache, k)
			}
		}
	}
	portCacheMu.Unlock()
	return ports
}

// portsForPGID returns every TCP port some process in `pgid` is
// currently LISTENING on, deduped and sorted. Returns nil (not an
// error) on any failure — `ss` missing, the process group gone,
// proc files unreadable, etc. — so callers can poll cheaply.
//
// Strategy: shell out to `ss -tlnpHO` (no header, one-line, with
// process info), parse each line's `Local-Address:Port` plus the
// `users:(...)` field's pid=N entries, then read /proc/<pid>/stat's
// pgrp field and keep only those matching `pgid`.
//
// Requires the `ss` binary from iproute2 (we install it in the
// runtime image's Dockerfile) and root access to read /proc/<pid>/fd
// indirectly via stat (we don't actually need fd, just pgrp).
func portsForPGID(pgid int) []int {
	if pgid <= 0 {
		return nil
	}
	cmd := exec.Command("ss", "-tlnpHO")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	seen := make(map[int]struct{})
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		port, pids := parseSSLine(sc.Text())
		if port == 0 || len(pids) == 0 {
			continue
		}
		for _, pid := range pids {
			if procPGID(pid) == pgid {
				seen[port] = struct{}{}
				break
			}
		}
	}

	out2 := make([]int, 0, len(seen))
	for p := range seen {
		out2 = append(out2, p)
	}
	sort.Ints(out2)
	return out2
}

// parseSSLine extracts the listening port and PID list from one row
// of `ss -tlnpHO` output. Layout (space-separated):
//
//	LISTEN 0 511 *:8080 *:* users:(("sandbox-runtim",pid=1,fd=3))
//	└─state Recv-Q Send-Q LocalAddr:Port PeerAddr:Port [Process]
//
// The address can be `*:N`, `[::]:N`, `127.0.0.1:N`, etc. — the port
// is always after the LAST `:` in field[3].
func parseSSLine(line string) (port int, pids []int) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return 0, nil
	}
	port = portFromSSAddr(fields[3])
	if port == 0 {
		return 0, nil
	}
	// The users:(...) chunk may span multiple "fields" if Go's Fields
	// splits on the comma+space inside it. Re-join everything from
	// the first piece that starts with `users:(`.
	for i := 4; i < len(fields); i++ {
		if strings.HasPrefix(fields[i], "users:(") {
			rest := strings.Join(fields[i:], " ")
			pids = parsePIDsFromSSUsers(rest)
			break
		}
	}
	return port, pids
}

func portFromSSAddr(addr string) int {
	// Handle `[::]:80`, `[::1]:80`, `*:80`, `127.0.0.1:80`. Always the
	// rightmost colon is the port boundary.
	idx := strings.LastIndex(addr, ":")
	if idx < 0 || idx == len(addr)-1 {
		return 0
	}
	p, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		return 0
	}
	return p
}

// parsePIDsFromSSUsers extracts every PID from a `users:(...)` blob.
// The blob looks like
//
//	users:(("python3",pid=42,fd=3),("worker",pid=43,fd=5))
//
// Sockets shared across processes (preforking servers, for example)
// list each process — we collect them all so the caller's PGID check
// picks up whichever is in the project's group.
func parsePIDsFromSSUsers(s string) []int {
	var pids []int
	for {
		i := strings.Index(s, "pid=")
		if i < 0 {
			return pids
		}
		s = s[i+len("pid="):]
		j := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == 0 {
			return pids
		}
		if p, err := strconv.Atoi(s[:j]); err == nil {
			pids = append(pids, p)
		}
		s = s[j:]
	}
}

// procPGID reads /proc/<pid>/stat and returns the process group id
// (the 5th field). Returns 0 on any error — caller will treat that
// as "doesn't match my pgid", which is the right behaviour: a dead
// process can't own a live socket.
//
// /proc/<pid>/stat format:
//
//	<pid> (<comm>) <state> <ppid> <pgrp> <session> ...
//
// `comm` may contain spaces, parens, and other delights, so the
// canonical way to parse is to take everything after the LAST `)`,
// then split on whitespace.
func procPGID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 || end+2 >= len(s) {
		return 0
	}
	tail := strings.Fields(s[end+2:])
	if len(tail) < 3 {
		return 0
	}
	// tail[0] = state, tail[1] = ppid, tail[2] = pgrp
	pgid, err := strconv.Atoi(tail[2])
	if err != nil {
		return 0
	}
	return pgid
}
