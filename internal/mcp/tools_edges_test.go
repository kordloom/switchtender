package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// callRecord is one request a tool made, as the upstream saw it.
type callRecord struct {
	// Method is the HTTP method.
	Method string
	// Path is the escaped request path.
	Path string
	// Query is the raw query string.
	Query string
	// Body is the decoded request body, nil when there was none.
	Body map[string]any
	// Auth is the Authorization header.
	Auth string
}

// apiRecorder is an upstream that records every request and answers with a fixed reply, so a test
// can prove both what reached the product and that nothing did.
type apiRecorder struct {
	// mu guards calls, since the client writes it from the request goroutine.
	mu sync.Mutex
	// calls are the requests served, in order.
	calls []callRecord
	// reply is the body answered to every request.
	reply string
}

// ServeHTTP records the request and answers with the recorder's fixed reply.
func (a *apiRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := callRecord{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization")}
	if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &rec.Body)
	}
	a.mu.Lock()
	a.calls = append(a.calls, rec)
	a.mu.Unlock()
	reply := a.reply
	if reply == "" {
		reply = `{"id":"run_1","status":"pending_approval"}`
	}
	_, _ = io.WriteString(w, reply)
}

// records returns the requests served so far.
func (a *apiRecorder) records() []callRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]callRecord(nil), a.calls...)
}

// recordingTools stands a recorder behind a full tool set and returns both.
func recordingTools(t *testing.T, opts Options) (*apiRecorder, []Tool) {
	t.Helper()
	rec := &apiRecorder{}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	c, err := NewClient(ts.URL, "st_test_token", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return rec, Tools(c, opts)
}

// TestEveryToolRefusesBadArgumentsBeforeReachingTheProduct pins that a malformed tool call is
// stopped here rather than sent on.
//
// A request that leaves this package is an authenticated call the API authorizes and records. A call
// the tool could not understand is not one an operator asked for, so it must not become a request at
// all: sending it and letting the API sort it out puts a proposal the model did not intend into the
// audit chain, and lands a refusal that reads like an authorization problem instead of an argument
// problem.
//
//nolint:funlen // Test function.
func TestEveryToolRefusesBadArgumentsBeforeReachingTheProduct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Tool is the tool to call.
		Tool string
		// Args is the arguments a model supplied.
		Args string
		// WantMention is a substring the refusal has to carry.
		WantMention string
	}{
		{ // Test 0: A launch with no template names nothing to launch.
			Tool: "propose_run", Args: `{}`, WantMention: "template_id is required",
		},
		{ // Test 1: A blank template id is not a template id.
			Tool: "propose_run", Args: `{"template_id":"   "}`, WantMention: "template_id is required",
		},
		{ // Test 2: Absent arguments are the same refusal.
			Tool: "propose_run", Args: ``, WantMention: "template_id is required",
		},
		{ // Test 3: A withheld control is refused before anything is proposed.
			Tool: "propose_run", Args: `{"template_id":"tpl_1","extra_vars":{"a":1}}`,
			WantMention: "extra_vars",
		},
		{ // Test 4: A pattern that reaches every host is refused without reading the template.
			Tool: "propose_run", Args: `{"template_id":"tpl_1","limit":"all"}`,
			WantMention: "every host",
		},
		{ // Test 5: A read tool with no run id reads nothing.
			Tool: "get_run", Args: `{}`, WantMention: "run_id is required",
		},
		{ // Test 6: Nor with a blank one.
			Tool: "get_run", Args: `{"run_id":""}`, WantMention: "run_id is required",
		},
		{ // Test 7: The log tool refuses the same way.
			Tool: "get_run_log", Args: `{}`, WantMention: "run_id is required",
		},
		{ // Test 8: And the evidence tool.
			Tool: "get_run_evidence", Args: `{}`, WantMention: "run_id is required",
		},
		{ // Test 9: A listing with an undefined argument is refused rather than silently widened.
			Tool: "list_runs", Args: `{"page_size":10}`, WantMention: "page_size",
		},
		{ // Test 10: A listing whose limit is not a number is refused, not coerced.
			Tool: "list_runs", Args: `{"limit":"ten"}`, WantMention: "invalid arguments",
		},
		{ // Test 11: An ad-hoc proposal with neither a playbook nor a command proposes nothing.
			Tool: "propose_adhoc_run", Args: `{}`, WantMention: "playbook or a command is required",
		},
		{ // Test 12: Nor does one carrying only whitespace.
			Tool: "propose_adhoc_run", Args: `{"command":"  \n "}`,
			WantMention: "playbook or a command is required",
		},
		{ // Test 13: An undefined ad-hoc argument is refused before anything is submitted.
			Tool: "propose_adhoc_run", Args: `{"command":"echo hi","extra_vars":{"a":1}}`,
			WantMention: "extra_vars",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec, tools := recordingTools(t, Options{AllowAdhoc: true})
			tool := findTool(t, tools, test.Tool)
			out, err := tool.Run(context.Background(), json.RawMessage(test.Args))
			if err == nil {
				t.Fatalf("%s(%s) = %q, want a refusal", test.Tool, test.Args, out)
			}
			if !strings.Contains(err.Error(), test.WantMention) {
				t.Errorf("%s refusal = %v, want it to mention %q", test.Tool, err, test.WantMention)
			}
			if got := rec.records(); len(got) != 0 {
				t.Errorf("%s reached the product %d time(s) on arguments it refused: %+v",
					test.Tool, len(got), got)
			}
		})
	}
}

