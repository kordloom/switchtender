package migration

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// mcpSession is an agent's MCP connection: the switchtender mcp command, spoken to over stdio.
type mcpSession struct {
	// t is the scenario the session belongs to.
	t *testing.T
	// in writes requests.
	in io.WriteCloser
	// out reads responses.
	out *bufio.Reader
	// cmd is the running command.
	cmd *exec.Cmd
	// next is the next request id.
	next int
}

// startMCP starts the MCP command against s with the agent's token, as an agent's host would.
func (in *install) startMCP(s *server) *mcpSession {
	in.t.Helper()
	self, err := os.Executable()
	if err != nil {
		in.t.Fatalf("locate the test binary: %v", err)
	}
	c := exec.Command(self, "mcp", "--server", s.url)
	c.Env = append(append([]string(nil), in.env...), "SWITCHTENDER_MCP_TOKEN="+in.tokens["agent"])
	c.Dir = in.root
	stdin, err := c.StdinPipe()
	if err != nil {
		in.t.Fatalf("open the MCP input: %v", err)
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		in.t.Fatalf("open the MCP output: %v", err)
	}
	c.Stderr = io.Discard
	if err := c.Start(); err != nil {
		in.t.Fatalf("start the MCP command: %v", err)
	}
	m := &mcpSession{t: in.t, in: stdin, out: bufio.NewReader(stdout), cmd: c, next: 1}
	in.t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = c.Process.Kill()
			<-done
		}
	})
	m.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "scenario-agent", "version": "1"},
	})
	return m
}

// call sends one request and returns its result.
func (m *mcpSession) call(method string, params any) json.RawMessage {
	m.t.Helper()
	id := m.next
	m.next++
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method,
		"params": params})
	if err != nil {
		m.t.Fatalf("encode the MCP request: %v", err)
	}
	if _, err := m.in.Write(append(req, '\n')); err != nil {
		m.t.Fatalf("send the MCP request: %v", err)
	}
	for {
		line, err := m.out.ReadBytes('\n')
		if err != nil {
			m.t.Fatalf("read the MCP response to %s: %v", method, err)
		}
		var res struct {
			// ID matches the request.
			ID int `json:"id"`
			// Result is the answer.
			Result json.RawMessage `json:"result"`
			// Error is a protocol error.
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &res) != nil || res.ID != id {
			continue
		}
		if len(res.Error) > 0 {
			m.t.Fatalf("MCP %s failed: %s", method, res.Error)
		}
		return res.Result
	}
}

// tool calls an MCP tool and returns the text it answered with, and whether it reported an error.
func (m *mcpSession) tool(name string, args map[string]any) (string, bool) {
	m.t.Helper()
	var res struct {
		// Content is the tool's answer.
		Content []struct {
			// Text is one text block.
			Text string `json:"text"`
		} `json:"content"`
		// IsError reports a tool failure.
		IsError bool `json:"isError"`
	}
	raw := m.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err := json.Unmarshal(raw, &res); err != nil {
		m.t.Fatalf("decode the %s answer: %v: %s", name, err, raw)
	}
	var text strings.Builder
	for _, c := range res.Content {
		text.WriteString(c.Text)
	}
	return text.String(), res.IsError
}

