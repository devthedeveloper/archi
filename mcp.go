package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// This file is the Model Context Protocol layer for `archi serve`: JSON-RPC
// 2.0 over newline-delimited JSON on stdin/stdout (the MCP stdio transport).
// It knows nothing about archi's domain — tool schemas and handlers live in
// cmd_serve.go. stdout carries protocol messages exclusively; every reused
// archi function keeps its human chatter on stderr (ui.go's contract), which
// MCP treats as logging.

// maxLineBytes caps one incoming request line; anything longer is answered
// with a -32700 parse error and the rest of the line drained.
const maxLineBytes = 16 << 20

// knownProtocolVersions are the MCP revisions this server can speak, newest
// first. An unknown or missing client version negotiates to the newest.
var knownProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// rpcRequest is one incoming JSON-RPC 2.0 message (request or notification).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent ⇒ notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// rpcResponse is one outgoing response line.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolDef couples a tool's wire description with its handler. handler is
// unexported so it never marshals into tools/list.
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	handler     toolHandler     `json:"-"`
}

// toolHandler runs one tool call. progress is nil when the client sent no
// progressToken. The returned text becomes the content (toolTextSep splits it
// into multiple items); err becomes isError:true with err.Error() as the
// text, except an *errInvalidArgs which surfaces as JSON-RPC -32602.
type toolHandler func(ctx context.Context, args json.RawMessage,
	progress func(message string, frac float64)) (string, error)

// toolTextSep separates multiple content items in a handler's returned text
// (archi_design returns the design document and a "Saved to …" note as two
// items). Handlers join with it; handleToolsCall splits on it.
const toolTextSep = "\x1e"

// errInvalidArgs marks a handler failure that is the caller's fault — the
// arguments failed their schema shape — so handleToolsCall reports JSON-RPC
// -32602 instead of an isError tool result.
type errInvalidArgs struct{ detail string }

func (e *errInvalidArgs) Error() string { return e.detail }

// mcpServer holds one stdio session's state. Requests are handled serially,
// in arrival order: every tool reads or mutates the same on-disk .archi
// state, so concurrency would race writes the CLI never races.
type mcpServer struct {
	root     string    // confinement root, absolute, symlink-resolved
	tools    []toolDef // registration order = tools/list order
	protocol string    // negotiated version
	ready    bool      // notifications/initialized seen
	out      *bufio.Writer
	outMu    sync.Mutex
	mk       func(name, model string, temp float64, maxTokens int) (Provider, error) // newProvider; swapped in tests
}

// errLineTooLong marks a request line that exceeded maxLineBytes.
var errLineTooLong = errors.New("request line too long")

// readLimitedLine reads one '\n'-terminated line of at most max bytes. On
// overflow it drains the rest of the line and returns errLineTooLong. A final
// unterminated line is returned along with the underlying error (io.EOF).
func readLimitedLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(buf)+len(frag) > max {
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			return nil, errLineTooLong
		}
		buf = append(buf, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}

// serve reads newline-delimited JSON-RPC from r and writes responses to w
// until EOF, a fatal read error, or ctx cancellation. Requests are handled
// serially, in arrival order. Shutdown is always clean (nil): the in-flight
// call finishes, the derived context is cancelled, and the caller exits 0.
func (s *mcpServer) serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.out = bufio.NewWriter(w)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	br := bufio.NewReaderSize(r, 64<<10)
	for ctx.Err() == nil {
		line, err := readLimitedLine(br, maxLineBytes)
		if errors.Is(err, errLineTooLong) {
			s.replyErr(nil, -32700, "request line too long",
				fmt.Sprintf("lines are capped at %d bytes", maxLineBytes))
			continue
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			switch {
			case trimmed[0] == '[':
				// JSON-RPC batch arrays were removed in protocol 2025-06-18.
				s.replyErr(nil, -32600, "batch requests not supported", nil)
			default:
				var req rpcRequest
				if uerr := json.Unmarshal(trimmed, &req); uerr != nil {
					s.replyErr(nil, -32700, "parse error", uerr.Error())
				} else {
					s.handle(ctx, req)
				}
			}
		}
		if err != nil {
			return nil
		}
	}
	return nil
}

