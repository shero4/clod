//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
)

type unixBackend struct {
	cmd    *exec.Cmd
	master *os.File
}

func newBackend(cfg PTYConfig, w, h int) (backend, error) {
	cmd := exec.Command(cfg.Path, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.Env
	ws := &pty.Winsize{Rows: uint16(h), Cols: uint16(w)}
	master, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}
	return &unixBackend{cmd: cmd, master: master}, nil
}

func (b *unixBackend) Read(p []byte) (int, error)  { return b.master.Read(p) }
func (b *unixBackend) Write(p []byte) (int, error) { return b.master.Write(p) }

func (b *unixBackend) Resize(w, h int) error {
	return pty.Setsize(b.master, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
}

func (b *unixBackend) Wait() error  { return b.cmd.Wait() }
func (b *unixBackend) Close() error { return b.master.Close() }
func (b *unixBackend) Kill() {
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}

// postStart runs optional one-shot setup against the live PTY. No-op on
// Unix; on Windows it works around ConPTY rendering artifacts.
func postStart(p *PTY) {}

// watchResize wires SIGWINCH so parent-TTY resize events propagate into the
// PTY slave. Exits when p.done is closed.
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
