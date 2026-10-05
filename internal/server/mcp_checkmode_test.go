package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/mcp"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAnAgentDryRunThatForcesRealTasksIsHeld drives the bypass the way an AI agent would reach it:
// through the MCP tool, over the API, into the real gate.
//
// An agent asked to look before it leaps proposes a dry run, and a rule that excludes dry runs is
// the natural one for an operator to write for an agent: let it preview freely, hold everything
// else. Ansible runs a task that sets check_mode to false for real under --check, so an agent
// proposing a dry run of such a playbook changed hosts with no person involved. The run must be
// held, and what the agent and the approver read back must say why.
//
//nolint:funlen // Test function.
func TestAnAgentDryRunThatForcesRealTasksIsHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Playbook   string
		WantStatus run.Status
		WantForced string
	}{{ // Test 0: A clean dry run is a preview the agent may run unattended.
		Playbook:   "- hosts: all\n  tasks:\n    - name: Look\n      ansible.builtin.ping:\n",
		WantStatus: run.StatusSucceeded,
	}, { // Test 1: A dry run whose playbook forces a real restart is held for a person.
		Playbook: "- hosts: all\n  tasks:\n    - name: Restart web\n" +
			"      ansible.builtin.service: name=web state=restarted\n      check_mode: false\n",
		WantStatus: run.StatusPendingApproval,
		WantForced: `site.yml: task "Restart web" sets check_mode to false`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			playbook := filepath.Join(dir, "site.yml")
			if err := os.WriteFile(playbook, []byte(test.Playbook), 0o600); err != nil {
				t.Fatalf("write playbook: %v", err)
			}

			rules := policy.NewMemStore()
			if err := rules.Save(ctx, &policy.Policy{
				ID: "pol_agents", Name: "agents preview only", ActorKind: policy.ActorKindAgent,
				ExcludeDryRun: true, Effect: policy.EffectRequireApproval,
				MaxDestroy: policy.DisabledMaxDestroy,
			}); err != nil {
				t.Fatalf("Save(policy) error = %v", err)
			}
			store := run.NewMemStore()
			runner := roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{ExitCode: 0}, nil
				})
			d := dispatch.New(store, runner, zap.NewNop(), dispatch.WithPolicies(rules),
				dispatch.WithNoJanitor())
			t.Cleanup(d.Close)

			tokens, users := auth.NewMemStore(), user.NewMemStore()
			owner, err := user.New("owner", "a long enough password", user.RoleOperator)
			if err != nil {
				t.Fatalf("user.New() error = %v", err)
			}
			if err := users.Save(ctx, owner); err != nil {
				t.Fatalf("users.Save() error = %v", err)
			}
			plain, tok, err := auth.New("deploy-bot")
			if err != nil {
				t.Fatalf("auth.New() error = %v", err)
			}
			tok.UserID, tok.Kind = owner.ID, auth.KindAgent
			if err := tokens.Save(ctx, tok); err != nil {
				t.Fatalf("tokens.Save() error = %v", err)
			}
			handler := New(store, d, zap.NewNop(), WithTokens(tokens), WithUsers(users),
				WithAudit(audit.NewMemStore()), WithApprover(d)).Handler()
			ts := httptest.NewServer(handler)
			t.Cleanup(ts.Close)

			client, err := mcp.NewClient(ts.URL, plain, 5*time.Second)
			if err != nil {
				t.Fatalf("mcp.NewClient() error = %v", err)
			}
			var propose mcp.Tool
			for _, tool := range mcp.Tools(client, mcp.Options{AllowAdhoc: true}) {
				if tool.Name == "propose_adhoc_run" {
					propose = tool
				}
			}
			args, err := json.Marshal(map[string]any{
				"tool": "ansible", "playbook": playbook, "dry_run": true,
				"reason": "check the web tier before the change window",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			reply, err := propose.Run(ctx, args)
			if err != nil {
				t.Fatalf("propose_adhoc_run error = %v", err)
			}
			var proposed run.Run
			if err := json.Unmarshal([]byte(reply), &proposed); err != nil {
				t.Fatalf("decode the tool's reply %q: %v", reply, err)
			}
			got := proposed.Status
			if test.WantStatus != run.StatusPendingApproval {
				got = waitDone(t, store, proposed.ID).Status
			}
			if got != test.WantStatus {
				t.Fatalf("the agent's dry run reached %q, want %q. Recorded: %q", got,
					test.WantStatus, proposed.DryRunFindings())
			}
			if test.WantForced == "" {
				return
			}
			// What the agent reads back, and what the approver's view of the run says.
			if !strings.Contains(strings.Join(proposed.DryRunFindings(), "\n"), test.WantForced) {
				t.Errorf("the agent's reply does not say why the dry run is held: %s", reply)
			}
			if !strings.Contains(proposed.HoldNote, "Two clean fixes") {
				t.Errorf("the agent's reply does not carry the hold note: %s", reply)
			}
			view := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/v1/runs/"+proposed.ID, nil)
			req.Header.Set("Authorization", "Bearer "+plain)
			handler.ServeHTTP(view, req)
			var shown run.Run
			if err := json.Unmarshal(view.Body.Bytes(), &shown); err != nil {
				t.Fatalf("decode the run view %q: %v", view.Body.String(), err)
			}
			if !strings.Contains(strings.Join(shown.DryRunFindings(), "\n"), test.WantForced) {
				t.Errorf("the run view does not name the forcing task: %q", shown.DryRunFindings())
			}
			if len(shown.DryRunScans) != 1 || shown.DryRunScans[0].Classification !=
				run.DryRunNotChangeFree || len(shown.DryRunScans[0].Inputs) == 0 {
				t.Errorf("the run view does not carry the scan's evidence: %+v", shown.DryRunScans)
			}
			if shown.Risk == nil || !strings.Contains(strings.Join(shown.Risk.Reasons, "\n"),
				"Restart web") {
				t.Errorf("the approver's risk grade does not name the forcing task: %+v", shown.Risk)
			}
			if shown.Reversibility == nil || shown.Reversibility.Class == run.Reversible {
				t.Errorf("the approver is told the dry run has nothing to undo: %+v", shown.Reversibility)
			}
		})
	}
}

// waitDone polls store until the run is terminal.
func waitDone(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := store.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