// handle routes one parsed message. Notifications never get a response
// (unknown ones are ignored); requests other than initialize/ping are
// rejected until the client has sent notifications/initialized.
func (s *mcpServer) handle(ctx context.Context, req rpcRequest) {
	if len(req.ID) == 0 || string(req.ID) == "null" { // notification
		if req.Method == "notifications/initialized" {
			s.ready = true
		}
		return
	}
	switch req.Method {
	case "":
		s.replyErr(req.ID, -32600, "missing method", nil)
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			json.Unmarshal(req.Params, &p) // tolerated: a bad shape negotiates the default
		}
		s.protocol = negotiateProtocol(p.ProtocolVersion)
		s.reply(req.ID, map[string]any{
			"protocolVersion": s.protocol,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "archi", "version": version},
		})
	case "ping":
		s.reply(req.ID, map[string]any{})
	default:
		if !s.ready {
			s.replyErr(req.ID, -32600, "server not initialized", nil)
			return
		}
		switch req.Method {
		case "tools/list":
			s.reply(req.ID, map[string]any{"tools": s.tools})
		case "tools/call":
			s.handleToolsCall(ctx, req)
		default:
			s.replyErr(req.ID, -32601, "unknown method: "+req.Method, nil)
		}
	}
}

// handleToolsCall decodes a tools/call request, resolves the tool, applies
// the ARCHI_TIMEOUT cap, and wraps the handler's outcome. Malformed calls are
// JSON-RPC errors ("fix the request"); domain failures are isError:true tool
// results whose text names the fix, so the calling LLM can recover.
func (s *mcpServer) handleToolsCall(ctx context.Context, req rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		s.replyErr(req.ID, -32602, "invalid tools/call params", nil)
		return
	}
	var tool *toolDef
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			tool = &s.tools[i]
			break
		}
	}
	if tool == nil {
		s.replyErr(req.ID, -32602, "unknown tool: "+p.Name, nil)
		return
	}
	var progress func(message string, frac float64)
	if token := progressToken(req.Params); token != nil {
		progress = func(message string, frac float64) { s.notifyProgress(token, frac, message) }
	}
	cctx, cancel, err := requestContext(ctx)
	if err != nil { // invalid ARCHI_TIMEOUT: a domain failure that names the fix
		s.reply(req.ID, toolResult(err.Error(), true))
		return
	}
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			s.replyErr(req.ID, -32603, fmt.Sprintf("internal error in %s: %v", p.Name, r), nil)
		}
	}()
	text, herr := tool.handler(cctx, p.Arguments, progress)
	var bad *errInvalidArgs
	switch {
	case errors.As(herr, &bad):
		s.replyErr(req.ID, -32602, "invalid arguments for "+p.Name, bad.detail)
	case herr != nil:
		s.reply(req.ID, toolResult(herr.Error(), true))
	default:
		s.reply(req.ID, toolResultTexts(strings.Split(text, toolTextSep), false))
	}
}

// writeMsg marshals v, appends '\n', and flushes, under outMu so progress
// notifications never interleave mid-line with a response.
func (s *mcpServer) writeMsg(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.out.Write(data)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *mcpServer) reply(id json.RawMessage, result any) {
	s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: idOrNull(id), Result: result})
}

func (s *mcpServer) replyErr(id json.RawMessage, code int, msg string, data any) {
	s.writeMsg(rpcResponse{JSONRPC: "2.0", ID: idOrNull(id), Error: &rpcError{Code: code, Message: msg, Data: data}})
}

// notifyProgress emits a notifications/progress line, echoing the client's
// token verbatim (clients send strings or ints).
func (s *mcpServer) notifyProgress(token json.RawMessage, frac float64, message string) {
	s.writeMsg(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": map[string]any{
			"progressToken": token,
			"progress":      frac,
			"total":         1,
			"message":       message,
		},
	})
}

// idOrNull normalizes a missing request id to JSON null for the wire.
func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// negotiateProtocol picks the response protocolVersion: a known client
// version is echoed; anything else gets the newest supported. Pure.
func negotiateProtocol(client string) string {
	for _, v := range knownProtocolVersions {
		if client == v {
			return client
		}
	}
	return knownProtocolVersions[0]
}

// progressToken extracts params._meta.progressToken, nil when absent. Pure.
func progressToken(params json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return nil
	}
	var p struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	if len(p.Meta.ProgressToken) == 0 || string(p.Meta.ProgressToken) == "null" {
		return nil
	}
	return p.Meta.ProgressToken
}

// toolResult wraps text into the MCP tools/call result shape. Pure.
func toolResult(text string, isError bool) map[string]any {
	return toolResultTexts([]string{text}, isError)
}

// toolResultTexts wraps one or more text items into the result shape. Pure.
func toolResultTexts(texts []string, isError bool) map[string]any {
	items := make([]map[string]any, len(texts))
	for i, t := range texts {
		items[i] = map[string]any{"type": "text", "text": t}
	}
	return map[string]any{"content": items, "isError": isError}
}