// TestEveryToolCallIsOneAuthenticatedRequestOnItsOwnEndpoint pins the request each tool makes: the
// method, the path, and that the operator's bearer token rides on it.
//
// This package holds no authority of its own. A tool that reached the product by another route, or
// on another endpoint than the one its description names, would be doing something the operator did
// not authorize under a name that says otherwise.
//
//nolint:funlen // Test function.
func TestEveryToolCallIsOneAuthenticatedRequestOnItsOwnEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Tool is the tool to call.
		Tool string
		// Args is the arguments a model supplied.
		Args string
		// WantMethod is the HTTP method the single request must use.
		WantMethod string
		// WantPath is the request path.
		WantPath string
		// WantQuery is the raw query string.
		WantQuery string
	}{
		{ // Test 0: Listing templates reads the collection.
			Tool: "list_templates", Args: ``, WantMethod: http.MethodGet, WantPath: "/v1/templates",
		},
		{ // Test 1: Reading one run reads that run.
			Tool: "get_run", Args: `{"run_id":"run_1"}`, WantMethod: http.MethodGet,
			WantPath: "/v1/runs/run_1",
		},
		{ // Test 2: Reading a log reads the log endpoint, which serves text.
			Tool: "get_run_log", Args: `{"run_id":"run_1"}`, WantMethod: http.MethodGet,
			WantPath: "/v1/runs/run_1/logs",
		},
		{ // Test 3: Evidence is asked for as data, because the reader is a model rather than a person
			// and the same endpoint otherwise answers with a page of markup.
			Tool: "get_run_evidence", Args: `{"run_id":"run_1"}`, WantMethod: http.MethodGet,
			WantPath: "/v1/runs/run_1/evidence", WantQuery: "format=json",
		},
		{ // Test 4: Listing runs reads the collection with the search the agent gave.
			Tool: "list_runs", Args: `{"query":"status:failed","limit":3}`, WantMethod: http.MethodGet,
			WantPath: "/v1/runs", WantQuery: "limit=3&q=status%3Afailed",
		},
		{ // Test 5: An ad-hoc proposal is a submission at the runs collection.
			Tool: "propose_adhoc_run", Args: `{"command":"echo hi"}`, WantMethod: http.MethodPost,
			WantPath: "/v1/runs",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec, tools := recordingTools(t, Options{AllowAdhoc: true})
			tool := findTool(t, tools, test.Tool)
			if _, err := tool.Run(context.Background(), json.RawMessage(test.Args)); err != nil {
				t.Fatalf("%s error = %v", test.Tool, err)
			}
			got := rec.records()
			if len(got) != 1 {
				t.Fatalf("%s made %d requests, want exactly one: %+v", test.Tool, len(got), got)
			}
			if got[0].Method != test.WantMethod || got[0].Path != test.WantPath {
				t.Errorf("%s called %s %s, want %s %s",
					test.Tool, got[0].Method, got[0].Path, test.WantMethod, test.WantPath)
			}
			if got[0].Query != test.WantQuery {
				t.Errorf("%s query = %q, want %q", test.Tool, got[0].Query, test.WantQuery)
			}
			if got[0].Auth != "Bearer st_test_token" {
				t.Errorf("%s carried authorization %q, want the operator's bearer token",
					test.Tool, got[0].Auth)
			}
		})
	}
}