// TestAgentStartsTheImportedWorkflowAndOnlyAPersonReleasesIt is scenario five. An agent connected
// over MCP with its own token starts the imported release workflow. It can see the workflow
// waiting at the approval step and cannot decide it, through any route; a person can. The chain
// and the workflow's receipt name both: the agent, on behalf of the account it acts for, as the one
// that launched, and the person as the one that released.
func TestAgentStartsTheImportedWorkflowAndOnlyAPersonReleasesIt(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a")
	agent := in.startMCP(s)

	text, failed := agent.tool("propose_run", map[string]any{
		"template_id": in.template(s, "release"), "reason": "ship the release the build produced",
	})
	if failed {
		t.Fatalf("the agent's proposal was refused: %s", text)
	}
	wf := decodeRun(t, []byte(text))
	step := in.waitPending(s, "agent", wf.ID)
	if waiting, _ := agent.tool("list_pending_approvals", nil); !strings.Contains(waiting, step.ID) {
		t.Errorf("the agent's list_pending_approvals does not show the step it is waiting on: %s",
			waiting)
	}
	if step.RequestedByType != "agent" {
		t.Errorf("the waiting step names its requester as %s (%s), want the agent", step.RequestedBy,
			step.RequestedByType)
	}

	for _, verb := range []string{"approve", "reject"} {
		r := in.api(s, "agent", "POST", "/v1/runs/"+step.ID+"/"+verb,
			map[string]any{"state_digest": step.StateDigest})
		if r.Status != 403 {
			t.Errorf("the agent's %s of its own workflow's step = %d, want 403: %s", verb, r.Status,
				r.Body)
		}
		r = in.api(s, "agent", "POST", "/v1/runs/"+wf.ID+"/"+verb, nil)
		if r.Status != 403 {
			t.Errorf("the agent's %s of its workflow = %d, want 403: %s", verb, r.Status, r.Body)
		}
	}
	if again := in.waitPending(s, "agent", wf.ID); again.ID != step.ID {
		t.Fatalf("the step changed after the agent's refused decisions: %+v", again)
	}
	in.requireSteps(s, wf.ID, map[string]int{"build": 1, "ship": 0, "page": 0})

	in.must(s, "approver", "POST", "/v1/runs/"+step.ID+"/approve",
		map[string]any{"state_digest": step.StateDigest}, 200)
	if done := in.waitDone(s, wf.ID); done.Status != "succeeded" {
		t.Fatalf("the released workflow = %s, want succeeded: %s", done.Status, describe(done.Raw))
	}
	in.requireSteps(s, wf.ID, map[string]int{"build": 1, "ship": 1, "page": 0})

	ev := in.checkEvidence(s, wf.ID)
	rec := ev.Receipts[wf.ID]
	requireRecord(t, rec, recordWant{
		Launcher: "release-agent", LauncherType: "agent", OnBehalfOf: "operator",
	})
	requireChildren(t, rec, map[string]string{
		"build": "succeeded", "approve": "succeeded", "ship": "succeeded",
	})
	requireReceiptDecision(t, rec, step.ID, "approved", "approver-laptop", "approver")
	requireStepDecision(t, ev, step.ID, "approved", "approver-laptop")
	requireInitiator(t, rec, "release-agent", "operator")
	attempts := 0
	for _, e := range ev.entries("/v1/runs/") {
		if e.Actor == "release-agent" && (strings.HasSuffix(e.Path, "/approve") ||
			strings.HasSuffix(e.Path, "/reject")) {
			attempts++
			if e.ActorType != "agent" {
				t.Errorf("the agent's refused decision is recorded as actor type %q", e.ActorType)
			}
		}
	}
	if attempts != 4 {
		t.Errorf("the chain records %d of the agent's four refused decisions, want all four", attempts)
	}
}

// requireInitiator fails the scenario unless the receipt records the identity evidence of the agent
// that started the run: the agent, the account it is bound to, and that its token was minted from
// the command line, and unless the approval decision records how separation of duties was evaluated
// with the bound account counting as the requester.
func requireInitiator(t *testing.T, rec *receipt, agent, bound string) {
	t.Helper()
	got, _ := rec.outcome(t).Outcome["initiator"].(map[string]any)
	fromCLI := str(got["provisioned_by_type"]) == "cli" &&
		strings.HasPrefix(str(got["provisioned_by"]), "cli:")
	if str(got["initiated_by"]) != agent || str(got["bound_to"]) != bound || !fromCLI {
		t.Errorf("the receipt records the run's initiator as %v, want %s bound to %s and minted "+
			"from the command line", got, agent, bound)
	}
	var doc struct {
		// Claims are the disclosed entries.
		Claims []struct {
			// Payload is the entry.
			Payload struct {
				// Path is the entry's path.
				Path string `json:"path"`
				// DecisionBody is a decision entry's disclosed body.
				DecisionBody struct {
					// SoD is how separation of duties was evaluated.
					SoD map[string]any `json:"separation_of_duties"`
				} `json:"decision_body"`
			} `json:"payload"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(rec.Raw, &doc); err != nil {
		t.Fatalf("decode the receipt of %s: %v", rec.RunID, err)
	}
	evaluated := 0
	for _, c := range doc.Claims {
		sod := c.Payload.DecisionBody.SoD
		if !strings.Contains(c.Payload.Path, "/decision/approved") || sod == nil {
			continue
		}
		evaluated++
		if str(sod["requester"]) != bound || str(sod["result"]) != "not_required" {
			t.Errorf("the approval at %s records separation of duties as %v, want %s as the requester "+
				"and no independent approver required", c.Payload.Path, sod, bound)
		}
	}
	if evaluated == 0 {
		t.Errorf("no approval in the receipt of %s records separation of duties", rec.RunID)
	}
}
