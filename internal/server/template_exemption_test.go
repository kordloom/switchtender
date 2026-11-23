package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// smokeExemption exempts release-agent on dev-lead for bash commands containing ./smoke.sh.
const smokeExemption = `{"name":"smoke","effect":"exempt","actor":"release-agent",` +
	`"account":"dev-lead","tool":"bash","command_contains":"./smoke.sh"}`

// smokeNote is the note a run carries when the smoke exemption let it proceed.
var smokeNote = exemptNote("smoke")

// writeSmokeExemption has the admin write the smoke exemption.
func writeSmokeExemption(t *testing.T, s *agentHoldServer) {
	t.Helper()
	if code, got := s.call(s.Admin, http.MethodPost, "/v1/policies",
		smokeExemption); code != http.StatusCreated {
		t.Fatalf("admin writing the exemption = %d, want 201: %s", code, got)
	}
}

// saveTemplate has the admin save a template from body and returns its id.
func saveTemplate(t *testing.T, s *agentHoldServer, body string) string {
	t.Helper()
	code, got := s.call(s.Admin, http.MethodPost, "/v1/templates", body)
	if code != http.StatusCreated {
		t.Fatalf("admin saving the template = %d, want 201: %s", code, got)
	}
	var saved template.Template
	if err := json.Unmarshal([]byte(got), &saved); err != nil {
		t.Fatalf("decode template %q: %v", got, err)
	}
	return saved.ID
}

