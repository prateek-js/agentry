package main

import (
	"net/url"
	"testing"
)

func TestRepairConnectionURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "already valid — postgres simple",
			in:   "postgresql://postgres:pw@host:5432/db",
			want: "postgresql://postgres:pw@host:5432/db",
		},
		{
			// The user's actual failure case from the bug report.
			// `%az` looks like a malformed percent-encoded triplet
			// because `az` aren't hex digits. Repair encodes the raw
			// `%` as `%25` so parsers see well-formed encoding.
			name: "supabase password with %az",
			in:   "postgresql://postgres:4g.uW%azWbL*mZ9@db.qwmcvkqwdweuxefofeni.supabase.co:5432/postgres",
			want: "postgresql://postgres:4g.uW%25azWbL%2AmZ9@db.qwmcvkqwdweuxefofeni.supabase.co:5432/postgres",
		},
		{
			name: "no userinfo — pass through unchanged",
			in:   "postgresql://host:5432/db",
			want: "postgresql://host:5432/db",
		},
		{
			name: "mongodb+srv with raw % in password",
			in:   "mongodb+srv://u:r%aw@cluster.mongodb.net/db",
			want: "mongodb+srv://u:r%25aw@cluster.mongodb.net/db",
		},
		{
			// Go's url.Parse accepts u:p@ss@host gracefully (splits on
			// last @), so the URL is already valid — no repair needed.
			// Verify we don't molest these.
			name: "password with @ — already valid",
			in:   `postgresql://u:p@ss@host/db`,
			want: `postgresql://u:p@ss@host/db`,
		},
		{
			// Likewise, : in a password is fine: Go splits userinfo on
			// the first : (user=u, password=p1:p2).
			name: "password with : — already valid",
			in:   `postgres://u:p1:p2@host/db`,
			want: `postgres://u:p1:p2@host/db`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repairConnectionURL(tc.in)
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
			// Sanity: every repaired URL parses cleanly.
			if got != "" {
				if _, err := url.Parse(got); err != nil {
					t.Errorf("repaired URL still doesn't parse: %v", err)
				}
			}
		})
	}
}

func TestRepairConnectionURL_Idempotent(t *testing.T) {
	// A URL that's already correctly percent-encoded must NOT get
	// re-encoded on the second pass. Without this property, repeated
	// bindings of the same URL would produce ever-more-encoded results.
	once := repairConnectionURL("postgresql://postgres:4g.uW%azWbL*mZ9@host/db")
	twice := repairConnectionURL(once)
	if once != twice {
		t.Fatalf("not idempotent:\n  once=%q\n  twice=%q", once, twice)
	}
}

func TestRepairConnectionURL_LeavesUnknownShapesAlone(t *testing.T) {
	// A bad URL with no userinfo isn't something we know how to fix.
	// Hand it back; the parser's error is more useful to the user
	// than a confusing "repaired" version.
	bad := "://no-scheme.example/path"
	if got := repairConnectionURL(bad); got != bad {
		t.Errorf("should have left malformed input alone: got %q", got)
	}
}

func TestLooksLikeDBURL(t *testing.T) {
	cases := map[string]bool{
		"postgresql://x":       true,
		"postgres://x":         true,
		"mysql://x":            true,
		"mariadb://x":          true,
		"mongodb://x":          true,
		"mongodb+srv://x":      true,
		"sk-abcdef":            false, // OpenAI key
		"AKIA12345":            false, // AWS key
		"https://api.host":     false,
		"":                     false,
	}
	for v, want := range cases {
		if got := looksLikeDBURL(v); got != want {
			t.Errorf("looksLikeDBURL(%q) = %v, want %v", v, got, want)
		}
	}
}
