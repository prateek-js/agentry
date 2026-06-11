package main

import (
	"reflect"
	"sort"
	"testing"
)

func TestAuthEnvForStateEnabledNoProviders(t *testing.T) {
	state := &AuthState{
		Enabled:   true,
		DBBinding: "postgres",
		Secret:    "deadbeefcafe",
	}
	keys, values := authEnvForState(state)
	sort.Strings(keys)
	want := []string{"AGENTRY_AUTH_DB", "AGENTRY_AUTH_ENABLED", "AGENTRY_AUTH_SECRET"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys: got %v, want %v", keys, want)
	}
	if values["AGENTRY_AUTH_ENABLED"] != "true" {
		t.Fatalf("ENABLED: %q", values["AGENTRY_AUTH_ENABLED"])
	}
	if values["AGENTRY_AUTH_DB"] != "postgres" {
		t.Fatalf("DB: %q", values["AGENTRY_AUTH_DB"])
	}
	if values["AGENTRY_AUTH_SECRET"] != "deadbeefcafe" {
		t.Fatalf("SECRET: %q", values["AGENTRY_AUTH_SECRET"])
	}
}

func TestAuthEnvForStateWithProviders(t *testing.T) {
	state := &AuthState{
		Enabled:   true,
		DBBinding: "postgres",
		Secret:    "k",
		Providers: map[string]AuthProviderState{
			"google": {ClientID: "gid", ClientSecret: "gsec"},
			"github": {ClientID: "hid", ClientSecret: "hsec"},
		},
	}
	keys, values := authEnvForState(state)
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	for _, want := range []string{
		"AGENTRY_AUTH_ENABLED",
		"AGENTRY_AUTH_DB",
		"AGENTRY_AUTH_SECRET",
		"GOOGLE_CLIENT_ID",
		"GOOGLE_CLIENT_SECRET",
		"GITHUB_CLIENT_ID",
		"GITHUB_CLIENT_SECRET",
	} {
		if !keySet[want] {
			t.Errorf("missing key %s in %v", want, keys)
		}
	}
	if values["GOOGLE_CLIENT_ID"] != "gid" {
		t.Errorf("google client id: %q", values["GOOGLE_CLIENT_ID"])
	}
}

func TestAuthEnvForStateDisabledNil(t *testing.T) {
	keys, values := authEnvForState(nil)
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	for _, want := range []string{
		"AGENTRY_AUTH_ENABLED",
		"AGENTRY_AUTH_DB",
		"AGENTRY_AUTH_SECRET",
	} {
		if !keySet[want] {
			t.Fatalf("missing key %s", want)
		}
		if values[want] != "" {
			t.Fatalf("%s should be empty on disabled state, got %q", want, values[want])
		}
	}
}

func TestAuthEnvForStateDisabledExplicit(t *testing.T) {
	state := &AuthState{Enabled: false, DBBinding: "postgres", Secret: "k"}
	_, values := authEnvForState(state)
	if values["AGENTRY_AUTH_ENABLED"] != "" {
		t.Fatalf("ENABLED should be empty when state.Enabled=false, got %q", values["AGENTRY_AUTH_ENABLED"])
	}
}

func TestAuthEnvForStateKeysSorted(t *testing.T) {
	state := &AuthState{
		Enabled: true, DBBinding: "x", Secret: "y",
		Providers: map[string]AuthProviderState{
			"zeta":  {ClientID: "i", ClientSecret: "s"},
			"alpha": {ClientID: "i", ClientSecret: "s"},
		},
	}
	keys, _ := authEnvForState(state)
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("keys not sorted at %d: %v", i, keys)
		}
	}
}

func TestSandboxIsLiveStatuses(t *testing.T) {
	cases := map[string]bool{
		"running":    true,
		"starting":   true,
		"ready":      true,
		"":           true, // empty status — bridge hasn't synced; assume live
		"unknown":    true, // novel — push and let the runtime decide
		"stopped":    false,
		"stopping":   false,
		"failed":     false,
		"errored":    false,
		"deleted":    false,
	}
	for status, want := range cases {
		got := sandboxIsLive(sandboxInfo{Status: status})
		if got != want {
			t.Errorf("status=%q: got %v, want %v", status, got, want)
		}
	}
}
