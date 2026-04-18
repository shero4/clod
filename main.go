package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	var (
		showVersion    = flag.Bool("version", false, "Print version and exit")
		port           = flag.Int("port", 7710, "Port to listen on")
		bind           = flag.String("bind", "127.0.0.1", "Bind address")
		token          = flag.String("token", "", "Bearer token (default: persisted random)")
		cwd            = flag.String("cwd", "", "Working directory for claude (default: $PWD)")
		permissionMode = flag.String("permission-mode", "dangerouslySkipPermissions", "Permission mode for claude")
		extraArgsStr   = flag.String("claude-args", "", "Extra args to pass to claude (space-separated)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("clod %s\n", version)
		return
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: claude CLI not found on PATH")
		os.Exit(1)
	}

	if *token == "" {
		t, err := loadOrCreateToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: token:", err)
			os.Exit(1)
		}
		*token = t
	}

	if *cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: getwd:", err)
			os.Exit(1)
		}
		*cwd = wd
	}

	turns := NewTurnRegistry()
	srv := NewServer(*token, turns)

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start HTTP first so claude's --mcp-config can connect immediately and
	// so the user can curl /healthz during the banner stage if they want.
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Stage 1: print connection info and wait for the user to copy it.
	printBanner(claudePath, *bind, *port, *token, *permissionMode, version)
	waitForEnter()

	// Stage 2: clear the screen and hand off to claude.
	clearScreen()

	// Build --mcp-config JSON pointing at our *internal* endpoint so the child
	// claude can call submit_result back into clod.
	internalURL := fmt.Sprintf("http://%s/mcp/internal", addr)
	mcpConfigJSON := buildInternalMCPConfig(internalURL, *token)

	// Compose the claude command line. Interactive by default (no --print).
	args := []string{
		"--mcp-config", mcpConfigJSON,
	}
	switch *permissionMode {
	case "dangerouslySkipPermissions", "":
		args = append(args, "--dangerously-skip-permissions")
	default:
		args = append(args, "--permission-mode", *permissionMode)
	}
	if *extraArgsStr != "" {
		args = append(args, strings.Fields(*extraArgsStr)...)
	}

	p, err := StartPTY(PTYConfig{
		Path: claudePath,
		Args: args,
		Dir:  *cwd,
		Env:  os.Environ(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: start claude pty:", err)
		_ = httpSrv.Shutdown(context.Background())
		os.Exit(1)
	}
	srv.SetPTY(p)

	// Wait for claude to exit, or for the server to error out catastrophically.
	waitCh := make(chan error, 1)
	go func() { waitCh <- p.Wait() }()

	select {
	case <-waitCh:
		// claude exited (user /exit or Ctrl+D). Shut down.
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, "server error:", err)
		p.Kill()
		<-waitCh
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// buildInternalMCPConfig returns a JSON string suitable for `claude --mcp-config`
// that registers clod (the internal-endpoint) as an MCP server inside the child.
func buildInternalMCPConfig(url, token string) string {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"clod": map[string]any{
				"type": "http",
				"url":  url,
				"headers": map[string]any{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// printBanner writes the stage-1 screen: connection details the user needs
// to paste into their remote Claude's .mcp.json, plus a press-Enter prompt.
// Called before we enter raw mode, so plain Println is fine.
func printBanner(claudePath, bind string, port int, token, permissionMode, ver string) {
	host := bind
	if host == "0.0.0.0" || host == "::" {
		host = "<this-machine-ip>"
	}
	line := "\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500"

	fmt.Println()
	fmt.Printf("  clod %s\n", ver)
	fmt.Println()
	fmt.Printf("  claude binary    %s\n", claudePath)
	fmt.Printf("  MCP server       http://%s:%d/mcp\n", host, port)
	fmt.Printf("  permission mode  %s\n", permissionMode)
	fmt.Println()
	fmt.Println("  paste into your remote Claude's .mcp.json:")
	fmt.Println()
	fmt.Println("  " + line)
	fmt.Printf("    \"clod\": {\n")
	fmt.Printf("      \"type\": \"http\",\n")
	fmt.Printf("      \"url\": \"http://%s:%d/mcp\",\n", host, port)
	fmt.Printf("      \"headers\": {\n")
	fmt.Printf("        \"Authorization\": \"Bearer %s\"\n", token)
	fmt.Printf("      }\n")
	fmt.Printf("    }\n")
	fmt.Println("  " + line)
	fmt.Println()
	fmt.Print("  Press Enter to launch Claude Code...")
}

// waitForEnter blocks until the user presses Enter. We read byte-at-a-time
// on os.Stdin rather than going through bufio, because on some Windows
// console configurations bufio's lookahead can observe end-of-input
// prematurely and return without waiting.
func waitForEnter() {
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil {
			return
		}
		if n == 1 && b[0] == '\n' {
			return
		}
	}
}

// clearScreen emits the standard VT "clear screen + home cursor" sequence.
// Supported by all VT-compatible terminals, including Windows Terminal and
// PowerShell 7.
func clearScreen() {
	fmt.Print("\x1b[2J\x1b[H")
}

// --- token persistence ---

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func tokenPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "clod", "token"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "clod", "token"), nil
}

func loadOrCreateToken() (string, error) {
	p, err := tokenPath()
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(p); err == nil {
		t := strings.TrimSpace(string(data))
		if t != "" {
			return t, nil
		}
	}
	t := randToken()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", fmt.Errorf("create token dir: %w", err)
	}
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	fmt.Printf("\u2713 new token saved to %s\n", p)
	return t, nil
}
