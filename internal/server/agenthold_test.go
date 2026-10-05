package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/mcp"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// agentHoldDB holds the stores an agent-hold test serves, all from one backend.
type agentHoldDB struct {
	// runs stores runs.
	runs run.Store
	// users stores accounts.
	users user.Store
	// tokens stores API tokens.
	tokens auth.Store
	// policies stores approval policies.
	policies policy.Store
	// templates stores job templates and saved workflows.
	templates template.Store
	// schedules stores schedules.
	schedules schedule.Store
	// triggers stores webhook triggers.
	triggers trigger.Store
	// inventories stores inventories.
	inventories inventory.Store
	// projects stores projects.
	projects project.Store
}

// agentHoldBackend names a backend and opens a fresh database on it.
type agentHoldBackend struct {
	// Name labels the backend in subtest names.
	Name string
	// Open returns the stores of a fresh database.
	Open func(t *testing.T) agentHoldDB
}

// agentHoldBackends returns the in-memory stores, SQLite, and PostgreSQL, which skips unless the
// test DSN names a server.
func agentHoldBackends() []agentHoldBackend {
	return []agentHoldBackend{
		{Name: "memory", Open: func(*testing.T) agentHoldDB {
			return agentHoldDB{runs: run.NewMemStore(), users: user.NewMemStore(),
				tokens: auth.NewMemStore(), policies: policy.NewMemStore(),
				templates: template.NewMemStore(), schedules: schedule.NewMemStore(),
				triggers: trigger.NewMemStore(), inventories: inventory.NewMemStore(),
				projects: project.NewMemStore()}
		}},
		{Name: "sqlite", Open: func(t *testing.T) agentHoldDB {
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return agentHoldDB{runs: db.Runs(), users: db.Users(), tokens: db.Tokens(),
				policies: db.Policies(), templates: db.Templates(), schedules: db.Schedules(),
				triggers: db.Triggers(), inventories: db.Inventories(), projects: db.Projects()}
		}},
		{Name: "postgres", Open: func(t *testing.T) agentHoldDB {
			if os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN") == "" {
				if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
					t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and " +
						"SWITCHTENDER_TEST_POSTGRES_DSN is not")
				}
				t.Skip("set SWITCHTENDER_TEST_POSTGRES_DSN to run the PostgreSQL case")
			}
			db, err := pgstore.Open(cbkFreshDatabase(t))
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return agentHoldDB{runs: db.Runs(), users: db.Users(), tokens: db.Tokens(),
				policies: db.Policies(), templates: db.Templates(), schedules: db.Schedules(),
				triggers: db.Triggers(), inventories: db.Inventories(), projects: db.Projects()}
		}},
	}
}

// agentHoldServer is a server over one backend with a real dispatcher behind it, and the three
// credentials every case needs: an agent's token, the token of the person it acts for, and an
// admin's.
type agentHoldServer struct {
	// Handler serves the API.
	Handler http.Handler
	// URL is where Handler listens, for the MCP client.
	URL string
	// DB is the backend the server and the dispatcher share.
	DB agentHoldDB
	// Agent is the agent's token, bound to the operator it acts for.
	Agent string
	// Person is the operator's own token, held by a person.
	Person string
	// Admin is an admin's token, held by a person.
	Admin string
}