// TestProposeRunReadsTheTemplateBeforeLaunchingWithALimit pins the order of the two requests a
// narrowing launch makes. The template has to be read first, because the answer decides whether the
// launch happens at all, and a launch issued before the check is a launch the check cannot stop.
func TestProposeRunReadsTheTemplateBeforeLaunchingWithALimit(t *testing.T) {
	t.Parallel()
	rec, tools := recordingTools(t, Options{})
	tool := findTool(t, tools, "propose_run")
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"template_id":"tpl_1","limit":"web01"}`)); err != nil {
		t.Fatalf("propose_run error = %v", err)
	}
	got := rec.records()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want the template read then the launch: %+v", len(got), got)
	}
	if got[0].Method != http.MethodGet || got[0].Path != "/v1/templates/tpl_1" {
		t.Errorf("first request = %s %s, want the template read", got[0].Method, got[0].Path)
	}
	if got[1].Method != http.MethodPost || got[1].Path != "/v1/templates/tpl_1/launch" {
		t.Errorf("second request = %s %s, want the launch", got[1].Method, got[1].Path)
	}
	if got[1].Body["limit"] != "web01" {
		t.Errorf("launch body limit = %v, want the narrowing pattern", got[1].Body["limit"])
	}
}

// TestProposeRunSendsOnlyWhatTheAgentSupplied pins the launch body. A key this tool invents is a
// control the operator's template did not ask for, and a key it drops is a control the model did
// ask for, so the body has to be exactly the agent's inputs plus the agent marker.
//
//nolint:funlen // Test function.
func TestProposeRunSendsOnlyWhatTheAgentSupplied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Args is the arguments a model supplied.
		Args string
		// WantBody is the exact launch body.
		WantBody map[string]any
	}{
		{ // Test 0: The minimum launch carries only the agent marker.
			Name: "bare launch", Args: `{"template_id":"tpl_1"}`,
			WantBody: map[string]any{"labels": map[string]any{"proposed_by": "mcp"}},
		},
		{ // Test 1: A stated reason rides as a label, so the register shows why an agent asked.
			Name: "reason", Args: `{"template_id":"tpl_1","reason":"disk filling on web01"}`,
			WantBody: map[string]any{"labels": map[string]any{
				"proposed_by": "mcp", "reason": "disk filling on web01"}},
		},
		{ // Test 2: A whitespace reason is no reason and does not write an empty label.
			Name: "blank reason", Args: `{"template_id":"tpl_1","reason":"   "}`,
			WantBody: map[string]any{"labels": map[string]any{"proposed_by": "mcp"}},
		},
		{ // Test 3: Survey answers are the supported channel and travel as given.
			Name: "answers", Args: `{"template_id":"tpl_1","answers":{"env":"stage","n":2}}`,
			WantBody: map[string]any{
				"labels":  map[string]any{"proposed_by": "mcp"},
				"answers": map[string]any{"env": "stage", "n": float64(2)},
			},
		},
		{ // Test 4: An empty answers object is nothing to send.
			Name: "empty answers", Args: `{"template_id":"tpl_1","answers":{}}`,
			WantBody: map[string]any{"labels": map[string]any{"proposed_by": "mcp"}},
		},
		{ // Test 5: An explicit dry run reaches the wire.
			Name: "dry run true", Args: `{"template_id":"tpl_1","dry_run":true}`,
			WantBody: map[string]any{
				"labels": map[string]any{"proposed_by": "mcp"}, "dry_run": true,
			},
		},
		{ // Test 6: So does an explicit refusal of one. The argument is a pointer precisely so that
			// asking for a real run is distinguishable from not asking, which for a preview flag is
			// the difference between a change and a rehearsal.
			Name: "dry run false", Args: `{"template_id":"tpl_1","dry_run":false}`,
			WantBody: map[string]any{
				"labels": map[string]any{"proposed_by": "mcp"}, "dry_run": false,
			},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec, tools := recordingTools(t, Options{})
			tool := findTool(t, tools, "propose_run")
			if _, err := tool.Run(context.Background(), json.RawMessage(test.Args)); err != nil {
				t.Fatalf("propose_run(%s) error = %v", test.Name, err)
			}
			got := rec.records()
			if len(got) != 1 {
				t.Fatalf("requests = %d, want only the launch: %+v", len(got), got)
			}
			if diff := cmp.Diff(test.WantBody, got[0].Body, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("launch body (-want +got):\n%s", diff)
			}
		})
	}
}

// TestProposeRunBoundsTheReasonLabel pins the bound on the one field a model writes freely. The
// reason rides into the change register as a label, so leaving it unbounded lets a model write
// unbounded data into the record it is being governed by.
func TestProposeRunBoundsTheReasonLabel(t *testing.T) {
	t.Parallel()
	rec, tools := recordingTools(t, Options{})
	tool := findTool(t, tools, "propose_run")
	args, err := json.Marshal(map[string]any{
		"template_id": "tpl_1", "reason": strings.Repeat("why ", 5000),
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := tool.Run(context.Background(), args); err != nil {
		t.Fatalf("propose_run error = %v", err)
	}
	labels, _ := rec.records()[0].Body["labels"].(map[string]any)
	reason, _ := labels["reason"].(string)
	if len(reason) != 200 {
		t.Errorf("the recorded reason is %d bytes, want it bounded at 200", len(reason))
	}
	if labels["proposed_by"] != "mcp" {
		t.Errorf("labels = %v, want the agent marker beside the bounded reason", labels)
	}
}

// TestAdhocProposalSendsOnlyTheFieldsTheAgentFilled pins the ad-hoc submission body. This tool is
// off by default because it widens the agent from a vetted menu to anything its token may submit, so
// what it sends has to be exactly what the model asked for and nothing invented alongside it.
func TestAdhocProposalSendsOnlyTheFieldsTheAgentFilled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Args is the arguments a model supplied.
		Args string
		// WantBody is the exact submission body.
		WantBody map[string]any
	}{
		{ // Test 0: A bare script submission carries the script and the agent marker.
			Name: "command only", Args: `{"command":"echo hi"}`,
			WantBody: map[string]any{
				"command": "echo hi", "labels": map[string]any{"proposed_by": "mcp"},
			},
		},
		{ // Test 1: An empty field is left out rather than sent as an empty string, which the API
			// would read as a caller choosing that value.
			Name: "empty fields dropped",
			Args: `{"tool":"ansible","playbook":"site.yml","command":"","inventory_id":"",` +
				`"project_id":"prj_1","limit":""}`,
			WantBody: map[string]any{
				"tool": "ansible", "playbook": "site.yml", "project_id": "prj_1",
				"labels": map[string]any{"proposed_by": "mcp"},
			},
		},
		{ // Test 2: A dry run is sent when asked for.
			Name: "dry run", Args: `{"command":"echo hi","dry_run":true}`,
			WantBody: map[string]any{
				"command": "echo hi", "dry_run": true,
				"labels": map[string]any{"proposed_by": "mcp"},
			},
		},
		{ // Test 3: And omitted when not, leaving the API's own default in force.
			Name: "no dry run", Args: `{"command":"echo hi","dry_run":false}`,
			WantBody: map[string]any{
				"command": "echo hi", "labels": map[string]any{"proposed_by": "mcp"},
			},
		},
		{ // Test 4: A stated reason rides as a bounded label here too.
			Name: "reason", Args: `{"command":"echo hi","reason":"triage"}`,
			WantBody: map[string]any{
				"command": "echo hi",
				"labels":  map[string]any{"proposed_by": "mcp", "reason": "triage"},
			},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec, tools := recordingTools(t, Options{AllowAdhoc: true})
			tool := findTool(t, tools, "propose_adhoc_run")
			if _, err := tool.Run(context.Background(), json.RawMessage(test.Args)); err != nil {
				t.Fatalf("propose_adhoc_run(%s) error = %v", test.Name, err)
			}
			got := rec.records()
			if len(got) != 1 {
				t.Fatalf("requests = %d, want only the submission: %+v", len(got), got)
			}
			if diff := cmp.Diff(test.WantBody, got[0].Body, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("submission body (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAdhocProposalIsNotSubjectToTheTemplateLimitGuard pins a real difference between the two
// proposal tools, so the gap is a decision on the record rather than a surprise.
//
// The limit guard exists because a template is an operator's statement of which hosts it may touch
// and the launch endpoint lets a caller replace it. An ad-hoc run has no template to widen, so the
// guard has nothing to compare against and a whole-inventory pattern reaches the API, where the risk
// grade and the approval policy are what hold it.
func TestAdhocProposalIsNotSubjectToTheTemplateLimitGuard(t *testing.T) {
	t.Parallel()
	rec, tools := recordingTools(t, Options{AllowAdhoc: true})
	tool := findTool(t, tools, "propose_adhoc_run")
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"command":"echo hi","limit":"all"}`)); err != nil {
		t.Fatalf("propose_adhoc_run error = %v", err)
	}
	got := rec.records()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want the submission: %+v", len(got), got)
	}
	if got[0].Body["limit"] != "all" {
		t.Errorf("submission limit = %v, want the pattern passed through for the API to grade",
			got[0].Body["limit"])
	}
}

