//go:build windows

package main

// onResize is a no-op on Windows — there's no SIGWINCH. The shell opens
// at the initial terminal size; live resizing isn't wired up.
func onResize(_ func()) {}
