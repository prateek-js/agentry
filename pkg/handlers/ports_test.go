package handlers

import (
	"reflect"
	"testing"

	"github.com/agentry/agentry/pkg/models"
)

// parseListenAddress exercises the four shapes ss/netstat write for
// the local-address column. The IPv6 cases are the load-bearing ones
// — splitting on the last ':' alone would mangle them.
func TestParseListenAddress(t *testing.T) {
	cases := []struct {
		in     string
		host   string
		port   int
		ok     bool
		reason string
	}{
		{"0.0.0.0:3000", "0.0.0.0", 3000, true, "ipv4 wildcard"},
		{"127.0.0.1:5432", "127.0.0.1", 5432, true, "ipv4 loopback"},
		{"10.0.0.5:8080", "10.0.0.5", 8080, true, "ipv4 specific interface"},
		{"[::]:8080", "::", 8080, true, "ipv6 wildcard"},
		{"[::1]:9000", "::1", 9000, true, "ipv6 loopback"},
		{"[2001:db8::1]:443", "2001:db8::1", 443, true, "ipv6 full address"},
		{"", "", 0, false, "empty input"},
		{"nope", "", 0, false, "no port separator"},
		{"[::1]9000", "", 0, false, "missing colon after bracket"},
		{"127.0.0.1:abc", "", 0, false, "non-numeric port"},
	}
	for _, tc := range cases {
		host, port, ok := parseListenAddress(tc.in)
		if ok != tc.ok || host != tc.host || port != tc.port {
			t.Errorf("%s: parseListenAddress(%q) = (%q, %d, %v); want (%q, %d, %v)",
				tc.reason, tc.in, host, port, ok, tc.host, tc.port, tc.ok)
		}
	}
}

// isLoopbackHost classifies the bind side. Wildcards (0.0.0.0, ::) are
// NOT loopback — they include all interfaces including external. The
// loopback flag is what drives the dashboard's "hide internal port"
// filter, so this gate decides what the user sees.
func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", true},   // anywhere in 127.0.0.0/8
		{"::1", true},
		{"0.0.0.0", false},    // wildcard = reachable externally
		{"::", false},         // ipv6 wildcard
		{"10.0.0.5", false},   // private interface
		{"203.0.113.5", false}, // public ip
		{"", false},
		{"not-an-ip", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v; want %v", tc.host, got, tc.want)
		}
	}
}

// parsePortListing eats the full ss / netstat output. This is the
// shape the dashboard's port table consumes; the test pins the
// classification of loopback vs reachable so a kernel ZMQ port can't
// accidentally show up as a forwardable user port.
func TestParsePortListing_FullOutputClassifies(t *testing.T) {
	// Real-shaped ss -tlnp output. The mix is what a sandbox running
	// (a) a Next.js dev server on 0.0.0.0:3000 and (b) two Jupyter
	// kernels (5 ports each on 127.0.0.1) would actually emit.
	raw := `State    Recv-Q Send-Q   Local Address:Port      Peer Address:Port  Process
LISTEN   0      511      0.0.0.0:3000            0.0.0.0:*          users:(("node",pid=42,fd=18))
LISTEN   0      128      127.0.0.1:34521         0.0.0.0:*          users:(("python3.11",pid=100,fd=11))
LISTEN   0      128      127.0.0.1:44779         0.0.0.0:*          users:(("python3.11",pid=100,fd=12))
LISTEN   0      128      [::]:8080               [::]:*             users:(("ad-runtime",pid=1,fd=7))
LISTEN   0      128      [::1]:9000              [::]:*             users:(("internal-tool",pid=200,fd=9))
`
	got := parsePortListing(raw)
	want := []portInfoWithPIDs{
		{PortInfo: models.PortInfo{Port: 3000, State: "LISTEN", Address: "0.0.0.0", Loopback: false}, pids: []int{42}},
		{PortInfo: models.PortInfo{Port: 34521, State: "LISTEN", Address: "127.0.0.1", Loopback: true}, pids: []int{100}},
		{PortInfo: models.PortInfo{Port: 44779, State: "LISTEN", Address: "127.0.0.1", Loopback: true}, pids: []int{100}},
		{PortInfo: models.PortInfo{Port: 8080, State: "LISTEN", Address: "::", Loopback: false}, pids: []int{1}},
		{PortInfo: models.PortInfo{Port: 9000, State: "LISTEN", Address: "::1", Loopback: true}, pids: []int{200}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsePortListing mismatch\n got  = %#v\n want = %#v", got, want)
	}
}

// Same exercise against netstat-style output, since the runtime falls
// back to it when ss isn't installed. Column count differs (LISTEN is
// the 6th field, not the 1st), but the local-address column lands at
// index 3 in both — that's the invariant the parser depends on.
func TestParsePortListing_NetstatFallback(t *testing.T) {
	raw := `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 0.0.0.0:3000            0.0.0.0:*               LISTEN      42/node
tcp        0      0 127.0.0.1:34521         0.0.0.0:*               LISTEN      100/python3.11
`
	got := parsePortListing(raw)
	// netstat output doesn't include the users:(...) block, so pids
	// stay nil — the handler treats every listener as unmanaged in
	// that fallback. This is OK: the LLM signal degrades to "I can't
	// tell what owns this", which is safer than a false-positive.
	want := []portInfoWithPIDs{
		{PortInfo: models.PortInfo{Port: 3000, State: "LISTEN", Address: "0.0.0.0", Loopback: false}},
		{PortInfo: models.PortInfo{Port: 34521, State: "LISTEN", Address: "127.0.0.1", Loopback: true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("netstat parse mismatch\n got  = %#v\n want = %#v", got, want)
	}
}

// Defensive: a row with too few fields (truncated, header artefact)
// must not panic and must not leak into the result.
func TestParsePortListing_SkipsMalformedRows(t *testing.T) {
	raw := `State Recv-Q Send-Q
LISTEN
LISTEN 0 0
LISTEN 0 0 not-an-address-port wildcard process
`
	got := parsePortListing(raw)
	if len(got) != 0 {
		t.Errorf("expected zero parsed rows from malformed input; got %#v", got)
	}
}
