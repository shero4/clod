# clod

`clod` wraps a live Claude Code session so a remote Claude — Claude Desktop on
your laptop, the API, anything that speaks MCP — can drive it. The remote
sends a prompt; it appears in your terminal as if you typed it; your Claude
answers with its full toolbelt; the answer comes back.

One static Go binary. Two small dependencies. No state directory, no config
files, no subcommands.

## Use case

You debug code that runs on a different machine than the one you type on. A
production box, a GPU host, a staging VM — somewhere a file browser can't go.
Until now that meant copy-pasting prompts between Claude Desktop and a
`claude` session on the remote box, with you as the clipboard.

clod removes you from the middle. The Claude on your laptop calls
`mcp__clod__run`; the Claude on the remote box runs the prompt with its real
filesystem, real processes, real logs, and returns the answer.

## Install

Pre-built binaries (Linux and macOS, amd64/arm64) are attached to each
[release](https://github.com/shero4/clod/releases).

```sh
# replace OS/ARCH for your target
curl -L https://github.com/shero4/clod/releases/latest/download/clod-linux-amd64 -o clod
chmod +x clod
sudo mv clod /usr/local/bin/
```

Or build from source (requires Go 1.25+):

```sh
git clone https://github.com/shero4/clod
cd clod
make build
```

You also need a working `claude` on `PATH`. Install it with
`npm i -g @anthropic-ai/claude-code` if you haven't.

## Run

```sh
clod
```

That's it. clod prints a `.mcp.json` snippet, then hands the terminal over to
a normal Claude Code session — same TUI, same slash commands, same hooks,
same `CLAUDE.md`, same existing MCP servers. Use it as you normally would.

On first launch clod generates a bearer token and persists it to
`~/.claude/clod/token`. Subsequent runs reuse that token so you don't have to
re-paste the config.

Copy the printed snippet into your remote Claude's `.mcp.json`:

```json
{
  "mcpServers": {
    "clod": {
      "type": "http",
      "url": "http://127.0.0.1:7710/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

For cross-machine access, replace `127.0.0.1` with the remote box's address
and pass `--bind 0.0.0.0` to clod (or put it behind a reverse proxy).

## The `run` tool

clod exposes a single tool to remote callers:

```jsonc
// tool: mcp__clod__run
// input
{ "prompt": "read main.go and list the exported symbols" }

// output
{
  "turn_id": "4aff2391d0a8",
  "result":  "main.go exports: main() ..."
}
```

Behind the scenes clod appends a short instruction block to your prompt
asking the child Claude to call `submit_result` once it's done. The block is
plainly visible in your terminal — it isn't hidden.

## Flags

```
--port N                 HTTP port (default 7710)
--bind ADDR              Bind address (default 127.0.0.1)
--token TOKEN            Override the persisted token for this run
--cwd DIR                Working directory for the child Claude (default $PWD)
--permission-mode MODE   Permission mode for the child claude
                         (default: dangerouslySkipPermissions)
--claude-args "..."      Extra args forwarded to the child claude
--version                Print version and exit
```

## Architecture

```
             your terminal (raw mode)
                     ↕ bytes
        ┌─────────────────────────────────────┐
        │  clod                               │
        │    • PTY master ↔ PTY slave         │ ──spawns── claude (unmodified TUI)
        │    • HTTP MCP server                │                       │
        │        /mcp          (external)     │                       │
        │        /mcp/internal (submit_result)│ ◀── tool call ────────┘
        │    • turn registry (channels)       │
        └─────────────────────────────────────┘
                     ↕ HTTP + bearer
                     remote Claude
```

claude runs inside a pseudo-terminal. clod proxies bytes between your real
TTY and the PTY master, so the child Claude behaves exactly as if you were
running it directly. When a remote `run` arrives, clod injects a
bracketed-paste escape sequence followed (after a 150 ms delay) by an Enter
keystroke. The child sees a normal pasted user message, executes it with all
available tools, and calls `mcp__clod__submit_result` as its final act. That
tool call unblocks the waiting HTTP request and the result is returned to
the caller.

Two MCP endpoints on the same HTTP server, separated by path:

- **`/mcp`** — external, exposes `run`. The remote Claude connects here.
- **`/mcp/internal`** — internal, exposes `submit_result`. The child Claude
  connects here (via `--mcp-config`).

## Security

- Bearer token required on every request; constant-time comparison.
- Default bind is `127.0.0.1`. Expose with `--bind 0.0.0.0` only on trusted
  networks or behind a reverse proxy with TLS.
- External and internal endpoints share one token. Fine for single-user
  boxes: anyone who can reach the server can reach both endpoints.
- The child Claude runs with `--dangerously-skip-permissions` by default.
  This matches the clod use case — you're already trusting the remote
  Claude's prompts — but you can override with `--permission-mode default`
  to get the normal approval flow for each tool call.

## Limitations

- **Keystroke races.** If you're mid-typing when a remote `run` fires, the
  injected paste interleaves with your input. In practice this hasn't been
  an issue; it's a real race though.
- **No streaming.** `run` blocks until `submit_result` fires or until a
  10-minute timeout elapses. Partial output isn't returned incrementally.
- **Single session.** One child Claude per clod process. Concurrent `run`
  calls queue inside the child's usual one-turn-at-a-time model.
- **Reliance on the child following instructions.** clod's return path
  depends on the child calling `submit_result`. Claude follows the
  `<clod_system>` block reliably in practice, but it's soft enforcement.
- **Unix only.** Linux and macOS are supported. Windows PTY handling (ConPTY)
  isn't wired up.

## Building

```sh
make build       # local binary, version from `git describe`
make release     # cross-compile matrix into dist/ with SHA256SUMS
make test        # go vet + compile check
```

The `release` target produces `clod-{linux,darwin}-{amd64,arm64}` binaries.

## Source layout

```
main.go     # flag parsing, boot, PTY spawn, shutdown
server.go   # HTTP MCP server, tool dispatch, turn lifecycle
pty.go      # PTY proxy, raw-mode handling, SIGWINCH forwarding
turns.go    # turn registry (in-memory, channel-based)
types.go    # JSON-RPC and MCP types
```

About 800 lines. The whole thing fits in one afternoon of reading.