// newAgentHoldServer serves db with a dispatcher whose runner succeeds at once. With policyStore
// false the dispatcher has no policy store at all, the shape of an install with no rules, which the
// built-in hold must cover as well.
func newAgentHoldServer(t *testing.T, db agentHoldDB, policyStore bool) *agentHoldServer {
	t.Helper()
	ctx := context.Background()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	opts := []dispatch.Option{dispatch.WithNoJanitor(), dispatch.WithInventories(db.inventories)}
	if policyStore {
		opts = append(opts, dispatch.WithPolicies(db.policies))
	}
	d := dispatch.New(db.runs, runner, zap.NewNop(), opts...)
	t.Cleanup(d.Close)

	account := func(name string, role user.Role) *user.User {
		u, err := user.New(name, "a long enough password", role)
		if err != nil {
			t.Fatalf("user.New(%s) error = %v", name, err)
		}
		if err := db.users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save(%s) error = %v", name, err)
		}
		return u
	}
	lead, approver := account("dev-lead", user.RoleOperator), account("approver", user.RoleAdmin)
	mint := func(name, userID, kind string) string {
		plain, tok, err := auth.New(name)
		if err != nil {
			t.Fatalf("auth.New(%s) error = %v", name, err)
		}
		tok.UserID, tok.Kind = userID, kind
		if err := db.tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save(%s) error = %v", name, err)
		}
		return plain
	}
	s := &agentHoldServer{
		DB:     db,
		Agent:  mint("release-agent", lead.ID, auth.KindAgent),
		Person: mint("dev-lead-cli", lead.ID, ""),
		Admin:  mint("approver-cli", approver.ID, ""),
	}
	serverOpts := []Option{WithTokens(db.tokens), WithUsers(db.users),
		WithAudit(audit.NewMemStore()), WithApprover(d), WithRetrier(d),
		WithTemplates(db.templates), WithSchedules(db.schedules),
		WithTriggers(db.triggers, credential.NewSealer("agent-hold-pass", "agent-hold-salt")),
		WithInventories(db.inventories), WithProjects(db.projects),
		WithForgeLinks(forgelink.NewMemStore(), forgelink.App{Provider: "github",
			WebURL: "https://github.example.com", APIURL: "https://github.example.com/api/v3",
			ClientID: "client"}),
		WithReviewReporting("https://switchtender.example.com", nil, time.Minute)}
	if policyStore {
		serverOpts = append(serverOpts, WithPolicies(db.policies))
	}
	s.Handler = New(db.runs, d, zap.NewNop(), serverOpts...).Handler()
	ts := httptest.NewServer(s.Handler)
	t.Cleanup(ts.Close)
	s.URL = ts.URL
	return s
}

// call sends one request as the bearer of token and returns the status and the body.
func (s *agentHoldServer) call(token, method, path, body string) (int, string) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// submit sends a request that creates a run and returns the run, failing unless it was accepted.
func (s *agentHoldServer) submit(t *testing.T, token, method, path, body string) run.Run {
	t.Helper()
	code, got := s.call(token, method, path, body)
	if code != http.StatusAccepted && code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("%s %s = %d, want the run created: %s", method, path, code, got)
	}
	return agentHoldDecode(t, got)
}

// agentHoldDecode decodes a run from a response body.
func agentHoldDecode(t *testing.T, body string) run.Run {
	t.Helper()
	var out run.Run
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode run %q: %v", body, err)
	}
	return out
}

// agentHoldVerdict is what the gate did to a run, read the way the approval view and the evidence
// read it.
type agentHoldVerdict struct {
	// Held reports the run waits for a person.
	Held bool
	// HeldBy names the rule that held it.
	HeldBy string
	// Noted reports the run's notes carry Note.
	Noted bool
}

// verdictOf reads r's verdict, with note the text its notes should carry.
func verdictOf(r run.Run, note string) agentHoldVerdict {
	return agentHoldVerdict{Held: r.Status == run.StatusPendingApproval, HeldBy: r.HeldByPolicy,
		Noted: note != "" && slices.Contains(r.PolicyNotes, note)}
}

// exemptNote is the note a run carries when the named exemption let it proceed.
func exemptNote(name string) string {
	return fmt.Sprintf("requested by an agent bound to account %q, exempt from the default hold "+
		"by policy %q", "dev-lead", name)
}

