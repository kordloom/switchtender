package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// probeTool returns a tool that records every call it receives, so a test can prove a request that
// should have been refused never reached the product.
type probeTool struct {
	// mu guards calls and args, which the server writes and the test reads.
	mu sync.Mutex
	// calls counts how many times the tool ran.
	calls int
	// args is the raw arguments of the most recent call.
	args string
}

// tool returns the registered Tool backed by this probe, under the given name.
func (p *probeTool) tool(name string) Tool {
	return Tool{
		Name: name, Description: "A probe.",
		InputSchema: object(map[string]any{"x": prop("string", "Anything.")}, nil),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.calls++
			p.args = string(args)
			return "ran", nil
		},
	}
}

// count returns how many times the probe ran.
func (p *probeTool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// serveLines feeds the joined lines through one Server and returns the reply lines it wrote, with
// the Serve error. It is the same path a client drives over stdio.
func serveLines(t *testing.T, tools []Tool, lines ...string) ([]string, error) {
	t.Helper()
	var out bytes.Buffer
	srv := NewServer("switchtender", "test", tools)
	err := srv.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out)
	text := strings.TrimRight(out.String(), "\n")
	if text == "" {
		return nil, err
	}
	return strings.Split(text, "\n"), err
}

// decodeReply parses one reply line into its parts, failing the test when it is not a JSON object.
func decodeReply(t *testing.T, line string) (id string, result json.RawMessage, rerr *rpcError) {
	t.Helper()
	var reply struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("reply %q is not JSON: %v", line, err)
	}
	if reply.JSONRPC != "2.0" {
		t.Errorf("reply %q does not declare jsonrpc 2.0", line)
	}
	return string(reply.ID), reply.Result, reply.Error
}

