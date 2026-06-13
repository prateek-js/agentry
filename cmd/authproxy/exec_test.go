package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitArgvSimple(t *testing.T) {
	got := splitArgv("node server.js")
	want := []string{"node", "server.js"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestSplitArgvQuotedSegment(t *testing.T) {
	got := splitArgv(`node --flag="with spaces" run`)
	want := []string{"node", "--flag=with spaces", "run"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestSplitArgvCollapsesMultipleSpaces(t *testing.T) {
	got := splitArgv("  node    server.js   ")
	want := []string{"node", "server.js"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestSplitArgvEmpty(t *testing.T) {
	got := splitArgv("")
	if len(got) != 0 {
		t.Fatalf("expected zero args, got %#v", got)
	}
}

func TestResolveUpstreamPortDefault(t *testing.T) {
	cfg := &Config{Port: "3000"}
	t.Setenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT", "")
	got, err := resolveUpstreamPort(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "3001" {
		t.Fatalf("got %q, want 3001", got)
	}
}

func TestResolveUpstreamPortExplicit(t *testing.T) {
	cfg := &Config{Port: "3000"}
	t.Setenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT", "8080")
	got, err := resolveUpstreamPort(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "8080" {
		t.Fatalf("got %q, want 8080", got)
	}
}

func TestResolveUpstreamPortRejectsCollision(t *testing.T) {
	cfg := &Config{Port: "3000"}
	t.Setenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT", "3000")
	_, err := resolveUpstreamPort(cfg)
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "collide") {
		t.Fatalf("error should mention collision: %v", err)
	}
}

func TestResolveUpstreamPortRejectsGarbage(t *testing.T) {
	cfg := &Config{Port: "3000"}
	t.Setenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT", "not-a-port")
	_, err := resolveUpstreamPort(cfg)
	if err == nil {
		t.Fatal("expected number-parse error")
	}
}

func TestResolveUpstreamPortBadParentPort(t *testing.T) {
	cfg := &Config{Port: "garbage"}
	t.Setenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT", "")
	_, err := resolveUpstreamPort(cfg)
	if err == nil {
		t.Fatal("expected parent-port parse error")
	}
}

func TestResolveUpstreamPortAtTopOfRange(t *testing.T) {
	cfg := &Config{Port: "65535"}
	t.Setenv("AGENTRY_AUTHPROXY_UPSTREAM_PORT", "")
	_, err := resolveUpstreamPort(cfg)
	if err == nil {
		t.Fatal("expected no-room error")
	}
}

func TestChildEnvDropsExecAndOverridesPort(t *testing.T) {
	t.Setenv("PORT", "3000")
	t.Setenv("AGENTRY_AUTHPROXY_EXEC", "echo hi")
	t.Setenv("UNRELATED", "keep-me")
	env := childEnv("3001")
	var hasPort, hasExec, hasUnrelated bool
	for _, kv := range env {
		switch {
		case kv == "PORT=3001":
			hasPort = true
		case strings.HasPrefix(kv, "PORT="):
			t.Fatalf("found unexpected PORT entry: %q", kv)
		case strings.HasPrefix(kv, "AGENTRY_AUTHPROXY_EXEC="):
			hasExec = true
		case kv == "UNRELATED=keep-me":
			hasUnrelated = true
		}
	}
	if !hasPort {
		t.Fatal("PORT=3001 not in child env")
	}
	if hasExec {
		t.Fatal("AGENTRY_AUTHPROXY_EXEC leaked into child env")
	}
	if !hasUnrelated {
		t.Fatal("UNRELATED env var not preserved")
	}
}

func TestExecModeEnabled(t *testing.T) {
	t.Setenv("AGENTRY_AUTHPROXY_EXEC", "")
	if execModeEnabled() {
		t.Fatal("expected disabled when env empty")
	}
	t.Setenv("AGENTRY_AUTHPROXY_EXEC", "node server.js")
	if !execModeEnabled() {
		t.Fatal("expected enabled when env set")
	}
}
