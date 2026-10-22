// Package mcp answers Model Context Protocol messages over the Streamable
// HTTP transport with JSON responses only: one JSON-RPC message in, one out,
// for a fixed set of tools. It knows nothing about who is calling or what a
// tool does; the caller authenticates the request and runs the tools.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
)

// ProtocolVersions are the protocol revisions served, newest first.
var ProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one tool as a client lists it. Every tool served is read-only.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Server answers messages for one set of tools.
type Server struct {
	Name, Version, Instructions string
	Tools                       []Tool
	// Call runs a listed tool and answers its text and whether it failed.
	Call func(ctx context.Context, name string, args json.RawMessage) (text string, isError bool)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Handle answers one message. A notification needs no answer: Handle then
// returns nil, which the transport answers with 202 Accepted.
func (s Server) Handle(ctx context.Context, body []byte) []byte {
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		return encode(nil, nil, &rpcError{codeInvalidRequest, "batches are not supported; send one message per request"})
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return encode(nil, nil, &rpcError{codeParseError, err.Error()})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return encode(req.ID, nil, &rpcError{codeInvalidRequest, `want {"jsonrpc":"2.0","method":...}`})
	}
	if len(req.ID) == 0 {
		return nil
	}
	result, err := s.dispatch(ctx, req)
	return encode(req.ID, result, err)
}

func (s Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := ProtocolVersions[0]
		if slices.Contains(ProtocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema, "annotations": map[string]any{"readOnlyHint": true}})
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, err.Error()}
		}
		if !slices.ContainsFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name }) {
			return nil, &rpcError{codeInvalidParams, "unknown tool " + p.Name}
		}
		text, isError := s.Call(ctx, p.Name, p.Arguments)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}, nil
	}
	return nil, &rpcError{codeMethodNotFound, "unknown method " + req.Method}
}

func encode(id json.RawMessage, result any, err *rpcError) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: result, Error: err})
	return out
}