// TestToolSchemasDeclareWhatTheModelMustSupply pins each tool's advertised contract. The schema is
// the only thing a model reads before it calls, so a required argument missing from it is a call the
// model makes wrong and a refusal it did not need to earn.
func TestToolSchemasDeclareWhatTheModelMustSupply(t *testing.T) {
	t.Parallel()
	_, tools := recordingTools(t, Options{AllowAdhoc: true})
	wantRequired := map[string][]string{
		"list_templates":    nil,
		"propose_run":       {"template_id"},
		"get_run":           {"run_id"},
		"get_run_log":       {"run_id"},
		"get_run_evidence":  {"run_id"},
		"list_runs":         nil,
		"propose_adhoc_run": nil,
	}
	if len(tools) != len(wantRequired) {
		t.Fatalf("tool set holds %d tools, want %d", len(tools), len(wantRequired))
	}
	for testNum, tool := range tools {
		t.Run(fmt.Sprintf("test %d %s", testNum, tool.Name), func(t *testing.T) {
			t.Parallel()
			want, known := wantRequired[tool.Name]
			if !known {
				t.Fatalf("tool %q is exposed but not accounted for here", tool.Name)
			}
			if tool.Description == "" || tool.Run == nil {
				t.Errorf("tool %q is missing a description or a body", tool.Name)
			}
			if tool.InputSchema["type"] != "object" {
				t.Errorf("tool %q declares schema type %v, want object", tool.Name, tool.InputSchema["type"])
			}
			if _, ok := tool.InputSchema["properties"].(map[string]any); !ok {
				t.Errorf("tool %q declares no properties object", tool.Name)
			}
			var got []string
			if req, ok := tool.InputSchema["required"].([]string); ok {
				got = req
			}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("tool %q required arguments (-want +got):\n%s", tool.Name, diff)
			}
			// Nothing in a schema may offer the model a way to release its own work or widen its
			// reach, since a declared argument is an invitation a model will take.
			props, _ := tool.InputSchema["properties"].(map[string]any)
			for name := range props {
				for _, banned := range []string{"approve", "approval", "token", "credential", "secret",
					"password", "grant", "policy", "admin", "actor", "extra_vars"} {
					if strings.Contains(name, banned) {
						t.Errorf("tool %q declares argument %q, which offers the model %q",
							tool.Name, name, banned)
					}
				}
			}
		})
	}
}

