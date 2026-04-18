package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	mcpProtocolVersion = "2025-06-18"
	serverName         = "clod"
	// internalServerName is what clod calls itself when registered INTO claude
	// via --mcp-config. Tool names inside claude become mcp__<name>__<tool>.
	internalServerName = "clod"
	runTimeout         = 10 * time.Minute
)

type Server struct {
	token string
	turns *TurnRegistry
	pty   *PTY // set via SetPTY after the child starts
}

func NewServer(token string, turns *TurnRegistry) *Server {
	return &Server{token: token, turns: turns}
}

// SetPTY installs the PTY handle used by the `run` tool to inject prompts
// into the live claude child. Must be called before any `run` call arrives.
func (s *Server) SetPTY(p *PTY) { s.pty = p }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.auth(s.handleExternal))
	mux.HandleFunc("/mcp/internal", s.auth(s.handleInternal))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		got := strings.TrimPrefix(h, prefix)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- MCP endpoints ---

func (s *Server) handleExternal(w http.ResponseWriter, r *http.Request) {
	s.handleMCP(w, r, s.dispatchExternal)
}

func (s *Server) handleInternal(w http.ResponseWriter, r *http.Request) {
	s.handleMCP(w, r, s.dispatchInternal)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request, dispatch func(*rpcRequest) (rpcResponse, bool)) {
	if r.Method == http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var batch []rpcRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			writeError(w, nil, -32700, "parse error")
			return
		}
		var out []rpcResponse
		for _, req := range batch {
			if resp, ok := dispatch(&req); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, nil, -32700, "parse error")
		return
	}
	resp, ok := dispatch(&req)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, resp)
}

// --- Dispatchers ---

func (s *Server) dispatchExternal(req *rpcRequest) (rpcResponse, bool) {
	return s.dispatchCommon(req, externalToolDefs(), s.callExternalTool)
}

func (s *Server) dispatchInternal(req *rpcRequest) (rpcResponse, bool) {
	return s.dispatchCommon(req, internalToolDefs(), s.callInternalTool)
}

func (s *Server) dispatchCommon(req *rpcRequest, tools []toolDef, callTool func(json.RawMessage) (toolsCallResult, error)) (rpcResponse, bool) {
	isNotification := len(req.ID) == 0
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = initializeResult{
			ProtocolVersion: mcpProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      serverInfo{Name: serverName, Version: version},
		}
	case "notifications/initialized", "notifications/cancelled":
		return rpcResponse{}, false
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = toolsListResult{Tools: tools}
	case "tools/call":
		res, err := callTool(req.Params)
		if err != nil {
			resp.Result = toolsCallResult{
				Content: []toolContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			}
		} else {
			resp.Result = res
		}
	default:
		if isNotification {
			return rpcResponse{}, false
		}
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	if isNotification {
		return rpcResponse{}, false
	}
	return resp, true
}

// --- Tool defs ---

func externalToolDefs() []toolDef {
	return []toolDef{
		{
			Name:        "run",
			Description: "Send a prompt to the live Claude Code session running inside clod. The prompt appears in that terminal session (visible to the operator) and runs with all normal Claude Code features (tools, subagents, hooks, MCP servers). Returns the final answer text.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string", "description": "The instruction or question to run."},
				},
				"required": []string{"prompt"},
			},
		},
	}
}

func internalToolDefs() []toolDef {
	return []toolDef{
		{
			Name:        "submit_result",
			Description: "Submit the final answer for an active clod-bridge turn. Call this exactly once at the very end of a clod-bridged task, with the turn_id from the clod_system instruction block and your complete final answer as result.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"turn_id": map[string]any{"type": "string"},
					"result":  map[string]any{"type": "string"},
				},
				"required": []string{"turn_id", "result"},
			},
		},
	}
}

// --- Tool calls ---

func (s *Server) callExternalTool(raw json.RawMessage) (toolsCallResult, error) {
	var call toolsCallParams
	if err := json.Unmarshal(raw, &call); err != nil {
		return toolsCallResult{}, fmt.Errorf("bad params: %w", err)
	}
	switch call.Name {
	case "run":
		var a runArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return toolsCallResult{}, fmt.Errorf("bad args: %w", err)
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return toolsCallResult{}, fmt.Errorf("prompt is required")
		}
		if s.pty == nil {
			return toolsCallResult{}, fmt.Errorf("clod: child claude not ready")
		}
		res, err := s.runTurn(a.Prompt)
		if err != nil {
			return toolsCallResult{}, err
		}
		return toolResultJSON(res), nil
	default:
		return toolsCallResult{}, fmt.Errorf("unknown tool: %s", call.Name)
	}
}

func (s *Server) callInternalTool(raw json.RawMessage) (toolsCallResult, error) {
	var call toolsCallParams
	if err := json.Unmarshal(raw, &call); err != nil {
		return toolsCallResult{}, fmt.Errorf("bad params: %w", err)
	}
	switch call.Name {
	case "submit_result":
		var a submitArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return toolsCallResult{}, fmt.Errorf("bad args: %w", err)
		}
		if a.TurnID == "" {
			return toolsCallResult{}, fmt.Errorf("turn_id is required")
		}
		if err := s.turns.Complete(a.TurnID, a.Result); err != nil {
			return toolsCallResult{}, err
		}
		return toolResultJSON(map[string]any{"ok": true}), nil
	default:
		return toolsCallResult{}, fmt.Errorf("unknown tool: %s", call.Name)
	}
}

// --- Turn lifecycle ---

func (s *Server) runTurn(prompt string) (*runResult, error) {
	turn := s.turns.Create()
	defer s.turns.Forget(turn.ID)

	paste := buildInjectedPrompt(prompt, turn.ID)
	if err := s.pty.Inject(paste); err != nil {
		return nil, fmt.Errorf("inject prompt: %w", err)
	}
	// Let the TUI finish ingesting the paste (bracketed-paste end marker)
	// before we submit. Without this, Enter can get coalesced with the paste
	// and land in the input field instead of sending.
	time.Sleep(150 * time.Millisecond)
	if err := s.pty.Inject("\r"); err != nil {
		return nil, fmt.Errorf("inject submit: %w", err)
	}

	text, err := turn.Wait(runTimeout)
	if err != nil {
		return nil, err
	}
	return &runResult{TurnID: turn.ID, Result: text}, nil
}

// buildInjectedPrompt wraps the user's prompt with a clod_system instruction
// telling claude to submit its final answer via mcp__clod__submit_result.
// Uses bracketed-paste escape sequences so the TUI treats it as a paste
// (preserving newlines) and then \r submits.
func buildInjectedPrompt(userPrompt, turnID string) string {
	const pasteStart = "\x1b[200~"
	const pasteEnd = "\x1b[201~"
	body := fmt.Sprintf(`%s

<clod_system>
This task was submitted via the clod MCP bridge from a remote Claude.
When you have completed your response, call the tool `+"`mcp__clod__submit_result`"+` with:
  turn_id = "%s"
  result  = <your complete final answer as plain text>
Call this tool exactly once, at the very end, after everything else is done.
</clod_system>`, userPrompt, turnID)
	return pasteStart + body + pasteEnd
}

// --- HTTP helpers ---

func toolResultJSON(v any) toolsCallResult {
	b, _ := json.Marshal(v)
	return toolsCallResult{Content: []toolContent{{Type: "text", Text: string(b)}}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}