// TestServerRefusesMalformedRequests pins what the stdio loop does with every shape of message a
// peer can put on the wire that is not a request this server implements.
//
// This is the agent-facing edge of the product. A malformed message must produce a JSON-RPC error
// against the right id, or no reply at all when it is a notification, and it must never reach a
// tool: a message the server could not parse is not an authorization the operator gave.
//
//nolint:funlen // Test function.
func TestServerRefusesMalformedRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Line is the raw message the peer sends.
		Line string
		// WantReplies is how many reply lines the server must write.
		WantReplies int
		// WantCode is the JSON-RPC error code, zero when the reply must carry a result instead.
		WantCode int
		// WantID is the id the reply must echo, checked when a reply is expected.
		WantID string
		// WantCalls is how many times a tool may run for this message.
		WantCalls int
	}{
		{ // Test 0: Bytes that are not JSON are a parse error against a null id.
			Name: "not json", Line: `{"jsonrpc":`, WantReplies: 1, WantCode: codeParse, WantID: "null",
		},
		{ // Test 1: A bare scalar is valid JSON but not a request object.
			Name: "scalar", Line: `42`, WantReplies: 1, WantCode: codeParse, WantID: "null",
		},
		{ // Test 2: A batch array is not supported and must not be read as a single request.
			Name: "batch array", Line: `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
			WantReplies: 1, WantCode: codeParse, WantID: "null",
		},
		{ // Test 3: A well-formed object with no method is an invalid request.
			Name: "no method", Line: `{"jsonrpc":"2.0","id":7}`,
			WantReplies: 1, WantCode: codeInvalidRequest, WantID: "7",
		},
		{ // Test 4: The same message without an id is a notification and must not be answered.
			Name: "no method no id", Line: `{"jsonrpc":"2.0"}`, WantReplies: 0,
		},
		{ // Test 5: An explicit null id is a notification too, whatever the method.
			Name: "null id", Line: `{"jsonrpc":"2.0","id":null,"method":"ping"}`, WantReplies: 0,
		},
		{ // Test 6: An unknown method is method-not-found, not a crash and not a tool call.
			Name: "unknown method", Line: `{"jsonrpc":"2.0","id":"a","method":"resources/read"}`,
			WantReplies: 1, WantCode: codeMethodNotFound, WantID: `"a"`,
		},
		{ // Test 7: Method names are matched exactly, so a different case is not the same method.
			Name: "wrong case", Line: `{"jsonrpc":"2.0","id":1,"method":"Tools/Call"}`,
			WantReplies: 1, WantCode: codeMethodNotFound, WantID: "1",
		},
		{ // Test 8: An unknown method sent as a notification is silently dropped.
			Name: "unknown notification", Line: `{"jsonrpc":"2.0","method":"resources/read"}`,
			WantReplies: 0,
		},
		{ // Test 9: Params that are not an object cannot name a tool, so the call is refused.
			Name:        "params not an object",
			Line:        `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":"probe"}`,
			WantReplies: 1, WantCode: codeInvalidRequest, WantID: "2",
		},
		{ // Test 10: A call naming no tool is a tool error the model can read, not a dropped request.
			Name: "no tool named", Line: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{}}`,
			WantReplies: 1, WantID: "3",
		},
		{ // Test 11: A tool that was never registered is refused by name.
			Name:        "unregistered tool",
			Line:        `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"approve_run"}}`,
			WantReplies: 1, WantID: "4",
		},
		{ // Test 12: A registered tool called with no arguments at all still runs.
			Name:        "registered tool",
			Line:        `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"probe"}}`,
			WantReplies: 1, WantID: "5", WantCalls: 1,
		},
		{ // Test 13: A string id is echoed as the string it was, not coerced to a number.
			Name: "string id", Line: `{"jsonrpc":"2.0","id":"req-1","method":"ping"}`,
			WantReplies: 1, WantID: `"req-1"`,
		},
		{ // Test 14: A missing jsonrpc member is tolerated rather than answered with a crash.
			Name: "no jsonrpc member", Line: `{"id":6,"method":"ping"}`, WantReplies: 1, WantID: "6",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			probe := &probeTool{}
			lines, err := serveLines(t, []Tool{probe.tool("probe")}, test.Line)
			if err != nil {
				t.Fatalf("Serve() error = %v, want a clean end of input", err)
			}
			if len(lines) != test.WantReplies {
				t.Fatalf("replies = %d, want %d for %s:\n%s",
					len(lines), test.WantReplies, test.Name, strings.Join(lines, "\n"))
			}
			if got := probe.count(); got != test.WantCalls {
				t.Errorf("the tool ran %d time(s), want %d for %s", got, test.WantCalls, test.Name)
			}
			if test.WantReplies == 0 {
				return
			}
			id, result, rerr := decodeReply(t, lines[0])
			if id != test.WantID {
				t.Errorf("reply id = %s, want %s", id, test.WantID)
			}
			switch {
			case test.WantCode != 0:
				if rerr == nil || rerr.Code != test.WantCode {
					t.Errorf("reply error = %+v, want code %d", rerr, test.WantCode)
				}
			default:
				if rerr != nil {
					t.Errorf("reply carries error %+v, want a result", rerr)
				}
				if len(result) == 0 {
					t.Errorf("reply carries no result:\n%s", lines[0])
				}
			}
		})
	}
}

// TestServerReportsAnUnknownToolToTheModel pins that guessing a tool name is answered as tool
// content rather than a transport error. A model that invented a name has to read the refusal to
// correct itself, and an error at the transport kills the session instead.
func TestServerReportsAnUnknownToolToTheModel(t *testing.T) {
	t.Parallel()
	probe := &probeTool{}
	lines, err := serveLines(t, []Tool{probe.tool("probe")},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve_run"}}`)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("replies = %d, want 1", len(lines))
	}
	if !strings.Contains(lines[0], `"isError":true`) {
		t.Errorf("an unknown tool is not marked a tool error:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], "no such tool: approve_run") {
		t.Errorf("the refusal does not name the tool the model guessed:\n%s", lines[0])
	}
	if probe.count() != 0 {
		t.Error("a call naming an unregistered tool ran a registered one")
	}
}

