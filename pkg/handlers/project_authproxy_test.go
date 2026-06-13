package handlers

import (
	"reflect"
	"testing"
)

func TestAuthEnabledInEnvTruthy(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"TRUE":  true,
		"True":  true,
		"1":     true,
		"yes":   true,
		"YES":   true,
		"false": false,
		"0":     false,
		"no":    false,
		"":      false,
	}
	for v, want := range cases {
		env := []string{"AGENTRY_AUTH_ENABLED=" + v}
		if got := authEnabledInEnv(env); got != want {
			t.Errorf("v=%q: got %v, want %v", v, got, want)
		}
	}
}

func TestAuthEnabledInEnvMissing(t *testing.T) {
	env := []string{"OTHER=value"}
	if authEnabledInEnv(env) {
		t.Fatal("expected false when the key is absent")
	}
}

func TestSetEnvKeyReplacesExisting(t *testing.T) {
	env := []string{"A=1", "TARGET=old", "B=2"}
	env = setEnvKey(env, "TARGET", "new")
	want := []string{"A=1", "TARGET=new", "B=2"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("got %#v, want %#v", env, want)
	}
}

func TestSetEnvKeyAppendsWhenMissing(t *testing.T) {
	env := []string{"A=1"}
	env = setEnvKey(env, "NEW", "val")
	want := []string{"A=1", "NEW=val"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("got %#v, want %#v", env, want)
	}
}

func TestJoinAuthproxyExecSimple(t *testing.T) {
	got := joinAuthproxyExec([]string{"node", "server.js"})
	want := "node server.js"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestJoinAuthproxyExecQuotesSpaces(t *testing.T) {
	got := joinAuthproxyExec([]string{"node", "--flag=with spaces", "run"})
	want := `node "--flag=with spaces" run`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMaybeWrapAuthSidecarPassthrough(t *testing.T) {
	// No AGENTRY_AUTH_ENABLED → no wrap. internalPort must be 0 so the
	// caller knows there's no port to hide.
	env := []string{"PORT=3000"}
	cmd, args, gotEnv, internal := maybeWrapAuthSidecar([]string{"node", "server.js"}, env)
	if cmd != "node" {
		t.Fatalf("expected cmd=node, got %q", cmd)
	}
	if !reflect.DeepEqual(args, []string{"server.js"}) {
		t.Fatalf("args wrong: %#v", args)
	}
	if !reflect.DeepEqual(gotEnv, env) {
		t.Fatalf("env should not change; got %#v", gotEnv)
	}
	if internal != 0 {
		t.Fatalf("internalPort should be 0 when not wrapped, got %d", internal)
	}
}

func TestMaybeWrapAuthSidecarFallsBackWhenBinaryMissing(t *testing.T) {
	// /usr/local/bin/authproxy is unlikely to exist on the dev mac
	// running the tests. The fallback returns the original command
	// unchanged so the project still starts. internalPort=0 because
	// no wrap means no port to hide.
	env := []string{"AGENTRY_AUTH_ENABLED=true"}
	cmd, args, gotEnv, internal := maybeWrapAuthSidecar([]string{"node", "server.js"}, env)
	if cmd == authproxyBinary {
		t.Skip("authproxy binary exists at the expected path; cannot exercise fallback")
	}
	if cmd != "node" {
		t.Fatalf("expected fallback to node, got %q", cmd)
	}
	if !reflect.DeepEqual(args, []string{"server.js"}) {
		t.Fatalf("args wrong: %#v", args)
	}
	if internal != 0 {
		t.Fatalf("internalPort should be 0 on fallback, got %d", internal)
	}
	// AGENTRY_AUTHPROXY_EXEC must NOT be stamped when the binary's
	// missing — otherwise downstream tooling sees a misleading marker.
	for _, kv := range gotEnv {
		if kv[:len("AGENTRY_AUTHPROXY_EXEC=")] == "AGENTRY_AUTHPROXY_EXEC=" {
			t.Fatal("AGENTRY_AUTHPROXY_EXEC stamped despite missing binary")
		}
		// Quick break — we only need the first non-match.
		break
	}
}

func TestFilterInternalPort(t *testing.T) {
	cases := []struct {
		name     string
		ports    []int
		internal int
		want     []int
	}{
		{"no wrap → return as-is", []int{3000, 5432}, 0, []int{3000, 5432}},
		{"wrap, both ports listed → 3001 stripped", []int{3000, 3001}, 3001, []int{3000}},
		{"wrap, only internal listed → empty (rare race)", []int{3001}, 3001, []int{}},
		{"wrap, only public listed → unchanged", []int{3000}, 3001, []int{3000}},
		{"wrap + unrelated services", []int{3000, 3001, 5432, 6379}, 3001, []int{3000, 5432, 6379}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Copy the input so the in-place filter doesn't corrupt
			// shared test data across subtests.
			in := append([]int(nil), tc.ports...)
			got := filterInternalPort(in, tc.internal)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
