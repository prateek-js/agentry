package provisioner

import (
	"strings"
	"testing"
)

// buildEnvExportLine is the seam between Go's map[string]string deploy
// env and a one-line shell prefix. Three shapes worth pinning:
//
//   1. empty map → empty string (don't emit `npm run build` with a
//      lone trailing space that smells weird in logs)
//   2. multiple keys → alphabetic order so the line is deterministic
//   3. value with a single quote → quoted via '\'' escape; the shell
//      should evaluate the byte sequence to the original literal

func TestBuildEnvExportLine_Empty(t *testing.T) {
	if got := buildEnvExportLine(nil); got != "" {
		t.Errorf("nil map: got %q, want empty", got)
	}
	if got := buildEnvExportLine(map[string]string{}); got != "" {
		t.Errorf("empty map: got %q, want empty", got)
	}
}

func TestBuildEnvExportLine_DeterministicOrder(t *testing.T) {
	got := buildEnvExportLine(map[string]string{
		"ZED":   "last",
		"ALPHA": "first",
		"M":     "mid",
	})
	// Order matters: ALPHA → M → ZED. We compare on positions because
	// the exact spacing is part of the contract too.
	want := "ALPHA='first' M='mid' ZED='last' "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildEnvExportLine_EscapesSingleQuotes(t *testing.T) {
	// A value with a single quote is the failure shape that bites:
	// MONGODB_URL connection strings sometimes contain quotes in
	// passwords. The escape sequence in sh is "'\''" — literally
	// close the open-quote, emit a backslash-quoted quote, reopen.
	got := buildEnvExportLine(map[string]string{"X": "a'b"})
	want := "X='a'\\''b' "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Sanity: the rendered string must NOT contain an unescaped
	// single quote that would terminate the shell quoting early.
	if strings.Contains(strings.TrimPrefix(strings.TrimSuffix(got, "' "), "X='"), "'") {
		// open-quote and close-quote stripped; remainder must have
		// only escaped quotes, not bare ones
		bare := strings.TrimPrefix(strings.TrimSuffix(got, "' "), "X='")
		if strings.Contains(bare, "'") && !strings.Contains(bare, `'\''`) {
			t.Errorf("unescaped bare quote in %q", bare)
		}
	}
}
