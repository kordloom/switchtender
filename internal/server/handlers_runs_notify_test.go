package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// recordingEmailer records each send to a run's own recipients.
type recordingEmailer struct {
	// mu guards to.
	mu sync.Mutex
	// to holds the recipients of each SendTo call.
	to [][]string
}

// Send ignores mail to the server-wide recipients.
func (e *recordingEmailer) Send(context.Context, string, string) error { return nil }

// SendTo records one send to a run's own recipients.
func (e *recordingEmailer) SendTo(_ context.Context, to []string, _, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.to = append(e.to, append([]string(nil), to...))
	return nil
}

// sends returns a copy of every recorded send.
func (e *recordingEmailer) sends() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.to)
}

// agentTargetsMail is the recipient list the audit's request named.
const agentTargetsMail = "a@x.example,b@y.example,c@z.example"

// TestAgentRunTargetsNameNoRecipients is the audit's outbound case. An agent token posted a run with
// an email target of three addresses, the built-in rule held it, and the hold mailed all three
// through the server's transport before anyone looked: an agent chose who the install mails. Now
// the request is refused, a Twilio target the same, and an agent's rerun of a person's run that
// mails is refused too. The controls are an operator's inline target, a template an admin saved
// with the same target, which still mails on the agent's hold, and an agent's rerun of a template
// launch, whose targets an admin chose.
func TestAgentRunTargetsNameNoRecipients(t *testing.T) {
	t.Parallel()
	mailTarget := `"notifications":[{"kind":"email","to":"` + agentTargetsMail + `"}]`
	tests := []struct {
		Agent     bool
		Path      string
		Body      string
		WantCode  int
		WantSends int
	}{{ // Test 0: The audit's case, an agent's run naming three addresses.
		Agent: true, Path: "/v1/runs",
		Body:     `{"tool":"bash","command":"./rotate-certs.sh",` + mailTarget + `}`,
		WantCode: http.StatusForbidden,
	}, { // Test 1: A text recipient is refused the same way.
		Agent: true, Path: "/v1/runs",
		Body: `{"tool":"bash","command":"./rotate-certs.sh",` +
			`"notifications":[{"kind":"twilio","to":"+15550100"}]}`,
		WantCode: http.StatusForbidden,
	}, { // Test 2: An agent's rerun of a person's run that mails.
		Agent: true, Path: "/v1/runs/run_mails/rerun", Body: `{}`, WantCode: http.StatusForbidden,
	}, { // Test 3: Control, an operator's inline target still mails.
		Path:     "/v1/runs",
		Body:     `{"tool":"bash","command":"./rotate-certs.sh",` + mailTarget + `}`,
		WantCode: http.StatusAccepted, WantSends: 1,
	}, { // Test 4: Control, an admin-saved template's target mails on the agent's hold.
		Agent: true, Path: "/v1/templates/tpl_mail/launch", Body: `{}`,
		WantCode: http.StatusAccepted, WantSends: 1,
	}, { // Test 5: Control, an agent's rerun of a template launch replays the admin's targets.
		Agent: true, Path: "/v1/runs/run_tpl_mails/rerun", Body: `{}`,
		WantCode: http.StatusAccepted, WantSends: 1,
	}, { // Test 6: Control, the person may rerun their own run that mails.
		Path: "/v1/runs/run_mails/rerun", Body: `{}`, WantCode: http.StatusAccepted, WantSends: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			db := agentHoldBackends()[0].Open(t)
			emailer := &recordingEmailer{}
			s := newAgentHoldServer(t, db, true, dispatch.WithEmail(emailer, false))
			seedMailingRuns(t, db)
			before, err := db.runs.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			token := s.Person
			if test.Agent {
				token = s.Agent
			}
			code, body := s.call(token, http.MethodPost, test.Path, test.Body)
			if code != test.WantCode {
				t.Fatalf("POST %s = %d, want %d: %s", test.Path, code, test.WantCode, body)
			}
			if test.WantCode == http.StatusForbidden {
				if !strings.Contains(body, "an agent cannot name mail or text recipients") {
					t.Errorf("refusal %s does not name the rule", body)
				}
				after, err := db.runs.List(context.Background())
				if err != nil {
					t.Fatalf("List() error = %v", err)
				}
				if len(after) != len(before) {
					t.Errorf("a refused request stored a run: %d runs before, %d after",
						len(before), len(after))
				}
			}
			want := test.WantSends
			deadline := time.Now().Add(3 * time.Second)
			for len(emailer.sends()) < want && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			// A refused request has nothing in flight, so a short wait is enough to see no send.
			if want == 0 {
				time.Sleep(100 * time.Millisecond)
			}
			got := emailer.sends()
			if len(got) != want {
				t.Fatalf("recorded %d sends %v, want %d", len(got), got, want)
			}
			if want > 0 && len(got[0]) != 3 {
				t.Errorf("send reached %v, want the three addresses", got[0])
			}
		})
	}
}

// seedMailingRuns saves a template an admin wrote with an email target, a person's finished run
// that mails, and a finished launch of that template.
func seedMailingRuns(t *testing.T, db agentHoldDB) {
	t.Helper()
	ctx := context.Background()
	targets := []run.NotifyTarget{{Kind: run.NotifyEmail, To: agentTargetsMail}}
	if err := db.templates.Save(ctx, &template.Template{ID: "tpl_mail", Name: "mailing deploy",
		Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "hosts.ini",
		ProjectID: dispatchtest.LocalProjectID, Notifications: targets}); err != nil {
		t.Fatalf("templates.Save() error = %v", err)
	}
	at := time.Now().Add(-time.Hour)
	for _, rn := range []*run.Run{
		{ID: "run_mails", Tool: run.ToolBash, Command: "./rotate-certs.sh",
			Status: run.StatusSucceeded, CreatedAt: at, Actor: "dev-lead-cli", ActorType: "token",
			Notifications: targets},
		{ID: "run_tpl_mails", TemplateID: "tpl_mail", Playbook: "site.yml", Inventory: "hosts.ini",
			ProjectID: dispatchtest.LocalProjectID, Status: run.StatusSucceeded, CreatedAt: at,
			Actor: "dev-lead-cli", ActorType: "token", Notifications: targets},
	} {
		if err := db.runs.Save(ctx, rn); err != nil {
			t.Fatalf("runs.Save(%s) error = %v", rn.ID, err)
		}
	}
}