// TestNoMethodOtherThanToolsCallReachesATool is the dispatch half of this package's security
// contract. Every governed action is a tool, so a method that is not tools/call must never execute
// one: a peer that finds another route to a tool has found a route around the operator's tool set.
func TestNoMethodOtherThanToolsCallReachesATool(t *testing.T) {
	t.Parallel()
	methods := []string{
		"initialize", "ping", "tools/list", "notifications/initialized",
		//nolint:misspell // Protocol method name.
		"notifications/cancelled",
		"tools/run", "tools/execute", "resources/read", "resources/list", "prompts/get",
		"completion/complete", "logging/setLevel", "", "probe",
	}
	for testNum, method := range methods {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			probe := &probeTool{}
			line := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"name":"probe","arguments":{}}}`, method)
			if _, err := serveLines(t, []Tool{probe.tool("probe")}, line); err != nil {
				t.Fatalf("Serve() error = %v", err)
			}
			if probe.count() != 0 {
				t.Errorf("method %q reached a tool without going through tools/call", method)
			}
		})
	}
}

// TestInitializeOffersOnlyTools pins the advertised surface. The package deliberately declares no
// resources and no prompts, because the governed actions are the tools and anything else is a
// surface an operator did not agree to expose.
func TestInitializeOffersOnlyTools(t *testing.T) {
	t.Parallel()
	lines, err := serveLines(t, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	var reply struct {
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &reply); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if reply.Result.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", reply.Result.ProtocolVersion, protocolVersion)
	}
	wantCaps := map[string]any{"tools": map[string]any{}}
	if diff := cmp.Diff(wantCaps, reply.Result.Capabilities, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("capabilities mismatch (-want +got):\n%s", diff)
	}
	if reply.Result.ServerInfo.Name != "switchtender" || reply.Result.ServerInfo.Version != "test" {
		t.Errorf("serverInfo = %+v, want the name and version the constructor was given",
			reply.Result.ServerInfo)
	}
}

// TestToolsListPublishesNameDescriptionAndSchemaOnly pins what a listing hands a model. Order is the
// registry's order, which is how a model is steered to list_templates first, and no field beyond the
// three the protocol defines may leak out of the registry.
func TestToolsListPublishesNameDescriptionAndSchemaOnly(t *testing.T) {
	t.Parallel()
	first := &probeTool{}
	second := &probeTool{}
	lines, err := serveLines(t, []Tool{first.tool("alpha"), second.tool("beta")},
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	var reply struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &reply); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(reply.Result.Tools) != 2 {
		t.Fatalf("listed %d tools, want 2", len(reply.Result.Tools))
	}
	if reply.Result.Tools[0]["name"] != "alpha" || reply.Result.Tools[1]["name"] != "beta" {
		t.Errorf("listing order = %v, want the registry's order", reply.Result.Tools)
	}
	for _, tool := range reply.Result.Tools {
		var keys []string
		for k := range tool {
			keys = append(keys, k)
		}
		want := []string{"description", "inputSchema", "name"}
		if diff := cmp.Diff(want, sorted(keys), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("listed tool fields (-want +got):\n%s", diff)
		}
	}
}

// sorted returns a sorted copy of the strings.
func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestServerAnswersAStreamInOrderAndOnlyOnce pins that a session of many messages produces exactly
// one reply per request, in the order the requests arrived, with notifications answered by nothing.
// A client matches replies by id, so an extra or missing reply wedges it.
func TestServerAnswersAStreamInOrderAndOnlyOnce(t *testing.T) {
	t.Parallel()
	probe := &probeTool{}
	lines, err := serveLines(t, []Tool{probe.tool("probe")},
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		"",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`   `,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"probe","arguments":{"x":"y"}}}`,
		//nolint:misspell // Protocol method name.
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":4}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	// The blank line is skipped, the whitespace line is a parse error against a null id, and the two
	// notifications are answered by nothing.
	wantIDs := []string{"1", "2", "null", "3", "4", "5"}
	var gotIDs []string
	for _, line := range lines {
		id, _, _ := decodeReply(t, line)
		gotIDs = append(gotIDs, id)
	}
	if diff := cmp.Diff(wantIDs, gotIDs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("reply ids (-want +got):\n%s", diff)
	}
	if probe.count() != 1 {
		t.Errorf("the tool ran %d time(s) across the session, want 1", probe.count())
	}
	if probe.args != `{"x":"y"}` {
		t.Errorf("tool arguments = %q, want the call's arguments verbatim", probe.args)
	}
}

