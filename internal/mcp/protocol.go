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

// protocolVersion is the Model Context Protocol revision this server speaks. It is echoed back to
// the client at initialize so a mismatched peer can refuse rather than guess.
const protocolVersion = "2024-11-05"

// JSON-RPC 2.0 error codes used here, from the specification.
const (
	// codeParse means the peer sent bytes that are not valid JSON.
	codeParse = -32700
	// codeInvalidRequest means the JSON is valid but is not a well-formed request.
	codeInvalidRequest = -32600
	// codeMethodNotFound means the method is not one this server implements.
	codeMethodNotFound = -32601
	// codeInternal means the server failed while handling a well-formed request.
	codeInternal = -32603
)

// maxLineBytes bounds one incoming message. A request larger than this is a peer fault or an attempt
// to exhaust memory, not a tool call worth reading.
const maxLineBytes = 4 << 20

// request is one incoming JSON-RPC message. A message with no ID is a notification, which the
// protocol says must not be answered.
type request struct {
	// JSONRPC is the protocol version string, always "2.0".
	JSONRPC string `json:"jsonrpc"`
	// ID identifies a request awaiting a reply; absent on a notification.
	ID json.RawMessage `json:"id,omitempty"`
	// Method names the operation.
	Method string `json:"method"`
	// Params carries the method's arguments, shaped per method.
	Params json.RawMessage `json:"params,omitempty"`
}

// response is one outgoing JSON-RPC reply. Exactly one of Result and Error is set.
type response struct {
	// JSONRPC is the protocol version string, always "2.0".
	JSONRPC string `json:"jsonrpc"`
	// ID echoes the request's id.
	ID json.RawMessage `json:"id"`
	// Result is the success payload.
	Result any `json:"result,omitempty"`
	// Error is the failure payload.
	Error *rpcError `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	// Code is the JSON-RPC error code.
	Code int `json:"code"`
	// Message is a short description.
	Message string `json:"message"`
}

// Server speaks the Model Context Protocol over a byte stream, dispatching tool calls to the
// SwitchTender API. It is used by one client over one stream and serializes its own writes.
type Server struct {
	// tools is the registry of callable tools, in listing order.
	tools []Tool
	// byName indexes tools for dispatch.
	byName map[string]Tool
	// name and version identify this server to the client at initialize.
	name, version string
	// mu serializes writes so a reply is never interleaved with another.
	mu sync.Mutex
	// out is where replies are written.
	out io.Writer
}

// NewServer returns a Server exposing tools, identifying itself with name and version.
func NewServer(name, version string, tools []Tool) *Server {
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return &Server{tools: tools, byName: byName, name: name, version: version}
}

// Serve reads newline-delimited JSON-RPC messages from in and writes replies to out until the input
// ends or ctx is canceled. It returns nil on a clean end of input, which is how a client disconnects.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		s.handleLine(ctx, line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read: %w", err)
	}
	return nil
}

// handleLine parses and dispatches one message, replying unless it is a notification.
func (s *Server) handleLine(ctx context.Context, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(nil, codeParse, "invalid JSON")
		return
	}
	// A notification carries no id and must not be answered, even when it fails.
	notification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.Method == "" {
		if !notification {
			s.writeError(req.ID, codeInvalidRequest, "method is required")
		}
		return
	}
	result, rerr := s.dispatch(ctx, req)
	if notification {
		return
	}
	if rerr != nil {
		s.writeError(req.ID, rerr.Code, rerr.Message)
		return
	}
	s.write(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

// dispatch routes one method to its handler.
func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			// Only tools are offered. Declaring no resources or prompts keeps the surface to the
			// governed actions this server exists to provide.
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": s.name, "version": s.version},
		}, nil
	// The cancellation notification's name is spelled the way the protocol spells it. It is a wire
	// value, not prose, so it cannot be Americanized without ceasing to match what clients send.
	case "notifications/initialized", "notifications/cancelled": //nolint:misspell // Protocol method name.
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolList()}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + req.Method}
	}
}

// toolList renders the registry in the shape tools/list returns.
func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]any{
			"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
		})
	}
	return out
}

// callTool runs one tool and wraps its output in the protocol's content envelope.
//
// A tool that fails returns a result with isError set rather than a JSON-RPC error, which is what the
// protocol asks for: the failure is the model's to read and act on, not a transport fault. A refusal
// by the API, such as an approval gate holding a run or authorization denying it, arrives here and is
// therefore reported to the agent as text it can reason about.
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var call struct {
		// Name is the tool to run.
		Name string `json:"name"`
		// Arguments carries the tool's inputs.
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, &rpcError{Code: codeInvalidRequest, Message: "invalid params"}
		}
	}
	tool, ok := s.byName[call.Name]
	if !ok {
		// An unknown tool is reported as a tool error, so a model that guessed a name reads the
		// refusal and can correct itself instead of the session failing at the transport.
		return errorResult("no such tool: " + call.Name), nil
	}
	text, err := tool.Run(ctx, call.Arguments)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}, nil
}

// errorResult renders a tool failure in the protocol's content envelope.
func errorResult(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

// write emits one reply as a single line.
func (s *Server) write(res response) {
	data, err := json.Marshal(res)
	if err != nil {
		// The only way marshaling fails here is a result this server built, so report it as an
		// internal error against the same id rather than dropping the reply and hanging the client.
		data, err = json.Marshal(response{
			JSONRPC: "2.0", ID: res.ID,
			Error: &rpcError{Code: codeInternal, Message: "could not encode the reply"},
		})
		if err != nil {
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

// writeError emits one error reply.
func (s *Server) writeError(id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}
