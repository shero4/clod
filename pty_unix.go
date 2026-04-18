//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize wires SIGWINCH so terminal resize events from the parent TTY
// propagate into the PTY slave. Exits when p.done is closed.
func watchResize(p *PTY) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-p.done:
				return
			case <-ch:
				syncSize(p)
			}
		}
	}()
}
