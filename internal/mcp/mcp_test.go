package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testClient returns a Client pointed at ts.
func testClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(ts.URL, "st_test_token", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return c
}

// TestToolSurfaceIsProposeAndRead is the security contract of this package: an agent may propose a
// run and read what happened, and may not approve its own work or reach a credential, an account, a
// token, a grant, or a policy. A tool added outside that boundary fails here, which is the point.
func TestToolSurfaceIsProposeAndRead(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	for _, opts := range []Options{{}, {AllowAdhoc: true}} {
		names := map[string]bool{}
		for _, tool := range Tools(testClient(t, ts), opts) {
			names[tool.Name] = true
		}
		// Nothing that releases work or widens the agent's reach may ever be exposed.
		for _, banned := range []string{
			"approve", "approve_run", "reject", "reject_run",
			"create_credential", "update_credential", "delete_credential", "list_credentials",
			"create_user", "create_token", "create_grant", "create_policy", "update_policy",
		} {
			if names[banned] {
				t.Errorf("tool %q is exposed; an agent must not release its own work or widen its reach",
					banned)
			}
		}
		for _, want := range []string{"list_templates", "propose_run", "get_run", "get_run_evidence"} {
			if !names[want] {
				t.Errorf("tool %q is missing from the surface", want)
			}
		}
	}

	// The ad-hoc tool is opt-in: a default deployment keeps the agent on the operator's menu.
	def := map[string]bool{}
	for _, tool := range Tools(testClient(t, ts), Options{}) {
		def[tool.Name] = true
	}
	if def["propose_adhoc_run"] {
		t.Error("the ad-hoc run tool is exposed by default, widening the agent past vetted templates")
	}
	opt := map[string]bool{}
	for _, tool := range Tools(testClient(t, ts), Options{AllowAdhoc: true}) {
		opt[tool.Name] = true
	}
	if !opt["propose_adhoc_run"] {
		t.Error("the ad-hoc run tool is missing when explicitly allowed")
	}
}

// TestEveryToolCallCarriesTheToken proves each tool reaches the product only as an authenticated API
// request. A call that skipped the bearer token would be a path around the authorization gate, the
// approval policy, and the audit append that make an agent's changes governable.
func TestEveryToolCallCarriesTheToken(t *testing.T) {
	t.Parallel()
	var seen int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer st_test_token" {
			t.Errorf("%s %s carried authorization %q, want the bearer token", r.Method, r.URL.Path, got)
		}
		seen++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	tools := Tools(testClient(t, ts), Options{AllowAdhoc: true})
	args := map[string]json.RawMessage{
		"propose_run":       json.RawMessage(`{"template_id":"tpl_1"}`),
		"get_run":           json.RawMessage(`{"run_id":"run_1"}`),
		"get_run_log":       json.RawMessage(`{"run_id":"run_1"}`),
		"get_run_evidence":  json.RawMessage(`{"run_id":"run_1"}`),
		"propose_adhoc_run": json.RawMessage(`{"playbook":"site.yml"}`),
	}
	for _, tool := range tools {
		if _, err := tool.Run(context.Background(), args[tool.Name]); err != nil {
			t.Errorf("%s returned %v", tool.Name, err)
		}
	}
	if seen != len(tools) {
		t.Errorf("%d API calls for %d tools; a tool reached the product another way", seen, len(tools))
	}
}

// TestRunIDIsConfinedToOnePathSegment proves an identifier from the model cannot walk to another
// endpoint. The value is untrusted text however plausible it looks, and a bare id concatenated into
// a path would turn a read tool into a request the operator never authorized.
func TestRunIDIsConfinedToOnePathSegment(t *testing.T) {
	t.Parallel()
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	var getRun Tool
	for _, tool := range Tools(testClient(t, ts), Options{}) {
		if tool.Name == "get_run" {
			getRun = tool
		}
	}
	// A traversal attempt aimed at the admin-only account list.
	args := json.RawMessage(`{"run_id":"../../v1/users"}`)
	if _, err := getRun.Run(context.Background(), args); err != nil {
		t.Fatalf("get_run returned %v", err)
	}
	if strings.Contains(gotPath, "/v1/users") {
		t.Errorf("path %q escaped the run endpoint, reaching another API surface", gotPath)
	}
	if !strings.HasPrefix(gotPath, "/v1/runs/") {
		t.Errorf("path %q left the runs endpoint", gotPath)
	}
}