// TestProposalToolsTellTheModelItCannotApproveItsOwnWork pins the descriptions. The tool text is
// what a model reasons from, and the one thing it must not conclude is that proposing and releasing
// are the same act: an agent that believes it can approve will keep trying and will report a held
// run as a completed one.
func TestProposalToolsTellTheModelItCannotApproveItsOwnWork(t *testing.T) {
	t.Parallel()
	_, tools := recordingTools(t, Options{AllowAdhoc: true})
	for testNum, name := range []string{"propose_run", "propose_adhoc_run"} {
		t.Run(fmt.Sprintf("test %d %s", testNum, name), func(t *testing.T) {
			t.Parallel()
			tool := findTool(t, tools, name)
			for _, want := range []string{"cannot approve it", "audit"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("the %s description does not carry %q:\n%s", name, want, tool.Description)
				}
			}
		})
	}
}

// TestAnEmptyLogIsExplainedRatherThanReturnedBlank pins what a run with no output yet reads as. A
// blank string is indistinguishable from a tool that failed quietly, and a model handed one has no
// way to tell "nothing has happened yet" from "something went wrong reading it".
func TestAnEmptyLogIsExplainedRatherThanReturnedBlank(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Body is what the log endpoint serves.
		Body string
		// WantResult is the text handed to the model.
		WantResult string
	}{
		{Name: "no output yet", Body: "", WantResult: "(no output recorded yet)"},    // Test 0.
		{Name: "one newline", Body: "\n", WantResult: "\n"},                          // Test 1: Real output.
		{Name: "real log", Body: "PLAY [all] ***\n", WantResult: "PLAY [all] ***\n"}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = io.WriteString(w, test.Body)
			}))
			defer ts.Close()
			tool := getRunLogTool(t, testClient(t, ts))
			got, err := tool.Run(context.Background(), json.RawMessage(`{"run_id":"run_1"}`))
			if err != nil {
				t.Fatalf("get_run_log(%s) error = %v", test.Name, err)
			}
			if got != test.WantResult {
				t.Errorf("get_run_log(%s) = %q, want %q", test.Name, got, test.WantResult)
			}
		})
	}
}

