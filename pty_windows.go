//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// This file is a minimal ConPTY wrapper that enables
// PSEUDOCONSOLE_PASSTHROUGH_MODE (flag 0x2 on CreatePseudoConsole). That
// mode tells the ConPTY not to parse and regenerate VT escape sequences,
// avoiding the double-translation artifacts you hit when clod's outer
// terminal is also a ConPTY (Windows Terminal, VS Code, etc.). Without it,
// claude's erase-line and cursor-position escapes get mangled across the
// two layers and dismissed prompts leave ghost text behind.
//
// Based on UserExistsError/conpty (MIT), trimmed and with the passthrough
// flag threaded through. Falls back to flag=0 (translating mode) on older
// Windows where passthrough isn't supported (ERROR_INVALID_PARAMETER).

const (
	procThreadAttributePseudoconsole uintptr = 0x20016
	stillActive                      uint32  = 259
)

var (
	modKernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole            = modKernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole            = modKernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole             = modKernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttrList   = modKernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute      = modKernel32.NewProc("UpdateProcThreadAttribute")
)

type coord struct {
	X, Y int16
}

func (c coord) pack() uintptr {
	return uintptr((int32(c.Y) << 16) | int32(c.X))
}

type startupInfoEx struct {
	startupInfo   windows.StartupInfo
	attributeList []byte
}

type windowsBackend struct {
	hpc      windows.Handle // pseudoconsole handle
	pi       windows.ProcessInformation
	cmdIn    windows.Handle // our write end → child stdin
	cmdOut   windows.Handle // child stdout → our read end
	ptyIn    windows.Handle // ConPTY-owned after CreatePseudoConsole, we still hold our reference for Close
	ptyOut   windows.Handle
	exited   bool
	exitCode uint32
}

func isConPtyAvailable() bool {
	return procCreatePseudoConsole.Find() == nil &&
		procResizePseudoConsole.Find() == nil &&
		procClosePseudoConsole.Find() == nil &&
		procInitializeProcThreadAttrList.Find() == nil &&
		procUpdateProcThreadAttribute.Find() == nil
}

func createPseudoConsole(size coord, hIn, hOut windows.Handle, flags uintptr) (windows.Handle, error) {
	var hpc windows.Handle
	ret, _, _ := procCreatePseudoConsole.Call(
		size.pack(),
		uintptr(hIn),
		uintptr(hOut),
		flags,
		uintptr(unsafe.Pointer(&hpc)),
	)
	if ret != 0 {
		return 0, fmt.Errorf("CreatePseudoConsole: status 0x%x", ret)
	}
	return hpc, nil
}

func resizePseudoConsole(hpc windows.Handle, size coord) error {
	ret, _, _ := procResizePseudoConsole.Call(uintptr(hpc), size.pack())
	if ret != 0 {
		return fmt.Errorf("ResizePseudoConsole: status 0x%x", ret)
	}
	return nil
}

func closePseudoConsole(hpc windows.Handle) {
	if procClosePseudoConsole.Find() == nil {
		procClosePseudoConsole.Call(uintptr(hpc))
	}
}

func makeStartupInfoEx(hpc windows.Handle) (*startupInfoEx, error) {
	var si startupInfoEx
	si.startupInfo.Cb = uint32(unsafe.Sizeof(windows.StartupInfo{}) + unsafe.Sizeof(&si.attributeList[0]))
	si.startupInfo.Flags |= windows.STARTF_USESTDHANDLES

	var size uintptr
	// First call: discover required size. Return is false (0).
	procInitializeProcThreadAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	si.attributeList = make([]byte, size)
	ret, _, err := procInitializeProcThreadAttrList.Call(
		uintptr(unsafe.Pointer(&si.attributeList[0])),
		1, 0,
		uintptr(unsafe.Pointer(&size)),
	)
	if ret != 1 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList: %v", err)
	}
	ret, _, err = procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&si.attributeList[0])),
		0,
		procThreadAttributePseudoconsole,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0, 0,
	)
	if ret != 1 {
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %v", err)
	}
	return &si, nil
}

func spawnProcessAttached(hpc windows.Handle, cmdLine, dir string, env []string) (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation
	cmdLinePtr, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return pi, fmt.Errorf("utf16 cmd line: %w", err)
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return pi, fmt.Errorf("utf16 dir: %w", err)
		}
	}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT)
	var envBlock *uint16
	if env != nil {
		flags |= uint32(windows.CREATE_UNICODE_ENVIRONMENT)
		envBlock = buildEnvBlock(env)
	}
	si, err := makeStartupInfoEx(hpc)
	if err != nil {
		return pi, err
	}
	err = windows.CreateProcess(
		nil,
		cmdLinePtr,
		nil, nil,
		false,
		flags,
		envBlock,
		dirPtr,
		&si.startupInfo,
		&pi,
	)
	if err != nil {
		return pi, fmt.Errorf("CreateProcess: %w", err)
	}
	return pi, nil
}

