package main

import "encoding/json"

// JSON-RPC 2.0 envelope types.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// MCP handshake.

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// MCP tools.

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolsCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Tool argument + result shapes.

type runArgs struct {
	Prompt string `json:"prompt"`
}

type runResult struct {
	TurnID string `json:"turn_id"`
	Result string `json:"result"`
}

type submitArgs struct {
	TurnID string `json:"turn_id"`
	Result string `json:"result"`
}
