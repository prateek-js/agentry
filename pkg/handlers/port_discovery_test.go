package handlers

import (
	"os"
	"os/exec"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"
)

// portFromSSAddr / parsePIDsFromSSUsers / parseSSLine are the
// pure-string parts of port discovery — they need no Docker, no
// /proc, no ss binary, so they're cheap to unit-test directly.

func TestPortFromSSAddr(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1:5200":  5200,
		"*:8080":          8080,
		"[::]:53":         53,
		"[::1]:8081":      8081,
		"0.0.0.0:65535":   65535,
		"":                0,
		":":               0,
		"no-port":         0,
		"127.0.0.1:bogus": 0,
	}
	for in, want := range cases {
		if got := portFromSSAddr(in); got != want {
			t.Errorf("portFromSSAddr(%q) = %d; want %d", in, got, want)
		}
	}
}

func TestParsePIDsFromSSUsers(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{`users:(("python3",pid=42,fd=3))`, []int{42}},
		{`users:(("p1",pid=11,fd=3),("p2",pid=12,fd=5))`, []int{11, 12}},
		{`users:(("only-name",fd=3))`, nil}, // no pid=
		{``, nil},
	}
	for _, tc := range cases {
		got := parsePIDsFromSSUsers(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parsePIDsFromSSUsers(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseSSLine(t *testing.T) {
	cases := []struct {
		line     string
		wantPort int
		wantPIDs []int
	}{
		{
			`LISTEN 0 511 *:8080 *:* users:(("sandbox-runtim",pid=1,fd=3))`,
			8080, []int{1},
		},
		{
			`LISTEN 0 128 [::]:22 [::]:* users:(("sshd",pid=234,fd=4))`,
			22, []int{234},
		},
		{
			// Two listeners share the socket (preforking server).
			`LISTEN 0 64 127.0.0.1:5200 0.0.0.0:* users:(("uvicorn",pid=99,fd=5),("uvicorn",pid=100,fd=5))`,
			5200, []int{99, 100},
		},
		{
			// Header line — should yield nothing.
			`State Recv-Q Send-Q Local-Address:Port Peer-Address:Port`,
			0, nil,
		},
	}
	for _, tc := range cases {
		port, pids := parseSSLine(tc.line)
		if port != tc.wantPort {
			t.Errorf("port for %q = %d; want %d", tc.line, port, tc.wantPort)
		}
		if !reflect.DeepEqual(pids, tc.wantPIDs) {
			t.Errorf("pids for %q = %v; want %v", tc.line, pids, tc.wantPIDs)
		}
	}
}

// TestProcPGIDOnSelf reads /proc/self/stat and compares against the
// kernel's reported PGID. This is the only test that needs procfs,
// so it's gated on Linux.
func TestProcPGIDOnSelf(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("procfs not available")
	}
	want := syscall.Getpgrp()
	got := procPGID(os.Getpid())
	if got != want {
		t.Errorf("procPGID(self) = %d; want %d (from syscall.Getpgrp)", got, want)
	}
}

// TestPortsForPGIDDiscoversRealListener spawns a Python http.server
// in a new process group and confirms portsForPGID reports the bound
// port. End-to-end exercise of the ss + procfs path.
func TestPortsForPGIDDiscoversRealListener(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss (iproute2) not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	// Use port 0 → ask Python's http.server for any free port, then
	// parse the banner to learn which.
	cmd := exec.Command("python3", "-u", "-c",
		`import http.server, socketserver, sys
with socketserver.TCPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler) as s:
    print(s.server_address[1], flush=True)
    s.serve_forever()`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Read the printed port.
	buf := make([]byte, 16)
	n, _ := stdout.Read(buf)
	var port int
	for _, b := range buf[:n] {
		if b < '0' || b > '9' {
			break
		}
		port = port*10 + int(b-'0')
	}
	if port == 0 {
		t.Fatalf("never got a port from python (got %q)", buf[:n])
	}

	// Give Linux a beat to materialize the LISTEN socket in /proc.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := portsForPGID(cmd.Process.Pid) // Setpgid → pgid == pid
		if len(got) == 1 && got[0] == port {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := portsForPGID(cmd.Process.Pid)
	t.Fatalf("portsForPGID(%d) = %v; want [%d]", cmd.Process.Pid, got, port)
}

// TestPortsForPGIDFiltersOutOthers spawns two python servers, one in
// our test's PGID and one in its own, and confirms we only see ours.
func TestPortsForPGIDFiltersOutOthers(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	spawn := func() (*exec.Cmd, int) {
		c := exec.Command("python3", "-u", "-c",
			`import http.server, socketserver, sys
with socketserver.TCPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler) as s:
    print(s.server_address[1], flush=True)
    s.serve_forever()`)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, _ := c.StdoutPipe()
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		// Read port from stdout.
		buf := make([]byte, 16)
		n, _ := out.Read(buf)
		var p int
		for _, b := range buf[:n] {
			if b < '0' || b > '9' {
				break
			}
			p = p*10 + int(b-'0')
		}
		return c, p
	}

	a, pa := spawn()
	b, pb := spawn()
	t.Cleanup(func() {
		_ = syscall.Kill(-a.Process.Pid, syscall.SIGKILL)
		_ = a.Wait()
		_ = syscall.Kill(-b.Process.Pid, syscall.SIGKILL)
		_ = b.Wait()
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := portsForPGID(a.Process.Pid)
		if len(got) == 1 && got[0] == pa {
			// also confirm B is invisible from A's PGID
			if !containsInt(got, pb) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("isolation check failed: portsForPGID(A) = %v (A=%d B=%d)",
		portsForPGID(a.Process.Pid), pa, pb)
}

func containsInt(xs []int, v int) bool {
	i := sort.SearchInts(xs, v)
	return i < len(xs) && xs[i] == v
}