// TestServerStopsAtAnOversizedMessage pins the memory bound on one incoming message. A line past the
// cap is a peer fault or an attempt to exhaust the process, so it must be refused rather than
// buffered, and no tool may run from bytes the server would not read.
func TestServerStopsAtAnOversizedMessage(t *testing.T) {
	t.Parallel()
	probe := &probeTool{}
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"probe","arguments":{"x":"` +
		strings.Repeat("a", maxLineBytes) + `"}}}`
	lines, err := serveLines(t, []Tool{probe.tool("probe")}, huge,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if err == nil {
		t.Fatal("Serve() accepted a message past the size bound")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("Serve() error = %v, want the size bound to be named", err)
	}
	if probe.count() != 0 {
		t.Error("a tool ran from a message the server refused to read")
	}
	// The stream is abandoned at the fault, so nothing after the oversized message is answered.
	// A peer that overran the bound does not get a partly served session.
	if len(lines) != 0 {
		t.Errorf("replies after an oversized message = %d, want none:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
}

// TestServerAcceptsAMessageJustUnderTheSizeBound is the other side of the bound: a large but legal
// message is served. A cap that also refuses the largest allowed request is a cap that silently
// narrows the tool surface.
func TestServerAcceptsAMessageJustUnderTheSizeBound(t *testing.T) {
	t.Parallel()
	probe := &probeTool{}
	head := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"probe","arguments":{"x":"`
	tail := `"}}}`
	fill := maxLineBytes - len(head) - len(tail) - 1
	lines, err := serveLines(t, []Tool{probe.tool("probe")}, head+strings.Repeat("a", fill)+tail)
	if err != nil {
		t.Fatalf("Serve() refused a message at the bound: %v", err)
	}
	if len(lines) != 1 || probe.count() != 1 {
		t.Fatalf("replies = %d, tool calls = %d, want one of each", len(lines), probe.count())
	}
}

// TestServeStopsOnACanceledContext pins that a canceled session ends cleanly rather than draining
// whatever the peer still has queued. The loop reports nil, which is how a disconnect reads.
func TestServeStopsOnACanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probe := &probeTool{}
	var out bytes.Buffer
	srv := NewServer("switchtender", "test", []Tool{probe.tool("probe")})
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"probe"}}` + "\n")
	if err := srv.Serve(ctx, in, &out); err != nil {
		t.Errorf("Serve() on a canceled context = %v, want nil", err)
	}
	if probe.count() != 0 {
		t.Error("a tool ran after the session was canceled")
	}
	if out.Len() != 0 {
		t.Errorf("a canceled session wrote %q", out.String())
	}
}

// TestServeReturnsNilOnEmptyInput pins that a client connecting and disconnecting without saying
// anything is not an error. That is an ordinary disconnect, and reporting it as a fault would make
// every clean exit look like one.
func TestServeReturnsNilOnEmptyInput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	srv := NewServer("switchtender", "test", nil)
	if err := srv.Serve(context.Background(), strings.NewReader(""), &out); err != nil {
		t.Errorf("Serve() on empty input = %v, want nil", err)
	}
	if out.Len() != 0 {
		t.Errorf("an empty session wrote %q", out.String())
	}
}

// TestNewServerIndexesDuplicateNamesToTheLast pins what happens when a tool set carries two tools
// under one name. Dispatch takes the last registration, and the listing still shows both, so an
// operator who shadowed a tool can see it in the listing rather than only in behavior.
func TestNewServerIndexesDuplicateNamesToTheLast(t *testing.T) {
	t.Parallel()
	first := &probeTool{}
	second := &probeTool{}
	tools := []Tool{first.tool("probe"), second.tool("probe")}
	if _, err := serveLines(t, tools,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"probe"}}`); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if first.count() != 0 || second.count() != 1 {
		t.Errorf("calls = first %d, second %d, want the last registration to win",
			first.count(), second.count())
	}
}

// TestWriteSerializesConcurrentReplies pins the mutex the Server documents. Replies are newline
// delimited, so two writes interleaving would produce a line that is not a JSON object and wedge the
// client on the other side of the stream.
func TestWriteSerializesConcurrentReplies(t *testing.T) {
	t.Parallel()
	var out lockedBuffer
	srv := NewServer("switchtender", "test", nil)
	srv.out = &out
	const writers = 32
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			srv.write(response{
				JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", n)),
				Result: map[string]any{"n": n, "pad": strings.Repeat("x", 512)},
			})
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != writers {
		t.Fatalf("wrote %d lines for %d replies", len(lines), writers)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		id, _, _ := decodeReply(t, line)
		if seen[id] {
			t.Errorf("reply id %s was written twice", id)
		}
		seen[id] = true
	}
}

// lockedBuffer is a bytes.Buffer safe for the concurrent writes the reply mutex is meant to
// serialize, so the race detector reports the Server's own race rather than the buffer's.
type lockedBuffer struct {
	// mu guards buf.
	mu sync.Mutex
	// buf holds what was written.
	buf bytes.Buffer
}

// Write appends p under the lock.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWriteFallsBackWhenAResultCannotBeEncoded pins that a result this server cannot marshal is
// answered as an internal error against the same id rather than dropped. A dropped reply hangs the
// client forever waiting on an id that will never come back.
func TestWriteFallsBackWhenAResultCannotBeEncoded(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	srv := NewServer("switchtender", "test", nil)
	srv.out = &out
	// A channel cannot be marshaled, which is the only way the encode fails here.
	srv.write(response{JSONRPC: "2.0", ID: json.RawMessage("9"), Result: make(chan int)})
	line := strings.TrimSpace(out.String())
	id, _, rerr := decodeReply(t, line)
	if id != "9" {
		t.Errorf("fallback reply id = %s, want the request's id", id)
	}
	if rerr == nil || rerr.Code != codeInternal {
		t.Errorf("fallback reply error = %+v, want an internal error", rerr)
	}
}

// TestNotificationMethodWithAnIDAnswersWithNeitherResultNorError demonstrates a defect. JSON-RPC 2.0
// requires a response to carry exactly one of result and error. The two notification methods return
// a nil result, and the reply omits an empty result, so a client that sends notifications/initialized
// with an id, which several do, receives {"jsonrpc":"2.0","id":1} and a strict client rejects it.
func TestNotificationMethodWithAnIDAnswersWithNeitherResultNorError(t *testing.T) {
	t.Parallel()
	lines, err := serveLines(t, nil, `{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("replies = %d, want 1", len(lines))
	}
	_, result, rerr := decodeReply(t, lines[0])
	if len(result) == 0 && rerr == nil {
		t.Errorf("reply carries neither result nor error, which JSON-RPC 2.0 forbids:\n%s", lines[0])
	}
}
