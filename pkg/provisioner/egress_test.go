package provisioner

import (
	"strings"
	"testing"
)

func TestEgressPolicyIsZero(t *testing.T) {
	if !(EgressPolicy{}).IsZero() {
		t.Fatal("zero policy should report IsZero")
	}
	p := EgressPolicy{Mode: EgressAllow}
	if p.IsZero() {
		t.Fatal("policy with mode set should not be zero")
	}
}

func TestEgressPolicyValidate(t *testing.T) {
	cases := []struct {
		name    string
		policy  EgressPolicy
		wantErr string
	}{
		{"empty ok", EgressPolicy{}, ""},
		{"bad mode", EgressPolicy{Mode: "loose"}, "mode"},
		{"bad cidr", EgressPolicy{
			Mode:  EgressDeny,
			Rules: []EgressRule{{CIDR: "not-a-cidr"}},
		}, "cidr"},
		{"bad port", EgressPolicy{
			Mode:  EgressAllow,
			Rules: []EgressRule{{CIDR: "10.0.0.0/8", Ports: []int{99999}}},
		}, "port"},
		{"bad proto", EgressPolicy{
			Mode:  EgressDeny,
			Rules: []EgressRule{{CIDR: "10.0.0.0/8", Proto: "icmp"}},
		}, "proto"},
		{"valid allow with block", EgressPolicy{
			Mode:  EgressAllow,
			Rules: []EgressRule{{CIDR: "169.254.169.254/32"}},
		}, ""},
		{"valid deny with allow", EgressPolicy{
			Mode: EgressDeny,
			Rules: []EgressRule{
				{CIDR: "0.0.0.0/0", Ports: []int{443}, Proto: "tcp"},
			},
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.policy.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestEgressRenderNFT_AllowMode_RulesAreBlocks(t *testing.T) {
	// Default-allow + a rule to block the AWS instance metadata endpoint
	// is the classic "soft jail" — let the LLM reach the internet, but
	// not the IMDS that would let it pivot into the host's IAM role.
	p := EgressPolicy{
		Mode: EgressAllow,
		Rules: []EgressRule{
			{CIDR: "169.254.169.254/32", Proto: "tcp", Ports: []int{80, 443}},
		},
	}
	got := p.RenderNFT()
	mustContain(t, got, "policy accept;")
	mustContain(t, got, "udp dport 53 accept")
	mustContain(t, got, "ip daddr 169.254.169.254/32 tcp dport { 80, 443 } drop")
}

func TestEgressRenderNFT_DenyMode_RulesAreAllows(t *testing.T) {
	// Default-deny + an explicit allow for HTTPS to the internet.
	// Cloud-style allow-list.
	p := EgressPolicy{
		Mode: EgressDeny,
		Rules: []EgressRule{
			{CIDR: "0.0.0.0/0", Proto: "tcp", Ports: []int{443}},
		},
	}
	got := p.RenderNFT()
	mustContain(t, got, "policy drop;")
	mustContain(t, got, "ct state established,related accept")
	mustContain(t, got, "ip daddr 0.0.0.0/0 tcp dport { 443 } accept")
}

func TestEgressRenderNFT_AddressOnlyRule(t *testing.T) {
	// CIDR with no proto and no ports: emit a single all-l4 line. nft
	// can't bind a bare "tcp"/"udp" without a port match, so falling
	// back to "ip daddr X verdict" is the only valid shape.
	p := EgressPolicy{
		Mode:  EgressDeny,
		Rules: []EgressRule{{CIDR: "10.0.0.0/8"}},
	}
	got := p.RenderNFT()
	mustContain(t, got, "    ip daddr 10.0.0.0/8 accept\n")
}

func TestEgressRenderNFT_ProtoNoPortsUsesL4proto(t *testing.T) {
	// Specifying tcp/udp but no port: must use "meta l4proto" qualifier.
	p := EgressPolicy{
		Mode:  EgressAllow,
		Rules: []EgressRule{{CIDR: "169.254.169.254/32", Proto: "tcp"}},
	}
	got := p.RenderNFT()
	mustContain(t, got, "ip daddr 169.254.169.254/32 meta l4proto tcp drop")
}

func TestEgressRenderNFT_EmptyIsEmpty(t *testing.T) {
	if got := (EgressPolicy{}).RenderNFT(); got != "" {
		t.Fatalf("zero policy should render empty, got %q", got)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q; got:\n%s", needle, haystack)
	}
}