// TestAgentHoldDirectSubmission proves the default on a fresh install: an agent's run is held for a
// person with no policy written, on every store and with no policy store at all, and says why. The
// control is the same request with the person's own token, which is not held.
func TestAgentHoldDirectSubmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Agent submits with the agent's token rather than the person's.
		Agent bool
		// NoPolicyStore runs the dispatcher with no policy store at all.
		NoPolicyStore bool
		// WantVerdict is what the gate does.
		WantVerdict agentHoldVerdict
	}{{ // Test 0: The agent's run is held by the built-in rule and says so.
		Agent: true,
		WantVerdict: agentHoldVerdict{Held: true, HeldBy: policy.AgentDefaultName,
			Noted: true},
	}, { // Test 1: The person's identical run is not held, the control.
		Agent: false,
	}, { // Test 2: An install with no policy store still holds the agent's run.
		Agent: true, NoPolicyStore: true,
		WantVerdict: agentHoldVerdict{Held: true, HeldBy: policy.AgentDefaultName,
			Noted: true},
	}, { // Test 3: And still lets the person's run through, the control.
		Agent: false, NoPolicyStore: true,
	}}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", backend.Name, testNum), func(t *testing.T) {
				t.Parallel()
				s := newAgentHoldServer(t, backend.Open(t), !test.NoPolicyStore)
				token := s.Person
				if test.Agent {
					token = s.Agent
				}
				got := s.submit(t, token, http.MethodPost, "/v1/runs",
					`{"tool":"bash","command":"./rotate-certs.sh"}`)
				if diff := cmp.Diff(test.WantVerdict,
					verdictOf(got, policy.AgentDefaultName)); diff != "" {
					t.Errorf("verdict mismatch (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestAgentHoldExemption proves an exemption is an explicit rule an admin writes, visible in the
// policy list, that lets exactly the agent runs it matches proceed and names itself on them. The
// controls are an agent run the exemption does not match, which is still held, and the agent
// trying to write its own exemption, which is refused.
func TestAgentHoldExemption(t *testing.T) {
	t.Parallel()
	const name = "agent smoke tests"
	tests := []struct {
		// Command is the agent's run.
		Command string
		// WantVerdict is what the gate does with the exemption in force.
		WantVerdict agentHoldVerdict
		// WantNote is the note the run carries.
		WantNote string
	}{{ // Test 0: The run the exemption matches proceeds and names the exemption.
		Command:     "./smoke-test.sh",
		WantVerdict: agentHoldVerdict{Noted: true},
		WantNote:    exemptNote(name),
	}, { // Test 1: A run it does not match is still held by default, the control.
		Command: "./rotate-certs.sh",
		WantVerdict: agentHoldVerdict{Held: true, HeldBy: policy.AgentDefaultName,
			Noted: true},
		WantNote: policy.AgentDefaultName,
	}}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", backend.Name, testNum), func(t *testing.T) {
				t.Parallel()
				s := newAgentHoldServer(t, backend.Open(t), true)
				body := `{"name":"` + name + `","effect":"exempt","tool":"bash",` +
					`"command_contains":"smoke-test","actor":"release-agent","account":"dev-lead"}`
				// The agent cannot write its own way past the hold.
				if code, got := s.call(s.Agent, http.MethodPost, "/v1/policies",
					body); code != http.StatusForbidden {
					t.Fatalf("agent writing an exemption = %d, want 403: %s", code, got)
				}
				if code, got := s.call(s.Admin, http.MethodPost, "/v1/policies",
					body); code != http.StatusCreated {
					t.Fatalf("admin writing an exemption = %d, want 201: %s", code, got)
				}
				code, listed := s.call(s.Admin, http.MethodGet, "/v1/policies", "")
				if code != http.StatusOK || !strings.Contains(listed, `"effect":"exempt"`) ||
					!strings.Contains(listed, name) {
					t.Fatalf("the exemption is not in the policy list (%d): %s", code, listed)
				}
				got := s.submit(t, s.Agent, http.MethodPost, "/v1/runs",
					`{"tool":"bash","command":"`+test.Command+`"}`)
				if diff := cmp.Diff(test.WantVerdict, verdictOf(got, test.WantNote)); diff != "" {
					t.Errorf("verdict mismatch (-want +got):\n%s\nnotes: %q", diff,
						got.PolicyNotes)
				}
			})
		}
	}
}

