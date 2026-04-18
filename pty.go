package main

import (
	"fmt"
	"io"
	"os"
	"sync"

	xpty "github.com/aymanbagabas/go-pty"
	"golang.org/x/term"
)

// PTY wraps a child process attached to a new pseudo-terminal. Bytes flow
// between the user's real TTY (os.Stdin/os.Stdout) and the PTY master, so the
// child sees a normal interactive terminal. Inject writes programmatic bytes
// into the PTY master without racing the stdin copier.
//
// go-pty provides a single abstraction over Unix PTYs (via creack/pty) and
// Windows ConPTY, so this file is platform-neutral; only the resize-event
// source differs per-OS (see pty_unix.go / pty_windows.go).
type PTY struct {
	pty      xpty.Pty
	cmd      *xpty.Cmd
	writeMu  sync.Mutex // guards concurrent writes to the master
	rawState *term.State
	done     chan struct{}
}

// PTYConfig is the spawn request for StartPTY.
type PTYConfig struct {
	Path string   // binary to exec
	Args []string // arguments (not including argv[0])
	Dir  string   // working directory
	Env  []string // environment
}

// StartPTY spawns the child on a new pseudo-terminal. Must be called from a
// real TTY (os.Stdin must be a terminal). Places the parent TTY in raw mode;
// call Wait to restore it.
func StartPTY(cfg PTYConfig) (*PTY, error) {
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return nil, fmt.Errorf("clod must run in a real terminal (stdin is not a tty)")
	}

	ptmx, err := xpty.New()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}

	// Size the PTY to the parent TTY BEFORE starting the child. go-pty's
	// default ConPTY size on Windows is 80x25, and some TUIs (claude among
	// them) render their initial layout against that and don't cleanly
	// redraw on a later resize. Setting the size up front avoids the
	// wrapped-headers / cramped-panels look.
	if w, h, sizeErr := term.GetSize(stdinFd); sizeErr == nil {
		_ = ptmx.Resize(w, h)
	}

	cmd := ptmx.Command(cfg.Path, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env

	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, fmt.Errorf("start child: %w", err)
	}

	p := &PTY{
		pty:  ptmx,
		cmd:  cmd,
		done: make(chan struct{}),
	}

	// Keep size synced on subsequent parent-TTY resizes.
	watchResize(p) // platform-specific (see pty_unix.go / pty_windows.go)

	// Raw mode on the parent TTY so keystrokes pass through byte-for-byte.
	state, err := term.MakeRaw(stdinFd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = ptmx.Close()
		return nil, fmt.Errorf("make raw: %w", err)
	}
	p.rawState = state

	// stdin -> pty. Locked so Inject can interleave without corrupting a
	// partial user keystroke.
	go p.stdinLoop()
	// pty -> stdout. Single writer to stdout; no lock needed.
	go func() { _, _ = io.Copy(os.Stdout, ptmx) }()

	return p, nil
}

func (p *PTY) stdinLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			p.writeMu.Lock()
			_, werr := p.pty.Write(buf[:n])
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
	_, err := io.WriteString(p.pty, text)
	return err
}

// Wait blocks until the child exits, then restores the parent TTY.
func (p *PTY) Wait() error {
	err := p.cmd.Wait()
	close(p.done)
	if p.rawState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), p.rawState)
	}
	_ = p.pty.Close()
	return err
}

// Kill terminates the child process. Portable across Unix (SIGKILL) and
// Windows (TerminateProcess).
func (p *PTY) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// syncSize reads the current size of the parent TTY and applies it to the
// child's PTY slave.
func syncSize(p *PTY) {
	w, h, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return
	}
	_ = p.pty.Resize(w, h)
}