// TestRefuseAdminToken proves the server declines to lend an agent admin authority. The rule that an
// agent holds an operator-bound token is what keeps it from approving its own work, and a rule
// enforced only in documentation is not enforced.
func TestRefuseAdminToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Status is what the admin-only probe endpoint answers.
		Status int
		// WantAdmin is whether the token should be judged an admin token.
		WantAdmin bool
		// WantErr is whether any error is expected.
		WantErr bool
	}{
		{Name: "admin token refused", Status: http.StatusOK, WantAdmin: true, WantErr: true},
		{Name: "operator token accepted", Status: http.StatusForbidden},
		{Name: "accounts disabled accepted", Status: http.StatusNotFound},
		{Name: "rejected token errors", Status: http.StatusUnauthorized, WantErr: true},
		{Name: "server fault errors", Status: http.StatusInternalServerError, WantErr: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/users" {
					t.Errorf("probed %q, want the admin-only account list", r.URL.Path)
				}
				w.WriteHeader(test.Status)
			}))
			defer ts.Close()

			err := testClient(t, ts).RefuseAdminToken(context.Background())
			if test.WantAdmin && err != ErrAdminToken {
				t.Errorf("error = %v, want ErrAdminToken", err)
			}
			if (err != nil) != test.WantErr {
				t.Errorf("error = %v, want error: %v", err, test.WantErr)
			}
		})
	}
}

// TestServerHandshakeAndToolCall drives the protocol the way a client does: initialize, list the
// tools, then call one, over the same stream.
func TestServerHandshakeAndToolCall(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"runs":[],"count":0}`))
	}))
	defer ts.Close()

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_runs","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope/nope"}`,
	}, "\n") + "\n")
	var out strings.Builder
	srv := NewServer("switchtender", "test", Tools(testClient(t, ts), Options{}))
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// The notification must not be answered, so four requests produce four replies.
	if len(lines) != 4 {
		t.Fatalf("got %d replies for 4 requests and 1 notification:\n%s", len(lines), out.String())
	}
	var initRes struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initRes); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if initRes.Result.ProtocolVersion != protocolVersion {
		t.Errorf("protocol version = %q, want %q", initRes.Result.ProtocolVersion, protocolVersion)
	}
	if !strings.Contains(lines[1], "propose_run") {
		t.Errorf("tools/list did not carry the tools:\n%s", lines[1])
	}
	if !strings.Contains(lines[2], `"content"`) {
		t.Errorf("tools/call did not return content:\n%s", lines[2])
	}
	// An unknown method is a JSON-RPC error, not a crash.
	if !strings.Contains(lines[3], `"code":-32601`) {
		t.Errorf("unknown method should answer method-not-found:\n%s", lines[3])
	}
}

// TestToolFailureIsReportedToTheModel proves an API refusal comes back as tool content the model can
// read and act on, not as a transport error that kills the session. An approval gate holding a run
// and an authorization denial both arrive this way.
func TestToolFailureIsReportedToTheModel(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not authorized for this template"}`))
	}))
	defer ts.Close()

	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
			`"params":{"name":"propose_run","arguments":{"template_id":"tpl_1"}}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ghost","arguments":{}}}` + "\n")
	var out strings.Builder
	srv := NewServer("switchtender", "test", Tools(testClient(t, ts), Options{}))
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d replies, want 2:\n%s", len(lines), out.String())
	}
	for i, want := range []string{"not authorized for this template", "no such tool"} {
		if !strings.Contains(lines[i], `"isError":true`) {
			t.Errorf("reply %d is not marked a tool error:\n%s", i, lines[i])
		}
		if !strings.Contains(lines[i], want) {
			t.Errorf("reply %d does not carry %q:\n%s", i, want, lines[i])
		}
	}
}