// TestAgentHoldExemptionIsCommunity proves the built-in rule costs no policy slot and that a
// Community install can write the exemption, which then takes its one slot. The controls are an
// actor-scoped hold rule, which stays Team, and the second policy, which the cap still refuses.
// Deliberately not parallel: the license is process state.
func TestAgentHoldExemptionIsCommunity(t *testing.T) {
	asCommunity(t, func() {
		tests := []struct {
			// Body is the policy written, in order, on one install.
			Body string
			// WantCode is the answer.
			WantCode int
		}{{ // Test 0: An exemption with a paid criterion is refused as malformed.
			Body:     `{"name":"risky","effect":"exempt","min_risk":"high"}`,
			WantCode: http.StatusBadRequest,
		}, { // Test 1: An exemption cannot be scoped to people.
			Body:     `{"name":"people","effect":"exempt","actor_kind":"human"}`,
			WantCode: http.StatusBadRequest,
		}, { // Test 2: A hold scoped to agents is the full engine, the control for the gate.
			Body:     `{"name":"agents need a person","actor_kind":"agent"}`,
			WantCode: http.StatusForbidden,
		}, { // Test 3: An exemption naming an agent's label without its account is refused.
			Body:     `{"name":"smoke","effect":"exempt","actor":"release-agent","tool":"bash"}`,
			WantCode: http.StatusBadRequest,
		}, { // Test 4: A hold scoped to one account is the full engine, as one scoped to an actor is.
			Body:     `{"name":"lead needs a person","account":"dev-lead"}`,
			WantCode: http.StatusForbidden,
		}, { // Test 5: The exemption naming one agent and its account is Community and fits.
			Body: `{"name":"smoke","effect":"exempt","actor":"release-agent",` +
				`"account":"dev-lead","tool":"bash"}`,
			WantCode: http.StatusCreated,
		}, { // Test 6: It took the one slot, so the cap refuses a second policy.
			Body:     `{"name":"hold everything"}`,
			WantCode: http.StatusForbidden,
		}}
		s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
		for testNum, test := range tests {
			code, got := s.call(s.Admin, http.MethodPost, "/v1/policies", test.Body)
			if code != test.WantCode {
				t.Errorf("test %d: POST /v1/policies %s = %d, want %d: %s", testNum, test.Body,
					code, test.WantCode, got)
			}
		}
	})
}

// TestAgentHoldDryRun proves every dry run an agent asks for waits for a person with no policy
// written, clean or not, since a dry run still runs code with this server's credentials, and that
// the hold says why. An exemption covering the dry run lets it through, and the controls are the
// same dry runs from a person, which proceed.
//
//nolint:funlen // Test function.
func TestAgentHoldDryRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clean := filepath.Join(dir, "look.yml")
	forced := filepath.Join(dir, "restart.yml")
	for path, text := range map[string]string{
		clean: "- hosts: all\n  tasks:\n    - name: Look\n      ansible.builtin.ping:\n",
		forced: "- hosts: all\n  tasks:\n    - name: Restart web\n" +
			"      ansible.builtin.service: name=web state=restarted\n      check_mode: false\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatalf("write playbook: %v", err)
		}
	}
	const checks = `{"name":"lead checks","effect":"exempt","actor":"release-agent",` +
		`"account":"dev-lead","command_contains":"rotate-certs"}`
	tests := []struct {
		// Body is the submission.
		Body string
		// Person submits as the person rather than the agent.
		Person bool
		// Exemption is a policy the admin writes first, empty for none.
		Exemption string
		// WantHeld reports the run waits for a person.
		WantHeld bool
		// WantNote is what the hold note must say when the run is held.
		WantNote string
	}{{ // Test 0: A bash dry run is held.
		Body:     `{"tool":"bash","command":"./rotate-certs.sh","dry_run":true}`,
		WantHeld: true, WantNote: "a dry run still runs code",
	}, { // Test 1: A clean Ansible check is held, since check mode still runs code.
		Body:     `{"playbook":"` + clean + `","inventory":"localhost,","dry_run":true}`,
		WantHeld: true, WantNote: "check mode still runs lookups",
	}, { // Test 2: A check whose playbook forces a real restart is held.
		Body:     `{"playbook":"` + forced + `","inventory":"localhost,","dry_run":true}`,
		WantHeld: true, WantNote: "check mode still runs lookups",
	}, { // Test 3: An exemption covering the agent's dry run lets it through.
		Body:      `{"tool":"bash","command":"./rotate-certs.sh","dry_run":true}`,
		Exemption: checks,
	}, { // Test 4: A person's clean Ansible check proceeds, the control for test 1.
		Body:   `{"playbook":"` + clean + `","inventory":"localhost,","dry_run":true}`,
		Person: true,
	}, { // Test 5: A person's bash dry run proceeds, the control for test 0.
		Body:   `{"tool":"bash","command":"./rotate-certs.sh","dry_run":true}`,
		Person: true,
	}}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", backend.Name, testNum), func(t *testing.T) {
				t.Parallel()
				s := newAgentHoldServer(t, backend.Open(t), true)
				if test.Exemption != "" {
					if code, got := s.call(s.Admin, http.MethodPost, "/v1/policies",
						test.Exemption); code != http.StatusCreated {
						t.Fatalf("admin writing the exemption = %d: %s", code, got)
					}
				}
				token := s.Agent
				if test.Person {
					token = s.Person
				}
				got := s.submit(t, token, http.MethodPost, "/v1/runs", test.Body)
				if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
					t.Fatalf("held = %t, want %t: held by %q", held, test.WantHeld,
						got.HeldByPolicy)
				}
				if !test.WantHeld {
					return
				}
				if got.HeldByPolicy != policy.AgentDefaultName {
					t.Errorf("held_by_policy = %q, want %q", got.HeldByPolicy,
						policy.AgentDefaultName)
				}
				if !strings.Contains(got.HoldNote, test.WantNote) ||
					!strings.Contains(got.HoldNote, "this server's credentials") {
					t.Errorf("hold note = %q, want it to say %q and why", got.HoldNote,
						test.WantNote)
				}
			})
		}
	}
}

