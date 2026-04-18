package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// PTY wraps a child process attached to a new pseudo-terminal. Bytes flow
// between the user's real TTY (os.Stdin/os.Stdout) and the PTY master, so the
// child sees a normal interactive terminal. Inject writes programmatic bytes
// into the PTY master without racing the stdin copier.
type PTY struct {
	cmd      *exec.Cmd
	master   *os.File
	writeMu  sync.Mutex // guards concurrent writes to master
	rawState *term.State
	winchCh  chan os.Signal
	done     chan struct{}
}

// Start spawns cmd on a new PTY. Must be called from a real TTY (os.Stdin
// must be a terminal). Places the parent TTY in raw mode; call Wait to
// restore.
func StartPTY(cmd *exec.Cmd) (*PTY, error) {
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return nil, fmt.Errorf("clod must run in a real terminal (stdin is not a tty)")
	}

	master, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	p := &PTY{
		cmd:     cmd,
		master:  master,
		winchCh: make(chan os.Signal, 1),
		done:    make(chan struct{}),
	}

	// Match the child's window size to the parent's, initially and on resize.
	if err := pty.InheritSize(os.Stdin, master); err != nil {
		// Non-fatal; just log.
		fmt.Fprintf(os.Stderr, "clod: inherit size: %v\n", err)
	}
	signal.Notify(p.winchCh, syscall.SIGWINCH)
	go p.resizeLoop()

	// Raw mode on the parent TTY so keystrokes pass through byte-for-byte.
	state, err := term.MakeRaw(stdinFd)
	if err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("make raw: %w", err)
	}
	p.rawState = state

	// stdin → pty master. We own the write side of master so Inject can
	// interleave without corrupting a partial user keystroke.
	go p.stdinLoop()
	// pty master → stdout. Single writer to stdout; no lock needed.
	go func() { _, _ = io.Copy(os.Stdout, master) }()

	return p, nil
}

func (p *PTY) resizeLoop() {
	for {
		select {
		case <-p.done:
			return
		case <-p.winchCh:
			_ = pty.InheritSize(os.Stdin, p.master)
		}
	}
}

func (p *PTY) stdinLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			p.writeMu.Lock()
			_, werr := p.master.Write(buf[:n])
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
	_, err := io.WriteString(p.master, text)
	return err
}

// Wait blocks until the child exits, then restores the parent TTY.
func (p *PTY) Wait() error {
	err := p.cmd.Wait()
	close(p.done)
	signal.Stop(p.winchCh)
	if p.rawState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), p.rawState)
	}
	_ = p.master.Close()
	return err
}

// Kill sends SIGTERM (then SIGKILL after a grace period elsewhere if needed).
func (p *PTY) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
}