// TestAnExemptionDoesNotCoverACommandTheAgentComposes proves an exemption naming a command does not
// let an agent run a different command that merely contains it. The agent composes "./smoke.sh;
// env" itself and no template backs it, so it is held. Naming a credential on that request is
// refused with 400 here, because this harness has no credential store or encryption key, so the
// refusal is recorded and the held case is asserted without a credential.
func TestAnExemptionDoesNotCoverACommandTheAgentComposes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the agent's request.
		Body string
		// WantCode is the answer, 202 when the run is held.
		WantCode int
	}{{ // Test 0: Naming a credential is refused, since the harness has no credential store.
		Body:     `{"tool":"bash","command":"./smoke.sh; env","credential_ids":["cred_unknown"]}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 1: The composed command with no credential is held.
		Body:     `{"tool":"bash","command":"./smoke.sh; env"}`,
		WantCode: http.StatusAccepted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
			writeSmokeExemption(t, s)
			code, body := s.call(s.Agent, http.MethodPost, "/v1/runs", test.Body)
			if code != test.WantCode {
				t.Fatalf("POST /v1/runs = %d, want %d: %s", code, test.WantCode, body)
			}
			if code != http.StatusAccepted {
				return
			}
			got := agentHoldDecode(t, body)
			if got.Status != run.StatusPendingApproval {
				t.Errorf("status = %s, want pending_approval (notes %q)", got.Status,
					got.PolicyNotes)
			}
		})
	}
}

// TestAnExemptionLetsAnAgentLaunchTheSavedTemplate proves the same exemption lets the agent launch
// an admin's saved template whose command it matches, and the run's notes name the exemption.
func TestAnExemptionLetsAnAgentLaunchTheSavedTemplate(t *testing.T) {
	t.Parallel()
	s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
	writeSmokeExemption(t, s)
	got := agentHoldLaunch(t, s, `{"name":"smoke","tool":"bash","command":"./smoke.sh"}`, `{}`)
	if got.Status == run.StatusPendingApproval {
		t.Errorf("the launch is held by %q, want it to proceed", got.HeldByPolicy)
	}
	if !strings.Contains(strings.Join(got.PolicyNotes, "\n"), smokeNote) {
		t.Errorf("notes = %q, want %q", got.PolicyNotes, smokeNote)
	}
}

// TestAnAgentCannotTurnADryRunTemplateIntoARealRun proves an agent's launch of a dry-run template
// with dry_run false is refused with 403 naming dry_run, and its launch as a dry run, or with no
// body, is accepted.
func TestAnAgentCannotTurnADryRunTemplateIntoARealRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the launch body.
		Body string
		// WantCode is the answer.
		WantCode int
	}{{ // Test 0: dry_run false is refused.
		Body: `{"dry_run":false}`, WantCode: http.StatusForbidden,
	}, { // Test 1: dry_run true is accepted.
		Body: `{"dry_run":true}`, WantCode: http.StatusAccepted,
	}, { // Test 2: No body is accepted.
		Body: ``, WantCode: http.StatusAccepted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
			id := saveTemplate(t, s,
				`{"name":"check","tool":"bash","command":"./check.sh","dry_run":true}`)
			code, got := s.call(s.Agent, http.MethodPost, "/v1/templates/"+id+"/launch", test.Body)
			if code != test.WantCode {
				t.Fatalf("launch = %d, want %d: %s", code, test.WantCode, got)
			}
			if code == http.StatusForbidden && !strings.Contains(got, "dry_run") {
				t.Errorf("the refusal %q does not mention dry_run", got)
			}
		})
	}
}

// TestAnExemptionCoversASavedWorkflowButNotOneSubmittedWhole proves an exemption naming the agent
// lets it launch a saved workflow, and the same steps the agent submits whole are held.
func TestAnExemptionCoversASavedWorkflowButNotOneSubmittedWhole(t *testing.T) {
	t.Parallel()
	const steps = `[{"name":"build","tool":"bash","command":"./build.sh"},` +
		`{"name":"ship","tool":"bash","command":"./ship.sh"}]`
	const exempt = `{"name":"flows","effect":"exempt","actor":"release-agent",` +
		`"account":"dev-lead"}`
	tests := []struct {
		// Whole submits the steps through POST /v1/pipelines rather than launching a template.
		Whole bool
		// WantHeld is whether the run waits for a person.
		WantHeld bool
	}{{ // Test 0: The saved workflow goes ahead.
		Whole: false, WantHeld: false,
	}, { // Test 1: The same steps submitted whole are held, the control.
		Whole: true, WantHeld: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
			if code, got := s.call(s.Admin, http.MethodPost, "/v1/policies",
				exempt); code != http.StatusCreated {
				t.Fatalf("admin writing the exemption = %d: %s", code, got)
			}
			var got run.Run
			if test.Whole {
				got = s.submit(t, s.Agent, http.MethodPost, "/v1/pipelines",
					`{"name":"release","steps":`+steps+`}`)
			} else {
				got = agentHoldLaunch(t, s, `{"name":"release","steps":`+steps+`}`, `{}`)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Errorf("held = %t, want %t (held by %q)", held, test.WantHeld, got.HeldByPolicy)
			}
			if test.WantHeld && got.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("held by %q, want %q", got.HeldByPolicy, policy.AgentDefaultName)
			}
		})
	}
}

// TestAnAgentsRerunOfAPersonsTemplateLaunch pins what happens when the agent reruns a finished run
// a person launched from a template the smoke exemption matches. Today the rerun is accepted with
// 202, is not held, carries the template id, names the smoke exemption in its notes, and records
// release-agent on dev-lead as its actor, with agent as the actor type.
func TestAnAgentsRerunOfAPersonsTemplateLaunch(t *testing.T) {
	t.Parallel()
	s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
	writeSmokeExemption(t, s)
	id := saveTemplate(t, s, `{"name":"smoke","tool":"bash","command":"./smoke.sh"}`)
	first := s.submit(t, s.Person, http.MethodPost, "/v1/templates/"+id+"/launch", `{}`)
	waitTerminal(t, s.DB.runs, first.ID)
	got := s.submit(t, s.Agent, http.MethodPost, "/v1/runs/"+first.ID+"/rerun", `{}`)
	if got.Status == run.StatusPendingApproval {
		t.Errorf("the rerun is held by %q, want it exempt", got.HeldByPolicy)
	}
	if !strings.Contains(strings.Join(got.PolicyNotes, "\n"), smokeNote) {
		t.Errorf("notes = %q, want %q", got.PolicyNotes, smokeNote)
	}
	if got.TemplateID != id || got.Actor != "release-agent" || got.ActorType != "agent" ||
		got.Account != "dev-lead" {
		t.Errorf("rerun template %q actor %s (%s) account %q, want %s release-agent (agent) "+
			"dev-lead", got.TemplateID, got.Actor, got.ActorType, got.Account, id)
	}
}
