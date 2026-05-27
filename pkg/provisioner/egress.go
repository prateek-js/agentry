package provisioner

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// EgressMode selects the default rule applied to traffic that doesn't
// match an explicit entry in EgressPolicy.Rules.
type EgressMode string

const (
	// EgressAllow is the default-allow stance: traffic flows freely and
	// rules express explicit blocks. Useful as a soft deny-list.
	EgressAllow EgressMode = "allow"

	// EgressDeny is the default-deny stance: traffic is blocked and rules
	// express explicit allows. The right choice when the sandbox shouldn't
	// be able to talk to anything you haven't explicitly approved.
	EgressDeny EgressMode = "deny"
)

// EgressRule names a single CIDR + (optional) port range + protocol. The
// shape is deliberately small — domain allow-listing belongs at the DNS
// layer, not at the packet filter, and we leave that to a future phase.
type EgressRule struct {
	CIDR  string `json:"cidr"`
	Ports []int  `json:"ports,omitempty"`
	Proto string `json:"proto,omitempty"` // "tcp", "udp", or "" (both)
}

// EgressPolicy controls what the sandbox can dial out to. Enforcement
// happens at the backend trust boundary (the container's netns for
// Docker, NetworkPolicy for K8s) so the LLM inside the sandbox can't
// bypass it by writing raw sockets or rewriting iptables.
type EgressPolicy struct {
	Mode  EgressMode   `json:"mode"`
	Rules []EgressRule `json:"rules,omitempty"`
}

// IsZero returns true when no policy was supplied. The backend should
// treat this as "do not install any rules" so unconfigured sandboxes
// keep working under whatever the host firewall does.
func (p EgressPolicy) IsZero() bool {
	return p.Mode == "" && len(p.Rules) == 0
}

// Validate returns nil for an empty policy, an error for a malformed
// one. Called from the provisioner HTTP layer so the API caller gets a
// 400 rather than a 500 from the backend.
func (p EgressPolicy) Validate() error {
	if p.IsZero() {
		return nil
	}
	switch p.Mode {
	case EgressAllow, EgressDeny:
	default:
		return fmt.Errorf("egress.mode %q: must be %q or %q", p.Mode, EgressAllow, EgressDeny)
	}
	for i, r := range p.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("egress.rules[%d]: %w", i, err)
		}
	}
	return nil
}

func (r EgressRule) validate() error {
	if r.CIDR == "" {
		return errors.New("cidr is required")
	}
	if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
		return fmt.Errorf("cidr %q: %w", r.CIDR, err)
	}
	for _, p := range r.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port %d: out of range", p)
		}
	}
	switch r.Proto {
	case "", "tcp", "udp":
	default:
		return fmt.Errorf("proto %q: must be tcp, udp, or empty", r.Proto)
	}
	return nil
}

// RenderNFT returns the nft script that implements this policy. The
// script is what we feed `nft -f -` from a privileged sidecar that
// joins the sandbox container's network namespace.
//
// Loopback + DNS are always allowed: the runtime listens on loopback,
// and without DNS the sandbox can't resolve even the hosts in an
// allow-list, making the policy useless.
//
// The script is idempotent — `flush ruleset` clears any previous
// install so re-applying just replaces.
func (p EgressPolicy) RenderNFT() string {
	if p.IsZero() {
		return ""
	}
	defaultVerdict := "accept"
	ruleVerdict := "drop" // allow mode → rules express explicit blocks
	if p.Mode == EgressDeny {
		defaultVerdict = "drop"
		ruleVerdict = "accept"
	}

	var b strings.Builder
	b.WriteString("flush ruleset\n")
	b.WriteString("table inet ad_sandbox_egress {\n")
	b.WriteString("  chain output {\n")
	fmt.Fprintf(&b, "    type filter hook output priority 0; policy %s;\n", defaultVerdict)
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    oif \"lo\" accept\n")
	b.WriteString("    ip daddr 127.0.0.0/8 accept\n")
	b.WriteString("    udp dport 53 accept\n")
	b.WriteString("    tcp dport 53 accept\n")
	for _, r := range p.Rules {
		writeRuleLines(&b, r, ruleVerdict)
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// writeRuleLines emits the nft lines that implement one rule. The shape
// depends on what the rule constrains:
//
//   - ports + proto:  "ip daddr X tcp dport { ... } verdict"   (one line)
//   - ports, no proto:expand to one tcp + one udp line
//   - proto, no ports: "ip daddr X meta l4proto tcp verdict"   (one line)
//   - neither:        "ip daddr X verdict"                     (one line, all l4)
//
// We expand explicitly rather than use anonymous nft sets so the output
// is greppable in logs.
func writeRuleLines(b *strings.Builder, r EgressRule, verdict string) {
	if len(r.Ports) == 0 {
		switch r.Proto {
		case "":
			// Address-only block/allow — covers every protocol.
			fmt.Fprintf(b, "    ip daddr %s %s\n", r.CIDR, verdict)
		default:
			// Address + protocol with no port match needs the l4proto
			// match qualifier; a bare "tcp" / "udp" expects a port.
			fmt.Fprintf(b, "    ip daddr %s meta l4proto %s %s\n", r.CIDR, r.Proto, verdict)
		}
		return
	}

	protos := []string{r.Proto}
	if r.Proto == "" {
		protos = []string{"tcp", "udp"}
	}
	parts := make([]string, len(r.Ports))
	for i, p := range r.Ports {
		parts[i] = strconv.Itoa(p)
	}
	portClause := " dport { " + strings.Join(parts, ", ") + " }"
	for _, proto := range protos {
		fmt.Fprintf(b, "    ip daddr %s %s%s %s\n", r.CIDR, proto, portClause, verdict)
	}
}
