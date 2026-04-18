package main

import (
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// PTYConfig is the spawn request for StartPTY.
type PTYConfig struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// backend is the OS-specific PTY implementation. pty_unix.go wraps
// creack/pty; pty_windows.go wraps a hand-rolled ConPTY with
// PSEUDOCONSOLE_PASSTHROUGH_MODE enabled (avoids nested-ConPTY VT
// re-translation artifacts).
type backend interface {
	io.ReadWriter
	Resize(w, h int) error
	Wait() error
	Close() error
	Kill()
}

// PTY wraps a backend with the common TTY-plumbing logic: raw mode on the
// parent TTY, stdin forwarding with a mutex so Inject can interleave, and
// stdout forwarding to the user's terminal.
type PTY struct {
	backend  backend
	writeMu  sync.Mutex
	rawState *term.State
	done     chan struct{}
}

// StartPTY spawns cfg.Path on a new PTY. Must be invoked from a real TTY.
// Places the parent TTY in raw mode; call Wait to restore.
func StartPTY(cfg PTYConfig) (*PTY, error) {
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return nil, fmt.Errorf("clod must run in a real terminal (stdin is not a tty)")
	}

	// Size the child to the parent TTY from frame zero. A delayed resize
	// makes TUIs like claude render their initial layout at the ConPTY
	// default (80x25) and not always redraw cleanly.
	w, h, _ := term.GetSize(stdinFd)
	if w <= 0 || h <= 0 {
		w, h = 80, 24
	}

	b, err := newBackend(cfg, w, h)
	if err != nil {
		return nil, err
	}

	p := &PTY{backend: b, done: make(chan struct{})}

	watchResize(p) // SIGWINCH on unix, polling on windows

	state, err := term.MakeRaw(stdinFd)
	if err != nil {
		b.Kill()
		_ = b.Close()
		return nil, fmt.Errorf("make raw: %w", err)
	}
	p.rawState = state

	go p.stdinLoop()
	go func() { _, _ = io.Copy(os.Stdout, b) }()

	postStart(p)

	return p, nil
}

func (p *PTY) stdinLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			p.writeMu.Lock()
			_, werr := p.backend.Write(buf[:n])
			p.writeMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Inject writes text into the PTY as if the user typed it. Atomic with
// respect to stdin forwarding.
func (p *PTY) Inject(text string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err := io.WriteString(p.backend, text)
	return err
}

// Wait blocks until the child exits, then restores the parent TTY and
// releases backend resources.
func (p *PTY) Wait() error {
	err := p.backend.Wait()
	close(p.done)
	if p.rawState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), p.rawState)
	}
	_ = p.backend.Close()
	return err
}

// Kill terminates the child process.
func (p *PTY) Kill() { p.backend.Kill() }

// syncSize reads the current size of the parent TTY and applies it to the
// child's PTY. Called from the platform-specific resize watcher.
func syncSize(p *PTY) {
	w, h, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return
	}
	_ = p.backend.Resize(w, h)
}
