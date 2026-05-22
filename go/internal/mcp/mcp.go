// Package mcp implements a minimal MCP (Model Context Protocol) server
// over stdio. JSON-RPC 2.0 envelope, line-delimited.
//
// We implement the protocol directly rather than depend on a third-party
// MCP SDK. Reasons (consistent with claude-env-sync's stack policy):
//
//   - Zero npm/Go third-party deps. The MCP wire format is small enough
//     (~150 LoC) that the maintenance cost is lower than the supply-chain
//     surface a community SDK would add.
//   - We only need server-side: initialize, tools/list, tools/call. No
//     resources, prompts, sampling, or completions in PoC scope.
//
// Specification reference: https://spec.modelcontextprotocol.io/
// Implemented protocol version: "2025-03-26" (current at writing).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the MCP spec version we declare in `initialize`.
const ProtocolVersion = "2025-03-26"

// JSON-RPC 2.0 standard error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// ─── Wire types ─────────────────────────────────────────────────────────

// request is the JSON-RPC envelope for an incoming message. ID is nil for
// notifications (one-way, no response expected).
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is what we send back. Either Result or Error is non-nil per
// JSON-RPC 2.0 — never both.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ─── Public Tool interface ─────────────────────────────────────────────

// Tool is the contract for a single MCP tool. Implementations are
// registered with Server.RegisterTool before Run.
type Tool interface {
	// Name is the tool identifier exposed to the client. Convention:
	// snake_case, namespaced (e.g. "kv_search").
	Name() string

	// Description is the short tool description shown in tools/list. The
	// MCP client uses this when deciding when to call the tool.
	Description() string

	// InputSchema returns the JSON Schema for the tool's arguments. Any
	// valid JSON Schema fragment is accepted; nil means "no arguments".
	InputSchema() json.RawMessage

	// Call executes the tool. raw is the raw JSON of the params.arguments
	// field — implementations unmarshal into their own struct. The
	// returned output is wrapped in MCP's [{type: "text", text: ...}]
	// shape; isError signals tool-level failure (vs JSON-RPC error).
	Call(ctx context.Context, raw json.RawMessage) (output string, isError bool, err error)
}

// ─── Server ────────────────────────────────────────────────────────────

// Server holds registered tools and drives the stdio message loop.
// Concurrency: Run is single-threaded (one request at a time) — MCP stdio
// transport assumes serial message handling. RegisterTool is goroutine-
// safe so callers can register from init() in any order.
type Server struct {
	Name    string // server name reported in initialize.serverInfo
	Version string // server version reported in initialize.serverInfo

	mu    sync.RWMutex
	tools map[string]Tool
}

// New returns an empty Server. Set Name and Version before Run.
func New(name, version string) *Server {
	return &Server{
		Name:    name,
		Version: version,
		tools:   make(map[string]Tool),
	}
}

// RegisterTool adds t. Re-registering a name replaces the previous Tool.
func (s *Server) RegisterTool(t Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name()] = t
}

// Run blocks on the stdio loop until in returns EOF or ctx is canceled.
//
// Each line on in is one JSON-RPC message. Each response is written to
// out as one line (line-delimited JSON, per MCP stdio transport).
//
// Errors from individual messages become JSON-RPC error responses;
// transport-level errors (broken pipe, JSON parse failure on read)
// return from Run.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// MCP messages can be large (tool list with rich schemas, tool call
	// outputs). Default 64KB is too small. Cap at 16 MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	enc := json.NewEncoder(out)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.dispatch(ctx, line, enc)
	}
	return scanner.Err()
}

// dispatch parses one request line and writes one response. Errors during
// parse → JSON-RPC error response. Notifications (id missing) get no
// response per spec.
func (s *Server) dispatch(ctx context.Context, line []byte, enc *json.Encoder) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(enc, nil, codeParseError, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeError(enc, req.ID, codeInvalidRequest, "jsonrpc must be \"2.0\"")
		return
	}

	// Notifications have no ID; we never respond to them. Spec section 5.4.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		result := s.handleInitialize()
		if !isNotification {
			s.writeResult(enc, req.ID, result)
		}
	case "notifications/initialized":
		// Client signaled ready. No response expected.
	case "tools/list":
		result := s.handleToolsList()
		if !isNotification {
			s.writeResult(enc, req.ID, result)
		}
	case "tools/call":
		result, rpcErr := s.handleToolsCall(ctx, req.Params)
		if isNotification {
			return
		}
		if rpcErr != nil {
			s.writeRPCError(enc, req.ID, rpcErr)
			return
		}
		s.writeResult(enc, req.ID, result)
	case "ping":
		// Heartbeat — empty result.
		if !isNotification {
			s.writeResult(enc, req.ID, struct{}{})
		}
	default:
		if !isNotification {
			s.writeError(enc, req.ID, codeMethodNotFound, "method not found: "+req.Method)
		}
	}
}

// ─── Method handlers ───────────────────────────────────────────────────

func (s *Server) handleInitialize() map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"serverInfo": map[string]string{
			"name":    s.Name,
			"version": s.Version,
		},
		"capabilities": map[string]any{
			// We support tools only. Empty object = "yes, this category".
			"tools": map[string]any{},
		},
	}
}

type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

func (s *Server) handleToolsList() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	descriptors := make([]toolDescriptor, 0, len(s.tools))
	for _, t := range s.tools {
		descriptors = append(descriptors, toolDescriptor{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return map[string]any{"tools": descriptors}
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) handleToolsCall(ctx context.Context, raw json.RawMessage) (map[string]any, *rpcError) {
	var params callParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "params: " + err.Error()}
	}
	if params.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tool name is required"}
	}

	s.mu.RLock()
	tool, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool: " + params.Name}
	}

	output, isError, err := tool.Call(ctx, params.Arguments)
	if err != nil {
		// Transport-level (panic-ish) failures. Surface as JSON-RPC error.
		return nil, &rpcError{Code: codeInternalError, Message: err.Error()}
	}

	return map[string]any{
		"content": []contentBlock{{Type: "text", Text: output}},
		"isError": isError,
	}, nil
}

// ─── Response writers ──────────────────────────────────────────────────

// writeResult writes a success response. id may be nil for system-generated
// errors that have no associated request id (parse errors).
func (s *Server) writeResult(enc *json.Encoder, id json.RawMessage, result any) {
	if err := enc.Encode(response{JSONRPC: "2.0", ID: id, Result: result}); err != nil {
		// Best-effort: nothing useful to do if we can't write the reply.
		return
	}
}

func (s *Server) writeError(enc *json.Encoder, id json.RawMessage, code int, msg string) {
	s.writeRPCError(enc, id, &rpcError{Code: code, Message: msg})
}

func (s *Server) writeRPCError(enc *json.Encoder, id json.RawMessage, rpcErr *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	_ = enc.Encode(response{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

// ─── Helpers for tool implementations ──────────────────────────────────

// UnmarshalArgs is a convenience for tool implementations: unmarshal the
// raw arguments JSON into the given struct, returning a user-facing
// error on malformed input.
func UnmarshalArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ErrToolUsage is the conventional error returned by Tool.Call when the
// caller's arguments are wrong (vs an internal failure). Wrap with
// fmt.Errorf("%w: ...", ErrToolUsage, ...) for context.
var ErrToolUsage = errors.New("tool usage")
