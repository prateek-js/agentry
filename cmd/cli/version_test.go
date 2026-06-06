package main

import (
	"strings"
	"testing"
)

func TestVersionString_NotEmptyAndPrefixed(t *testing.T) {
	v := versionString()
	if !strings.HasPrefix(v, "agentry ") {
		t.Errorf("version %q must start with 'agentry '", v)
	}
	if v == "agentry " || strings.TrimSpace(v) == "agentry" {
		t.Errorf("version body empty: %q", v)
	}
}