// TestProposeRunLabelsTheAgentAndItsReason proves a proposed run carries the agent marker and the
// model's stated reason, so the change register shows why an agent asked rather than only that it did.
func TestProposeRunLabelsTheAgentAndItsReason(t *testing.T) {
	t.Parallel()
	var body map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"run_1","status":"pending_approval"}`))
	}))
	defer ts.Close()

	var propose Tool
	for _, tool := range Tools(testClient(t, ts), Options{}) {
		if tool.Name == "propose_run" {
			propose = tool
		}
	}
	got, err := propose.Run(context.Background(),
		json.RawMessage(`{"template_id":"tpl_1","reason":"disk filling on web01"}`))
	if err != nil {
		t.Fatalf("propose_run error = %v", err)
	}
	labels, _ := body["labels"].(map[string]any)
	if labels["proposed_by"] != "mcp" {
		t.Errorf("labels = %v, want the agent marker", labels)
	}
	if labels["reason"] != "disk filling on web01" {
		t.Errorf("labels = %v, want the stated reason recorded", labels)
	}
	// The held status reaches the model, so it learns it cannot proceed on its own.
	if !strings.Contains(got, "pending_approval") {
		t.Errorf("result does not tell the model the run is held: %s", got)
	}
}

// TestNewClientRejectsBadInput covers the constructor's refusals.
func TestNewClientRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := NewClient("", "tok", time.Second); err == nil {
		t.Error("an empty server address was accepted")
	}
	if _, err := NewClient("https://host", "", time.Second); err == nil {
		t.Error("an empty token was accepted")
	}
	if _, err := NewClient("ftp://host", "tok", time.Second); err == nil {
		t.Error("a non-HTTP scheme was accepted")
	}
	// A bare host is treated as https, so a token never crosses a plain connection by omission.
	c, err := NewClient("switchtender.internal", "tok", time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if !strings.HasPrefix(c.base, "https://") {
		t.Errorf("base = %q, want https for a bare host", c.base)
	}
}

// TestGetRunLogReadsTextNotJSON proves the log tool reads the server's text/plain log as text. It
// used to decode the body as JSON, which failed on the first character of any real log, so the tool
// was unusable against the real API even though a JSON-returning mock made it look fine.
func TestGetRunLogReadsTextNotJSON(t *testing.T) {
	t.Parallel()
	const logText = "PLAY [all] ***\nTASK [ping] ***\nok: [web]\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run_1/logs" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(logText))
	}))
	defer ts.Close()

	tool := getRunLogTool(t, testClient(t, ts))
	out, err := tool.Run(context.Background(), json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("get_run_log returned an error on a text/plain log: %v", err)
	}
	if out != logText {
		t.Errorf("get_run_log = %q, want the raw log text", out)
	}
}

// TestGetRunLogEmptyIsExplained proves an empty log reads as a clear message rather than a blank
// string, which would once have been a JSON decode error on an empty body.
func TestGetRunLogEmptyIsExplained(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}))
	defer ts.Close()

	tool := getRunLogTool(t, testClient(t, ts))
	out, err := tool.Run(context.Background(), json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("get_run_log on an empty log returned %v", err)
	}
	if out == "" {
		t.Error("get_run_log returned an empty string for an empty log, want an explanation")
	}
}

// getRunLogTool returns the get_run_log tool for the client.
func getRunLogTool(t *testing.T, c *Client) Tool {
	t.Helper()
	for _, tool := range Tools(c, Options{}) {
		if tool.Name == "get_run_log" {
			return tool
		}
	}
	t.Fatal("get_run_log tool not found")
	return Tool{}
}