// TestAHostileIdentifierStaysInsideItsOwnEndpoint pins the escaping across every tool that takes an
// identifier from the model, including the two requests a template launch makes. An id is untrusted
// text however plausible it looks, and one that walked to another endpoint would turn a read tool
// into a request the operator never authorized, carrying the operator's own token.
func TestAHostileIdentifierStaysInsideItsOwnEndpoint(t *testing.T) {
	t.Parallel()
	hostile := []string{
		"../../v1/users", "/v1/users", "run_1?admin=1", "run_1#x", "run_1 OR 1=1", "run_1\nX",
	}
	targets := []struct {
		// Tool is the tool to call.
		Tool string
		// Key is the identifier argument's name.
		Key string
		// Prefix is the endpoint the request must stay under.
		Prefix string
	}{
		{Tool: "get_run", Key: "run_id", Prefix: "/v1/runs/"},
		{Tool: "get_run_log", Key: "run_id", Prefix: "/v1/runs/"},
		{Tool: "get_run_evidence", Key: "run_id", Prefix: "/v1/runs/"},
		{Tool: "propose_run", Key: "template_id", Prefix: "/v1/templates/"},
	}
	for testNum, target := range targets {
		t.Run(fmt.Sprintf("test %d %s", testNum, target.Tool), func(t *testing.T) {
			t.Parallel()
			for _, id := range hostile {
				rec, tools := recordingTools(t, Options{})
				tool := findTool(t, tools, target.Tool)
				args, err := json.Marshal(map[string]string{target.Key: id})
				if err != nil {
					t.Fatalf("Marshal() error = %v", err)
				}
				if _, err := tool.Run(context.Background(), args); err != nil {
					t.Fatalf("%s(%q) error = %v", target.Tool, id, err)
				}
				for _, got := range rec.records() {
					if !strings.HasPrefix(got.Path, target.Prefix) {
						t.Errorf("%s(%q) reached %q, outside %q", target.Tool, id, got.Path, target.Prefix)
					}
					if strings.Contains(got.Path, "/v1/users") {
						t.Errorf("%s(%q) reached the admin-only account list at %q",
							target.Tool, id, got.Path)
					}
					if got.Query != "" && target.Tool != "get_run_evidence" {
						t.Errorf("%s(%q) smuggled a query string %q", target.Tool, id, got.Query)
					}
				}
			}
		})
	}
}
