package main

import (
	"fmt"
	"runtime/debug"
)

// version is stamped at release time via
// -ldflags "-X main.version=v0.x.y" (see the Makefile `release` target).
// Empty for local dev builds, which fall back to VCS build info.
var version string

// cmdVersion prints what the user installed: the release semver when
// stamped, otherwise the VCS commit + dirty state baked in by `go build`.
// The stdlib debug.ReadBuildInfo populates the latter automatically.
func cmdVersion(_ []string) int {
	fmt.Println(versionString())
	return 0
}

func versionString() string {
	if version != "" {
		return "agentry " + version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "agentry (version unknown)"
	}
	v := info.Main.Version
	// `go build` from a non-released checkout reports "(devel)" — strip
	// the parens noise and report the commit instead.
	if v == "" || v == "(devel)" {
		var rev, mod string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					mod = " (dirty)"
				}
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			return "agentry " + rev + mod
		}
		return "agentry (devel)"
	}
	return "agentry " + v
}
