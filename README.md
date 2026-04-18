# clod

Run a real Claude Code session on one machine, drive it from another. clod is
a small HTTP server that wraps `claude` and exposes a single MCP tool called
`run`. You call it from a remote Claude (Claude Desktop, the API, anything
that speaks MCP). The prompt lands in your local claude terminal as if you
pasted it. The answer comes back.

One static Go binary, two tiny dependencies, no config files.

## Why

I debug code that runs on machines I'm not sitting in front of. GPU hosts,
staging boxes, production servers. The usual flow was: open Claude Desktop
on my laptop, write a prompt, SSH into the box, paste it into `claude`,
wait, copy the answer back. I was the clipboard.

clod removes that step. Claude Desktop on my laptop calls `mcp__clod__run`.
The `claude` session on the remote box runs it against the actual
filesystem, processes, and logs, then returns the answer.

## Install

Grab a binary from [releases](https://github.com/shero4/clod/releases):

```sh
curl -L https://github.com/shero4/clod/releases/latest/download/clod-linux-amd64 -o clod
chmod +x clod && sudo mv clod /usr/local/bin/
```

Binaries: linux and macOS, amd64 and arm64. You also need `claude`
installed (`npm i -g @anthropic-ai/claude-code`).

## First run

```sh
clod
```

On first launch clod generates a bearer token and saves it to
`~/.claude/clod/token`. It prints a ready-to-paste `.mcp.json` snippet, then
hands the terminal over to `claude`. Everything works exactly like running
`claude` yourself: slash commands, hooks, `CLAUDE.md`, MCP servers, all of
it. clod is invisible once `claude` takes over.

Paste the printed snippet into your remote Claude's `.mcp.json`:

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

The `127.0.0.1` assumes both sides are on the same machine. Usually they
aren't.

## Running across machines

This is where clod earns its keep.

### Same LAN

On the remote box:

```sh
clod --bind 0.0.0.0
```

On your laptop, change the URL in `.mcp.json` to the box's LAN IP:

```json
"url": "http://192.168.1.50:7710/mcp"
```

If the remote has a firewall, open the port:

```sh
sudo ufw allow from 192.168.0.0/16 to any port 7710
```

Copy the token from the remote's `~/.claude/clod/token` into your laptop's
`.mcp.json`. Sanity check from the laptop:

```sh
curl http://192.168.1.50:7710/healthz
# ok
```

If that works, the MCP config will work.

### Tailscale or any WireGuard mesh

If you already run Tailscale, this is the cleanest option. Bind clod to the
Tailscale address and use the MagicDNS name:

```sh
clod --bind 100.x.y.z
```

```json
"url": "http://remote-box.tail1234.ts.net:7710/mcp"
```

You get identity-based ACLs and encrypted transport with zero extra config.

### Public internet

Don't bind publicly without TLS. Put Caddy or nginx in front, terminate
HTTPS there, and point `.mcp.json` at the `https://` URL. The bearer token
still authenticates the request but plain HTTP over the internet leaks it
on the wire.

## The `run` tool

One tool, one argument:

```jsonc
// mcp__clod__run
{ "prompt": "read main.go and list the exported symbols" }

// returns
{ "turn_id": "4aff2391d0a8", "result": "main.go exports: main() ..." }
```

clod appends a short instruction block to every prompt telling the child
claude to call `submit_result` when it's done. The block is plainly
visible in the terminal. If that makes you uneasy, read the source
(`buildInjectedPrompt` in `server.go`, 10 lines).

## Flags

```
--port N                 HTTP port (default 7710)
--bind ADDR              Bind address (default 127.0.0.1)
--token TOKEN            Override the persisted token for this run
--cwd DIR                Working directory for the child claude (default $PWD)
--permission-mode MODE   Permission mode for the child claude
                         (default: dangerouslySkipPermissions)
--claude-args "..."      Extra args forwarded to the child claude
--version                Print version and exit
```

## How it works

```
             your terminal (raw mode)
                     ↕ bytes
        ┌─────────────────────────────────────┐
        │  clod                               │
        │    PTY master ↔ PTY slave           │  spawns  claude (unmodified)
        │    HTTP MCP server                  │   ──▶              │
        │      /mcp          (external)       │                    │
        │      /mcp/internal (submit_result)  │  ◀── tool call ────┘
        │    turn registry                    │
        └─────────────────────────────────────┘
                     ↕ HTTP + bearer
                     remote Claude
```

`claude` runs inside a pseudo-terminal. clod pipes bytes in both directions
so the child thinks it has a real human on the keyboard. When a `run` call
arrives, clod writes a bracketed-paste escape sequence into the PTY, waits
150 ms, then writes Enter. The child sees a pasted user message, runs it,
and calls `mcp__clod__submit_result` as its final action. That MCP call
unblocks the waiting HTTP request. The answer returns to the caller.

Two MCP endpoints on one server:

- `/mcp`. External. Tool: `run`. Your remote Claude connects here.
- `/mcp/internal`. Internal. Tool: `submit_result`. The child claude
  connects here via `--mcp-config`.

Both endpoints share the same bearer token. That's fine on single-user
boxes: anyone who can reach the port can reach both endpoints anyway.

### Return channel, in plain words

There is no reverse connection. The child claude is a normal MCP client
making outbound HTTP to clod's own internal endpoint, using a `turn_id`
that clod baked into the prompt when it injected it. clod correlates the
inbound `run` request with the inbound `submit_result` call via that ID.
Two independent HTTP requests, one shared piece of memory. No push, no
streams, no sockets.

## Security

Bearer token required on every request, constant-time compare. Default bind
is loopback. Bind publicly only behind TLS.

The child claude runs with `--dangerously-skip-permissions` by default,
which matches how people actually use clod (the remote operator is you, or
someone you trust). If you want the normal tool-approval flow, pass
`--permission-mode default` and approve each tool call in your terminal.

## Limitations

- **Keystroke races.** If you're typing when a `run` arrives, the injected
  paste lands interleaved with your input. Rare in practice, not fixed.
- **No streaming.** `run` blocks until `submit_result` or a 10-minute
  timeout. Long turns can hit the client-side timeout in Claude Desktop
  before clod gives up.
- **Single session.** One child claude per clod process. Concurrent `run`
  calls serialize through the child's normal one-turn-at-a-time model.
- **Soft return path.** If the child ignores the `<clod_system>`
  instruction, `run` times out. I haven't seen this happen, but the only
  thing making it reliable is prompt-following.
- **Unix only.** Linux and macOS. Windows (ConPTY) isn't wired up yet.

## Building

```sh
make build     # local binary, version from `git describe`
make release   # cross-compile into dist/ with SHA256SUMS
make test      # go vet + compile check
```

## Source layout

```
main.go     flags, boot, PTY spawn, shutdown
server.go   HTTP MCP server, tool dispatch, turn lifecycle
pty.go      PTY proxy, raw mode, SIGWINCH forwarding
turns.go    in-memory turn registry, channel-based
types.go    JSON-RPC and MCP types
```

Around 800 lines. Readable in one sitting.
