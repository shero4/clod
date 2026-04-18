//go:build windows

package main

import (
	"os"
	"time"

	"golang.org/x/term"
)

// watchResize polls the parent TTY size at ~5 Hz and, on change, resizes the
// PTY slave. Windows does not have SIGWINCH; polling is cheap and simple and
// avoids the ReadConsoleInput event-loop dance. Exits when p.done is closed.
func watchResize(p *PTY) {
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		lastW, lastH := 0, 0
		for {
			select {
			case <-p.done:
				return
			case <-t.C:
				w, h, err := term.GetSize(int(os.Stdin.Fd()))
				if err != nil {
					continue
				}
				if w != lastW || h != lastH {
					syncSize(p)
					lastW, lastH = w, h
				}
			}
		}
	}()
}
