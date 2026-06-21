package provisioner

import (
	"strings"
	"testing"
)

// projectPathAndSlug is the seam between a user-supplied project name
// (`req.Project` on /deploy-build and /preflight) and the shell-
// interpolated path used in commands like `cd %s && npm run build`.
//
// Two threats it has to refuse:
//   1. Path traversal — "../etc" lands outside /workspace
//   2. Shell metacharacter injection — ";", "$(...)", backticks,
//      redirects, whitespace — would inject into the surrounding
//      shell command even when the resolved path is safe
//
// Plus the happy-path shapes still need to keep working.

func TestProjectPathAndSlug_HappyPath(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantSlug string
	}{
		{"", "/workspace", "workspace"},
		{".", "/workspace", "workspace"},
		{"app", "/workspace/projects/app", "app"},
		{"my-app", "/workspace/projects/my-app", "my-app"},
		{"web/admin", "/workspace/projects/web/admin", "web-admin"},
		// Leading/trailing slashes get trimmed.
		{"/app/", "/workspace/projects/app", "app"},
		{"app/", "/workspace/projects/app", "app"},
		{"/app", "/workspace/projects/app", "app"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotPath, gotSlug, err := projectPathAndSlug(tc.in)
			if err != nil {
				t.Errorf("err = %v; want nil", err)
				return
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q; want %q", gotPath, tc.wantPath)
			}
			if gotSlug != tc.wantSlug {
				t.Errorf("slug = %q; want %q", gotSlug, tc.wantSlug)
			}
		})
	}
}

// Path traversal — every ".." pattern that resolves outside
// /workspace/projects/ must be refused with a non-nil error and empty
// outputs. Without this guard the LLM could pass ../../../etc and have
// `cd %s` land in /etc inside the sandbox.
func TestProjectPathAndSlug_RejectsTraversal(t *testing.T) {
	bad := []string{
		"..",
		"../etc",
		"../../etc",
		"../../../etc/passwd",
		"app/../../etc",
		"app/../..",
		"./../..",
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			path, slug, err := projectPathAndSlug(in)
			if err == nil {
				t.Errorf("expected err for traversal %q; got path=%q slug=%q",
					in, path, slug)
			}
			if path != "" || slug != "" {
				t.Errorf("err returned but outputs non-empty: path=%q slug=%q", path, slug)
			}
		})
	}
}

// Shell metacharacters — every char in this set has meaning in `sh -c`,
// so even if the resolved path is "safe", these would inject into the
// surrounding command. The handler builds shell commands by
// fmt.Sprintf("cd %s && …", path), so a project name like `";rm -rf
// /;"` lands as `cd ";rm -rf /;" && …` and runs three commands.
//
// Test each common attack character in isolation so a future "let's
// allow X" change has to deliberately update this test.
func TestProjectPathAndSlug_RejectsShellMetacharacters(t *testing.T) {
	bad := []string{
		"app;rm -rf /",
		"app|cat /etc/passwd",
		"app&whoami",
		"app`whoami`",
		"app$(whoami)",
		"app>output",
		"app<input",
		"app{a,b}",
		"app[a]",
		"app(a)",
		"app'sq",
		"app\"dq",
		"app\\bs",
		"app new", // space
		"app\nnewline",
		"app\tinjection",
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			_, _, err := projectPathAndSlug(in)
			if err == nil {
				t.Errorf("expected err for shell-unsafe %q; got nil", in)
			}
		})
	}
}

// Dots are tricky — single-dot files are legitimate ("./" is the
// no-project case; ".env" would be a filename inside a project). But
// ".." anywhere must be rejected. Confirm a few edge cases.
func TestProjectPathAndSlug_DotsEdgeCases(t *testing.T) {
	// Leading dot in a component is allowed (filename-like).
	if path, _, err := projectPathAndSlug(".env-config"); err != nil {
		t.Errorf(".env-config rejected: %v (path=%q)", err, path)
	}
	// Trailing dot — file extension shape. filepath.Clean strips
	// trailing slashes but not dots, so "app." → /workspace/projects/app.
	// Still inside the project root; safe.
	if path, _, err := projectPathAndSlug("app."); err != nil {
		t.Errorf("app. rejected: %v (path=%q)", err, path)
	}
	// Bare ".." rejected (already covered in TestRejectsTraversal,
	// re-asserted here for clarity).
	if _, _, err := projectPathAndSlug(".."); err == nil {
		t.Error("'..' should be rejected as traversal")
	}
}

// The resolved path must ALWAYS be either /workspace or under
// /workspace/projects/. Spot-check a few odd resolutions.
func TestProjectPathAndSlug_StaysUnderWorkspace(t *testing.T) {
	// Cases that filepath.Clean reduces interestingly:
	cases := []string{
		"foo",
		"foo/bar",
		"foo/./bar",
		"foo/bar/./baz",
		// Note: NOT testing "foo/../bar" — that's a traversal even
		// though it lands inside /workspace/projects (foo/../bar
		// → bar). The current implementation rejects it because
		// `..` is in the input; that's defensive and correct.
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			path, _, err := projectPathAndSlug(in)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
				return
			}
			if path != "/workspace" && !strings.HasPrefix(path, "/workspace/projects/") {
				t.Errorf("path = %q; want under /workspace/projects/", path)
			}
		})
	}
}

// Defense check: even if a future change to filepath.Clean ever stopped
// normalising ".." (unlikely; it's a stdlib contract), the prefix-suffix
// check would still catch traversal. This test pins the prefix check
// independent of filepath.Clean's behaviour.
func TestProjectPathAndSlug_PrefixCheckOnly(t *testing.T) {
	// A "valid" project name that nonetheless resolves to /etc would
	// have to either bypass filepath.Clean entirely (impossible from
	// pure-string input) OR escape the metacharacter filter (also
	// impossible — "/" is the only path separator allowed). This test
	// just asserts the basic shape: a resolved path NOT prefixed
	// "/workspace/projects/" gets refused. Future refactors that swap
	// out filepath.Clean for something custom shouldn't accidentally
	// lift this guarantee.
	//
	// We can't trigger the prefix-fail branch directly without a
	// crafted ".." input (which the metacharacter filter doesn't
	// reject — only the prefix check does). Use "../etc" which goes
	// through ContainsAny clean (no metas), filepath.Clean ("/etc"),
	// and trips the prefix check.
	_, _, err := projectPathAndSlug("../etc")
	if err == nil {
		t.Error("../etc must be rejected — defense against filepath.Clean regressions")
	}
	if !strings.Contains(err.Error(), "escape") {
		// The error message should say something about escaping —
		// helps debugging if a future input slips past.
		t.Logf("error message: %q (informational)", err)
	}
}

// Stress / fuzz lite — throw a small randomized inputs at it. Anything
// that resolves to a path outside /workspace is a test failure.
func TestProjectPathAndSlug_RandomSafetyNet(t *testing.T) {
	// Hand-crafted "weird but plausible" inputs. The pattern: if
	// projectPathAndSlug returns a path (err == nil), that path MUST
	// be /workspace or inside /workspace/projects/.
	inputs := []string{
		"a", "ab", "abc", "a/b", "a/b/c",
		"x-y-z", "x_y_z", "x.y.z",
		"app1", "app-1", "app.v2",
		"deep/nested/project/structure",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			path, _, err := projectPathAndSlug(in)
			if err != nil {
				return // rejected is fine
			}
			if path != "/workspace" && !strings.HasPrefix(path, "/workspace/projects/") {
				t.Errorf("safe input %q escaped workspace: %q", in, path)
			}
		})
	}
}
