// Package mcpserver implements the `salvage mcp` subcommand (spec 0032): a
// Model Context Protocol (MCP) server that speaks JSON-RPC 2.0 over stdio and
// exposes a curated subset of Salvage commands as MCP tools, so an agent
// runtime can drive the restore/verify/attest loop with structured JSON in and
// out — never scraped human-readable CLI text.
//
// The protocol layer is hand-rolled on the standard library (newline-delimited
// JSON-RPC messages, the MCP stdio transport). Spec 0032 R10 keeps the module's
// dependency surface at stdlib + gopkg.in/yaml.v3; MCP is a small
// JSON-RPC-shaped protocol and a minimal conforming server needs nothing more.
// Adopting an MCP library is an explicit Open question in the spec and has NOT
// been taken here.
//
// Deliberately NOT exposed (spec 0032 R4 / Non-goals): `login`/`logout`
// (interactive human device flow — the server inherits whatever credential the
// environment already holds), `schedule` (emitting installable scheduling
// config for an agent to silently install wants a human in the loop), and
// `version` (folded into the initialize serverInfo metadata).
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// protocolVersions are the MCP protocol revisions this server accepts. When the
// client requests one of these, initialize echoes it back; otherwise the server
// answers with latestProtocolVersion and the client decides whether to proceed.
var protocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

const latestProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes used by the server.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
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

// server holds the per-connection state of one MCP session.
type server struct {
	out   io.Writer
	tools []toolDef
}

// Serve runs an MCP session: it reads newline-delimited JSON-RPC messages from
// r, dispatches them, and writes responses to w until r reaches EOF (the host
// closing the transport is the shutdown signal). It is non-interactive and
// inherits the process environment — including any credential referenced via
// SALVAGE_ATTEST_KEY / ~/.salvage/credentials (spec 0032 R1, R5).
func Serve(r io.Reader, w io.Writer) error {
	s := &server{out: w, tools: toolset()}
	sc := bufio.NewScanner(r)
	// Generous line budget: tool arguments are small, but be tolerant.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.handleMessage(line)
	}
	return sc.Err()
}

// handleMessage decodes and dispatches one JSON-RPC message.
func (s *server) handleMessage(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(nil, codeParseError, "parse error: not valid JSON")
		return
	}
	if req.Method == "" {
		if req.ID != nil {
			s.writeError(req.ID, codeInvalidRequest, "invalid request: missing method")
		}
		return
	}
	// Notifications (no id) never get a response (JSON-RPC 2.0). The
	// notifications this server receives (initialized, cancelled) need no
	// server-side action in a sequential stdio session.
	if req.ID == nil {
		return
	}

	switch req.Method {
	case "initialize":
		s.writeResult(req.ID, s.initializeResult(req.Params))
	case "ping":
		s.writeResult(req.ID, struct{}{})
	case "tools/list":
		s.writeResult(req.ID, map[string]any{"tools": s.listTools()})
	case "tools/call":
		s.handleToolCall(req)
	default:
		s.writeError(req.ID, codeMethodNotFound, fmt.Sprintf("method %q not found", req.Method))
	}
}

// initializeResult negotiates the protocol version and advertises the server.
func (s *server) initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	ver := latestProtocolVersion
	if protocolVersions[p.ProtocolVersion] {
		ver = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "salvage",
			"title":   "Salvage backup restore-verification",
			"version": serverVersion(),
		},
		"instructions": "Salvage proves a backup actually restores and works. " +
			"Tools return the versioned machine-readable JSON of the underlying CLI commands. " +
			"salvage_run and salvage_check execute a real restore into an isolated throwaway environment; " +
			"salvage_attest writes to the hosted append-only attestation ledger. " +
			"Every tool carries a read-only / restore-executing / mutating classification " +
			"in its _meta[\"salvage.sh/classification\"]. " +
			"A failing backup is a successful tool call whose payload says verdict \"fail\"; " +
			"tool errors mean the operation itself could not run.",
	}
}

// listTools renders the advertised tool set (spec 0032 R2–R4): name,
// description, argument JSON Schema, MCP behavior annotations, and the
// machine-readable read-only/restore-executing/mutating classification.
func (s *server) listTools() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.inputSchema(),
			"annotations": map[string]any{
				"title":           t.title,
				"readOnlyHint":    t.classification == classReadOnly,
				"destructiveHint": false, // nothing exposed deletes/overwrites durable state
				"idempotentHint":  t.classification == classReadOnly,
				"openWorldHint":   t.openWorld,
			},
			// The three-way classification spec 0032 R4 requires. readOnlyHint
			// alone cannot express "runs a restore but mutates no Salvage state",
			// so the precise class rides in _meta for hosts that gate on it.
			"_meta": map[string]any{
				"salvage.sh/classification": string(t.classification),
			},
		})
	}
	return out
}

// handleToolCall validates and executes one tools/call request.
func (s *server) handleToolCall(req rpcRequest) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.writeError(req.ID, codeInvalidParams, "invalid params: expected {name, arguments}")
		return
	}
	var def *toolDef
	for i := range s.tools {
		if s.tools[i].name == p.Name {
			def = &s.tools[i]
			break
		}
	}
	if def == nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name))
		return
	}

	outcome := def.handler(context.Background(), p.Arguments)
	result, err := buildCallResult(outcome)
	if err != nil {
		s.writeError(req.ID, codeInternalError, "internal error: could not encode tool result")
		return
	}
	s.writeResult(req.ID, result)
}

// buildCallResult renders a tool outcome as an MCP CallToolResult. Both the
// success payload and the error body pass through redaction before leaving the
// server (spec 0032 R6) — tool output feeds an LLM context window, so every
// field is treated as attacker-reachable.
//
// The operational-vs-verdict split (spec 0032 R7) is preserved here: a verdict
// fail arrives as outcome.payload (isError=false, the payload says
// verdict "fail"); an operational error arrives as outcome.err and becomes an
// MCP tool error (isError=true) with a structured reason.
func buildCallResult(o toolOutcome) (map[string]any, error) {
	red := o.red
	if red == nil {
		red = newRedactor(nil)
	}

	var payload any
	isError := false
	if o.err != nil {
		isError = true
		payload = map[string]any{"error": o.err}
	} else {
		payload = o.payload
	}

	clean, err := red.redactPayload(payload)
	if err != nil {
		return nil, err
	}
	text, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": clean,
		"isError":           isError,
	}, nil
}

func (s *server) writeResult(id json.RawMessage, result any) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) writeError(id json.RawMessage, code int, msg string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	// Error messages are held to the same secret-safety bar as results
	// (spec 0032 R6): redact before writing.
	msg = newRedactor(nil).redactString(msg)
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		// A response we built that cannot marshal is a programmer error; emit a
		// minimal internal error so the host is not left hanging.
		b = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	_, _ = s.out.Write(append(b, '\n'))
}
