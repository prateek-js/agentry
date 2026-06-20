//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// onResize calls f whenever the controlling terminal is resized
// (SIGWINCH). Unix-only; the windows build provides a no-op.
func onResize(f func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			f()
		}
	}()
}