// buildEnvBlock produces a double-null-terminated UTF-16 environment block.
func buildEnvBlock(env []string) *uint16 {
	if len(env) == 0 {
		z := []uint16{0, 0}
		return &z[0]
	}
	total := 0
	for _, s := range env {
		total += len(s) + 1
	}
	total++ // trailing null
	buf := make([]byte, total)
	i := 0
	for _, s := range env {
		copy(buf[i:], s)
		i += len(s)
		buf[i] = 0
		i++
	}
	buf[i] = 0
	u := utf16.Encode([]rune(string(buf)))
	return &u[0]
}

// composeCommandLine joins path + args using Windows argv-escaping rules.
// .cmd / .bat shims (e.g. npm-installed CLIs) can't be executed directly
// by CreateProcess, so they get wrapped with cmd.exe /c.
func composeCommandLine(path string, args []string) string {
	low := strings.ToLower(path)
	if strings.HasSuffix(low, ".cmd") || strings.HasSuffix(low, ".bat") {
		wrapped := append([]string{"/c", path}, args...)
		return composeCommandLine(findCmdExe(), wrapped)
	}
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, syscall.EscapeArg(path))
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

func findCmdExe() string {
	if v := os.Getenv("ComSpec"); v != "" {
		return v
	}
	return `C:\Windows\System32\cmd.exe`
}

func newBackend(cfg PTYConfig, w, h int) (backend, error) {
	if !isConPtyAvailable() {
		return nil, fmt.Errorf("ConPTY is not available on this version of Windows")
	}

	// Two anonymous pipes. cmdIn/ptyIn: we write, ConPTY reads → child stdin.
	// ptyOut/cmdOut: child writes → ConPTY → we read.
	var cmdIn, ptyIn, ptyOut, cmdOut windows.Handle
	if err := windows.CreatePipe(&ptyIn, &cmdIn, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe(in): %w", err)
	}
	if err := windows.CreatePipe(&cmdOut, &ptyOut, nil, 0); err != nil {
		_ = windows.CloseHandle(ptyIn)
		_ = windows.CloseHandle(cmdIn)
		return nil, fmt.Errorf("CreatePipe(out): %w", err)
	}

	size := coord{X: int16(w), Y: int16(h)}

	// Default (flag=0) translating mode. PSEUDOCONSOLE_PASSTHROUGH_MODE (0x8)
	// is accepted by Windows 11 but the system-wide ConPTY doesn't implement
	// it the way microsoft/terminal's internal ConPTY does, and enabling it
	// produces significantly worse rendering for TUIs like Claude Code than
	// the translating default. Left as a future experiment.
	hpc, err := createPseudoConsole(size, ptyIn, ptyOut, 0)
	if err != nil {
		_ = windows.CloseHandle(ptyIn)
		_ = windows.CloseHandle(ptyOut)
		_ = windows.CloseHandle(cmdIn)
		_ = windows.CloseHandle(cmdOut)
		return nil, err
	}

	cmdLine := composeCommandLine(cfg.Path, cfg.Args)
	pi, err := spawnProcessAttached(hpc, cmdLine, cfg.Dir, cfg.Env)
	if err != nil {
		closePseudoConsole(hpc)
		_ = windows.CloseHandle(ptyIn)
		_ = windows.CloseHandle(ptyOut)
		_ = windows.CloseHandle(cmdIn)
		_ = windows.CloseHandle(cmdOut)
		return nil, err
	}

	return &windowsBackend{
		hpc:    hpc,
		pi:     pi,
		cmdIn:  cmdIn,
		cmdOut: cmdOut,
		ptyIn:  ptyIn,
		ptyOut: ptyOut,
	}, nil
}

func (b *windowsBackend) Read(p []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(b.cmdOut, p, &n, nil)
	return int(n), err
}

func (b *windowsBackend) Write(p []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(b.cmdIn, p, &n, nil)
	return int(n), err
}

func (b *windowsBackend) Resize(w, h int) error {
	return resizePseudoConsole(b.hpc, coord{X: int16(w), Y: int16(h)})
}

func (b *windowsBackend) Wait() error {
	_, err := windows.WaitForSingleObject(b.pi.Process, windows.INFINITE)
	if err != nil {
		return err
	}
	_ = windows.GetExitCodeProcess(b.pi.Process, &b.exitCode)
	b.exited = true
	return nil
}

func (b *windowsBackend) Close() error {
	closePseudoConsole(b.hpc)
	_ = windows.CloseHandle(b.pi.Process)
	_ = windows.CloseHandle(b.pi.Thread)
	_ = windows.CloseHandle(b.ptyIn)
	_ = windows.CloseHandle(b.ptyOut)
	_ = windows.CloseHandle(b.cmdIn)
	_ = windows.CloseHandle(b.cmdOut)
	return nil
}

func (b *windowsBackend) Kill() {
	_ = windows.TerminateProcess(b.pi.Process, 1)
}

// postStart is a no-op on Windows. Various tricks (auto /clear, resize
// toggle, VT clear escape) were tried to work around ConPTY ghost artifacts
// from dismissed modals; each caused worse problems than the one small
// artifact they attempted to fix. Accepted as a known limitation.
func postStart(p *PTY) {}

// watchResize polls the parent TTY size at ~5 Hz. Windows has no SIGWINCH.
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