// TestAgentHoldApplyWaitsBeforeItPlans proves an agent's Terraform apply is held where it was
// submitted, before anything plans, through the API, and so is a rerun of a person's apply that
// the agent asks for. The release that follows lets it plan, and the dispatcher tests follow it to
// the second approval. The control is the same requests from a person, which are not held.
func TestAgentHoldApplyWaitsBeforeItPlans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path and Body are the request.
		Path string
		// Body is the request body.
		Body string
	}{{ // Test 0: An apply submitted directly.
		Path: "/v1/runs", Body: `{"tool":"terraform","command":"infra"}`,
	}, { // Test 1: A rerun of a person's finished apply.
		Path: "/v1/runs/run_tf_done/rerun", Body: `{}`,
	}}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			for _, person := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s test %d person %v", backend.Name, testNum, person),
					func(t *testing.T) {
						t.Parallel()
						s := newAgentHoldServer(t, backend.Open(t), true)
						done := &run.Run{ID: "run_tf_done", Tool: run.ToolTerraform,
							Command: "infra", Status: run.StatusSucceeded,
							CreatedAt: time.Now().Add(-time.Hour), Actor: "dev-lead-cli",
							ActorType: "token"}
						if err := s.DB.runs.Save(context.Background(), done); err != nil {
							t.Fatalf("runs.Save() error = %v", err)
						}
						token := s.Agent
						if person {
							token = s.Person
						}
						got := s.submit(t, token, http.MethodPost, test.Path, test.Body)
						if held := got.Status == run.StatusPendingApproval; held == person {
							t.Fatalf("held = %t for person %v (held by %q)", held, person,
								got.HeldByPolicy)
						}
						if !person && !strings.Contains(got.HoldNote, "before anything plans") {
							t.Errorf("hold note = %q, want it to say the apply waits before "+
								"it plans", got.HoldNote)
						}
					})
			}
		}
	}
}

// agentHoldSeed stores what the indirect paths start from: a template, a saved workflow, a person's
// finished run, a person's failed run with per-host results, a person's failed split with a failed
// shard, and a drift check that found drift.
func agentHoldSeed(t *testing.T, db agentHoldDB) {
	t.Helper()
	ctx := context.Background()
	for _, tpl := range []*template.Template{
		{ID: "tpl_deploy", Name: "deploy", Tool: run.ToolAnsible, Playbook: "site.yml",
			Inventory: "hosts.ini"},
		{ID: "tpl_flow", Name: "release flow", Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "./build.sh"},
			{Name: "ship", Tool: run.ToolBash, Command: "./ship.sh"}}},
	} {
		if err := db.templates.Save(ctx, tpl); err != nil {
			t.Fatalf("templates.Save(%s) error = %v", tpl.ID, err)
		}
	}
	at := time.Now().Add(-time.Hour)
	person := func(id string, status run.Status) *run.Run {
		return &run.Run{ID: id, Playbook: "site.yml", Inventory: "hosts.ini",
			Status: status, CreatedAt: at, Actor: "dev-lead-cli", ActorType: "token"}
	}
	if err := db.runs.Save(ctx, person("run_done", run.StatusSucceeded)); err != nil {
		t.Fatalf("runs.Save(run_done) error = %v", err)
	}
	broken := person("run_broken", run.StatusRunning)
	if err := db.runs.Save(ctx, broken); err != nil {
		t.Fatalf("runs.Save(run_broken) error = %v", err)
	}
	if err := db.runs.SaveHostSummary(ctx, broken.ID,
		[]run.HostSummary{{Host: "web01", OK: 2}, {Host: "web02", Failures: 1}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	broken.Status = run.StatusFailed
	if err := db.runs.Save(ctx, broken); err != nil {
		t.Fatalf("runs.Save(run_broken) terminal error = %v", err)
	}
	split, count := person("run_split", run.StatusFailed), 1
	split.Kind, split.ShardCount = run.KindSplit, &count
	if err := db.runs.Save(ctx, split); err != nil {
		t.Fatalf("runs.Save(run_split) error = %v", err)
	}
	index := 0
	shard := person("run_split_c0", run.StatusFailed)
	shard.ParentID, shard.ShardIndex, shard.ShardCount = &split.ID, &index, &count
	if err := db.runs.Save(ctx, shard); err != nil {
		t.Fatalf("runs.Save(run_split_c0) error = %v", err)
	}
	// A drift check of a person's that found drift on web01, naming no stored object, so the
	// reconcile it proposes reaches the gate rather than stopping at a missing reference.
	check := person("chk_web", run.StatusRunning)
	check.DryRun = true
	if err := db.runs.Save(ctx, check); err != nil {
		t.Fatalf("runs.Save(chk_web) error = %v", err)
	}
	if err := db.runs.SaveHostSummary(ctx, check.ID, []run.HostSummary{{Host: "web01",
		Changed: 3, Worst: "changed", RanAt: at}}); err != nil {
		t.Fatalf("SaveHostSummary(chk_web) error = %v", err)
	}
	check.Status = run.StatusSucceeded
	if err := db.runs.Save(ctx, check); err != nil {
		t.Fatalf("runs.Save(chk_web) terminal error = %v", err)
	}
}

// TestAgentHoldIndirectRuns proves every way an agent can cause a run without submitting it
// directly is held by default with no policy written: a template launch, a saved workflow's launch,
// a workflow submitted whole, a rerun and a failed-host relaunch of a person's run, a retry of a
// person's failed split, and a drift reconcile. The control for each is the same request with the
// person's own token, which is not held, except the reconcile, which always waits and so is held
// for the person too, but by the request rather than by the built-in rule. Each request is made on
// an install of its own, since a rerun inside the dedupe window returns the rerun already made.
func TestAgentHoldIndirectRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Path and Body are the request.
		Path string
		// Body is the request body.
		Body string
		// WantPersonHeldBy names what holds the person's request, empty when nothing does.
		WantPersonHeldBy string
	}{{ // Test 0: A template launch.
		Path: "/v1/templates/tpl_deploy/launch", Body: `{}`,
	}, { // Test 1: A saved workflow's launch.
		Path: "/v1/templates/tpl_flow/launch", Body: `{}`,
	}, { // Test 2: A workflow submitted whole.
		Path: "/v1/pipelines",
		Body: `{"name":"release","steps":[{"name":"build","tool":"bash","command":"./build.sh"},` +
			`{"name":"ship","tool":"bash","command":"./ship.sh"}]}`,
	}, { // Test 3: A rerun of a person's finished run.
		Path: "/v1/runs/run_done/rerun", Body: `{}`,
	}, { // Test 4: A relaunch of a person's failed hosts.
		Path: "/v1/runs/run_broken/relaunch-failed", Body: `{}`,
	}, { // Test 5: A retry of a person's failed split, whose shards come with it.
		Path: "/v1/runs/run_split/retry", Body: `{}`,
	}, { // Test 6: A drift reconcile, which waits for the person too, by request.
		Path: "/v1/drift/reconcile", Body: `{"host":"web01"}`,
		WantPersonHeldBy: "requested at submission",
	}}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", backend.Name, testNum), func(t *testing.T) {
				t.Parallel()
				s := newAgentHoldServer(t, backend.Open(t), true)
				agentHoldSeed(t, s.DB)
				control := newAgentHoldServer(t, backend.Open(t), true)
				agentHoldSeed(t, control.DB)
				byAgent := s.submit(t, s.Agent, http.MethodPost, test.Path, test.Body)
				want := agentHoldVerdict{Held: true, HeldBy: policy.AgentDefaultName, Noted: true}
				if diff := cmp.Diff(want,
					verdictOf(byAgent, policy.AgentDefaultName)); diff != "" {
					t.Errorf("agent verdict mismatch (-want +got):\n%s", diff)
				}
				if byAgent.Actor != "release-agent" || byAgent.ActorType != "agent" {
					t.Errorf("the run names %s (%s), want the agent who asked", byAgent.Actor,
						byAgent.ActorType)
				}
				children, err := s.DB.runs.Shards(context.Background(), byAgent.ID)
				if err != nil {
					t.Fatalf("Shards() error = %v", err)
				}
				for _, child := range children {
					if child.Status != run.StatusPendingApproval {
						t.Errorf("child %s of the agent's run is %s, want held with it",
							child.ID, child.Status)
					}
				}
				byPerson := control.submit(t, control.Person, http.MethodPost, test.Path,
					test.Body)
				wantPerson := agentHoldVerdict{Held: test.WantPersonHeldBy != "",
					HeldBy: test.WantPersonHeldBy}
				if diff := cmp.Diff(wantPerson,
					verdictOf(byPerson, policy.AgentDefaultName)); diff != "" {
					t.Errorf("person verdict mismatch (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestAgentHoldMCP proves an agent proposing through the MCP tools is held with no policy written,
// for a template and for a run it composes. The control is the same tool driven by the person's
// token, which is not held.
func TestAgentHoldMCP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Tool is the MCP tool called.
		Tool string
		// Args are its arguments.
		Args string
		// Agent calls with the agent's token rather than the person's.
		Agent bool
	}{{ // Test 0: propose_run launches a template and is held.
		Tool: "propose_run", Args: `{"template_id":"tpl_deploy"}`, Agent: true,
	}, { // Test 1: The person's propose_run is not held, the control.
		Tool: "propose_run", Args: `{"template_id":"tpl_deploy"}`,
	}, { // Test 2: propose_adhoc_run composes a run and is held.
		Tool: "propose_adhoc_run", Args: `{"tool":"bash","command":"./rotate-certs.sh"}`,
		Agent: true,
	}, { // Test 3: The person's propose_adhoc_run is not held, the control.
		Tool: "propose_adhoc_run", Args: `{"tool":"bash","command":"./rotate-certs.sh"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newAgentHoldServer(t, agentHoldBackends()[1].Open(t), true)
			agentHoldSeed(t, s.DB)
			token := s.Person
			if test.Agent {
				token = s.Agent
			}
			client, err := mcp.NewClient(s.URL, token, 5*time.Second)
			if err != nil {
				t.Fatalf("mcp.NewClient() error = %v", err)
			}
			var tool *mcp.Tool
			for _, candidate := range mcp.Tools(client, mcp.Options{AllowAdhoc: true}) {
				if candidate.Name == test.Tool {
					tool = &candidate
				}
			}
			if tool == nil {
				t.Fatalf("the MCP server does not offer %s", test.Tool)
			}
			reply, err := tool.Run(context.Background(), json.RawMessage(test.Args))
			if err != nil {
				t.Fatalf("%s error = %v", test.Tool, err)
			}
			got := agentHoldDecode(t, reply)
			want := agentHoldVerdict{}
			if test.Agent {
				want = agentHoldVerdict{Held: true, HeldBy: policy.AgentDefaultName, Noted: true}
			}
			if diff := cmp.Diff(want, verdictOf(got, policy.AgentDefaultName)); diff != "" {
				t.Errorf("verdict mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAgentHoldLaunchersAreRefused proves an agent cannot create or edit anything that later
// launches a run under another actor: a schedule fires as the scheduler and a trigger as a webhook,
// so a launcher an agent wrote would run its change under somebody else's name with no hold. It
// cannot write a template or a saved workflow either, which a person or a schedule then launches,
// nor the inventories and projects those reach, nor a policy, nor a forge link, which is what lets
// a pull request comment act for an account. The control for each is the admin, whose identical
// request the role gate lets through.
func TestAgentHoldLaunchersAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Method and Path name the request.
		Method string
		// Path is the request path.
		Path string
		// Body is the request body.
		Body string
	}{
		{ // Test 0: Creating a schedule.
			Method: http.MethodPost, Path: "/v1/schedules",
			Body: `{"name":"nightly","cron":"0 2 * * *","template_id":"tpl_deploy"}`},
		{ // Test 1: Editing a schedule.
			Method: http.MethodPut, Path: "/v1/schedules/sch_nightly",
			Body: `{"name":"nightly","cron":"0 3 * * *","template_id":"tpl_deploy"}`},
		{ // Test 2: Creating a webhook trigger.
			Method: http.MethodPost, Path: "/v1/triggers",
			Body: `{"name":"on push","template_id":"tpl_deploy"}`},
		{ // Test 3: Editing a webhook trigger.
			Method: http.MethodPut, Path: "/v1/triggers/trg_push",
			Body: `{"name":"on push","template_id":"tpl_flow"}`},
		{ // Test 4: Rotating a trigger's secret, which hands back a working hook.
			Method: http.MethodPost, Path: "/v1/triggers/trg_push/rotate-secret"},
		{ // Test 5: Creating a template.
			Method: http.MethodPost, Path: "/v1/templates",
			Body: `{"name":"agent deploy","playbook":"site.yml","inventory":"hosts.ini"}`},
		{ // Test 6: Editing a template.
			Method: http.MethodPut, Path: "/v1/templates/tpl_deploy",
			Body: `{"name":"deploy","playbook":"destroy.yml","inventory":"hosts.ini"}`},
		{ // Test 7: Creating a saved workflow.
			Method: http.MethodPost, Path: "/v1/templates",
			Body: `{"name":"agent flow","steps":[{"name":"a","tool":"bash","command":"./a.sh"}]}`},
		{ // Test 8: Minting a host callback key for a template.
			Method: http.MethodPost, Path: "/v1/templates/tpl_deploy/callback-key"},
		{ // Test 9: Creating an inventory.
			Method: http.MethodPost, Path: "/v1/inventories",
			Body: `{"name":"prod","content":"web01\n"}`},
		{ // Test 10: Creating a project.
			Method: http.MethodPost, Path: "/v1/projects",
			Body: `{"name":"infra","url":"https://git.example.com/infra.git"}`},
		{ // Test 11: Writing a policy, its own exemption included.
			Method: http.MethodPost, Path: "/v1/policies",
			Body: `{"name":"let me through","effect":"exempt"}`},
		{ // Test 12: Linking a forge account, which a pull request comment acts through.
			Method: http.MethodPost, Path: "/v1/me/forge-links",
			Body: `{"provider":"github","api_url":"https://github.example.com/api/v3"}`},
	}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", backend.Name, testNum), func(t *testing.T) {
				t.Parallel()
				s := newAgentHoldServer(t, backend.Open(t), true)
				agentHoldSeed(t, s.DB)
				ctx := context.Background()
				if err := s.DB.schedules.Save(ctx, &schedule.Schedule{ID: "sch_nightly",
					Name: "nightly", Cron: "0 2 * * *", TemplateID: "tpl_deploy",
					Enabled: true, CreatedAt: time.Now()}); err != nil {
					t.Fatalf("schedules.Save() error = %v", err)
				}
				if err := s.DB.triggers.Save(ctx, &trigger.Trigger{ID: "trg_push",
					Name: "on push", TemplateID: "tpl_deploy", TokenHash: "hook-token-hash",
					CreatedAt: time.Now()}); err != nil {
					t.Fatalf("triggers.Save() error = %v", err)
				}
				if code, got := s.call(s.Agent, test.Method, test.Path,
					test.Body); code != http.StatusForbidden {
					t.Errorf("agent %s %s = %d, want 403: %s", test.Method, test.Path, code, got)
				}
				// The person the agent acts for may do this, so the refusal is the agent's alone.
				if test.Path == "/v1/me/forge-links" {
					if code, got := s.call(s.Person, test.Method, test.Path,
						test.Body); code == http.StatusForbidden {
						t.Errorf("person %s %s = 403, want it allowed: %s", test.Method,
							test.Path, got)
					}
					return
				}
				if code, got := s.call(s.Admin, test.Method, test.Path,
					test.Body); code == http.StatusForbidden || code == http.StatusNotFound {
					t.Errorf("admin %s %s = %d, want it allowed: %s", test.Method, test.Path,
						code, got)
				}
			})
		}
	}
}
