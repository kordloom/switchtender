package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	osuser "os/user"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/user"
)

// The agent the demo shows, the account it acts for, and the routine work an exemption lets
// through. The agent is an ordinary agent token bound to an ordinary operator account, minted the
// way switchtender token new --user ops-lead --agent mints one, and every request it makes goes
// through the real API and its auth gate with that token, so its runs carry the agent actor type,
// the account, and the initiator exactly as any agent's do, and every refusal it meets is the gate's.
const (
	// agentAccount is the account the agent's token is bound to.
	agentAccount = "ops-lead"
	// agentLabel is the agent token's label, the name its runs are recorded under.
	agentLabel = "remediation-agent"
	// agentExemptionName names the exemption the agent's smoke test runs under.
	agentExemptionName = "remediation-agent smoke tests"
	// agentSmokeMatch is the text the exemption matches, narrow enough to cover the smoke test and
	// nothing else the agent might ask for.
	agentSmokeMatch = "post-deploy smoke checks"
	// agentIncident labels the agent's work with the incident it is remediating.
	agentIncident = "INC-2207"
)

// agentTokenChange is what the chain entry recording the agent token's minting commits to, the
// same summary the command line commits: which token, for whom, and with what role, never its
// secret.
type agentTokenChange struct {
	// ID is the token's id.
	ID string `json:"id"`
	// Name is the token's label.
	Name string `json:"name,omitempty"`
	// Kind is agent.
	Kind string `json:"kind,omitempty"`
	// UserID is the account the token is bound to.
	UserID string `json:"user_id,omitempty"`
	// Role is the role the token acts with, capped for an agent.
	Role string `json:"role,omitempty"`
}

// clockedAudit is the chain the agent's requests are recorded on while seeding. The auth gate leaves
// an entry's time for the store to stamp, and the store stamps the wall clock, which would pin every
// later seeded entry forward to the seed instant. This stamps the seed clock instead, the clock
// every other seeded entry reads, and changes nothing else about the entry.
type clockedAudit struct {
	audit.Store
	// clock is the seed clock, nil to leave an entry's time to the store.
	clock *SeedClock
}

// Append stamps an undated entry with the seed clock and appends it.
func (c clockedAudit) Append(ctx context.Context, e *audit.Entry) error {
	if e.At.IsZero() && c.clock != nil {
		e.At = c.clock.Now()
	}
	return c.Store.Append(ctx, e)
}

// seedAgent shows the agent gate doing its work, all of it real. The agent asks to restart the app
// on one web host and to apply the network root, and both wait for a person by default, the apply
// before anything plans. Its routine smoke test runs under an exemption, when the install's license
// holds one more rule. It asks for a workflow that would apply Terraform, and the dispatcher refuses
// it at submission. It tries to approve its own held run, and the gate refuses it at the door. Both
// refusals are on the chain, recorded the way every refused request is.
func seedAgent(ctx context.Context, d Deps, playbook, inv, tfDir string, log *zap.Logger) {
	if d.Tokens == nil || d.Users == nil || d.Policies == nil || d.Audit == nil || d.Runs == nil {
		return
	}
	plain, err := mintAgentToken(ctx, d)
	if err != nil {
		log.Warn("demo: seed agent: " + err.Error())
		return
	}
	exempt := seedAgentExemption(ctx, d, log)
	api := server.New(d.Runs, d.Submitter, zap.NewNop(), server.WithTokens(d.Tokens),
		server.WithUsers(d.Users), server.WithAudit(clockedAudit{Store: d.Audit, clock: d.Clock}),
		server.WithPolicies(d.Policies)).Handler()
	call := func(method, path string, body any) (int, *run.Run) {
		return agentRequest(ctx, api, plain, method, path, body)
	}
	labels := map[string]string{"env": "prod", "incident": agentIncident}
	restart := filepath.Join(filepath.Dir(playbook), "restart-app.yml")

	// The fix an agent reaches for first: restart the app on the one web host that stopped
	// answering. It waits for a person, held by default.
	code, held := call(http.MethodPost, "/v1/runs", map[string]any{
		"playbook": restart, "inventory": inv, "limit": "web01", "labels": labels})
	expect(log, "the agent's restart", code, http.StatusAccepted)
	stepClock(d)

	// An apply of the network root, held before anything plans. Releasing it would plan it, and the
	// apply its plan proposes would wait for a second approval carrying the saved plan.
	code, _ = call(http.MethodPost, "/v1/runs", map[string]any{
		"tool": run.ToolTerraform, "command": tfDir,
		"labels": map[string]string{"env": "prod", "team": "network", "incident": agentIncident}})
	expect(log, "the agent's apply", code, http.StatusAccepted)
	stepClock(d)

	// The routine check the exemption lets through, which runs and succeeds. Without the exemption
	// it waits like the rest, which is what the gate does on an install that cannot hold the rule.
	code, smoke := call(http.MethodPost, "/v1/runs", map[string]any{
		"tool": run.ToolBash, "command": scriptSmoke, "labels": labels})
	expect(log, "the agent's smoke test", code, http.StatusAccepted)
	if smoke != nil && exempt {
		settle(ctx, d, smoke.ID)
	} else {
		stepClock(d)
	}

	// A workflow that would carry the apply inside it, where one approval of the workflow would
	// release a plan nobody saw. The dispatcher refuses it at submission and records the refusal.
	code, _ = call(http.MethodPost, "/v1/pipelines", map[string]any{
		"name": "Remediate web and network", "inventory": inv,
		"steps": []map[string]any{
			{"name": "restart", "playbook": restart},
			{"name": "apply", "tool": run.ToolTerraform, "command": tfDir},
		}})
	expect(log, "the agent's workflow", code, http.StatusForbidden)
	stepClock(d)

	// The agent tries to approve its own held restart. The gate refuses it at the door, below the
	// role every approval needs, and records the attempt before it answers.
	if held != nil {
		code, _ = call(http.MethodPost, "/v1/runs/"+held.ID+"/approve", map[string]any{})
		expect(log, "the agent's self-approval", code, http.StatusForbidden)
		stepClock(d)
	}
}

// mintAgentToken mints the agent's token the way the command line mints one with --user and
// --agent: a fresh token of kind agent, bound to the account, recording the host account behind the
// command line as its issuer, with the minting committed to the chain before the token is saved.
// It returns the plaintext, which the seeder presents to the API and nothing stores.
func mintAgentToken(ctx context.Context, d Deps) (string, error) {
	account, err := d.Users.FindByUsername(ctx, agentAccount)
	if err != nil {
		return "", fmt.Errorf("find the agent's account %q: %w", agentAccount, err)
	}
	plain, tok, err := auth.New(agentLabel)
	if err != nil {
		return "", fmt.Errorf("mint the agent token: %w", err)
	}
	tok.Kind, tok.UserID = auth.KindAgent, account.ID
	tok.CreatedBy, tok.CreatedByType = cliIssuer(), "cli"
	tok.CreatedAt = seedTime(d)
	body, err := json.Marshal(agentTokenChange{ID: tok.ID, Name: tok.Name, Kind: tok.Kind,
		UserID: tok.UserID, Role: string(user.AgentRole(account.Role))})
	if err != nil {
		return "", fmt.Errorf("encode the token change: %w", err)
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return "", fmt.Errorf("digest the token change: %w", err)
	}
	if err := d.Audit.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: tok.CreatedAt, Actor: tok.CreatedBy, ActorType: tok.CreatedByType,
		Method: audit.MethodCLI, Path: "/cli/token/new", ContentDigest: digest, Nonce: nonce,
	}); err != nil {
		return "", fmt.Errorf("record the token's minting: %w", err)
	}
	if err := d.Tokens.Save(ctx, tok); err != nil {
		return "", fmt.Errorf("save the agent token: %w", err)
	}
	return plain, nil
}

// cliIssuer names the host account behind the command line, the way the command line names itself
// as a token's issuer.
func cliIssuer() string {
	if u, err := osuser.Current(); err == nil && u.Username != "" {
		return "cli:" + u.Username
	}
	return "cli:unknown"
}

// seedAgentExemption stores the exemption the agent's smoke test runs under, when the license holds
// it. It is checked the way the API checks a rule an admin writes: the full engine when the rule
// uses it, and the number of rules the tier holds. An install whose license refuses it keeps the
// smoke test waiting like the agent's other runs, which is what that install would do, and says why.
func seedAgentExemption(ctx context.Context, d Deps, log *zap.Logger) bool {
	p := policy.NewPolicy(agentExemptionName)
	p.Effect, p.Actor, p.Account = policy.EffectExempt, agentLabel, agentAccount
	p.Tool, p.CommandContains = run.ToolBash, agentSmokeMatch
	p.CreatedAt = seedTime(d)
	if err := p.Validate(); err != nil {
		log.Warn("demo: seed the agent's exemption: " + err.Error())
		return false
	}
	existing, err := d.Policies.List(ctx)
	if err != nil {
		log.Warn("demo: seed the agent's exemption: " + err.Error())
		return false
	}
	if p.Advanced() {
		if err := license.Allow(license.FeaturePolicyFull); err != nil {
			log.Warn("demo: the agent's exemption is not seeded: " + err.Error())
			return false
		}
	}
	if err := license.AllowPolicies(len(existing) + 1); err != nil {
		log.Warn("demo: the agent's exemption is not seeded, so its smoke test waits like its "+
			"other runs: "+err.Error(), zap.Int("rules", len(existing)))
		return false
	}
	if err := d.Policies.Save(ctx, p); err != nil {
		log.Warn("demo: seed the agent's exemption: " + err.Error())
		return false
	}
	return true
}

// agentRequest sends one request to the API as the bearer of token and returns the status and, for
// an answer that is a run, the run.
func agentRequest(ctx context.Context, api http.Handler, token, method, path string,
	body any) (int, *run.Run) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	var out run.Run
	if rec.Code < http.StatusBadRequest && json.Unmarshal(rec.Body.Bytes(), &out) == nil &&
		out.ID != "" {
		return rec.Code, &out
	}
	return rec.Code, nil
}

// expect logs a request whose answer is not the one the scenario shows, so a seed that cannot show
// a step says so rather than leaving it out quietly.
func expect(log *zap.Logger, what string, got, want int) {
	if got != want {
		log.Warn(fmt.Sprintf("demo: %s answered %d, want %d", what, got, want))
	}
}

// stepClock steps the seed clock past a run that stays waiting, as settle does once a run lands.
func stepClock(d Deps) {
	if d.Clock != nil {
		d.Clock.advance(seedRunGap)
	}
}

// seedWaitingWorkflow seeds a person's release that is waiting at its approval step right now: the
// build ran, and the workflow paused at the step that asks whether to ship it. It is the workflow
// the approvals queue shows waiting, with what already ran and what each answer runs next.
func seedWaitingWorkflow(ctx context.Context, d Deps, playbook, inv string, log *zap.Logger) {
	if d.Runs == nil {
		return
	}
	steps := []run.PipelineStep{
		{Name: "build", Playbook: playbook},
		{Name: "approve-release", Type: run.StepApproval, DependsOn: []string{"build"},
			Description: "Release 4.3 built cleanly on every host. Ship it to production?"},
		{Name: "deploy", Playbook: playbook, DependsOn: []string{"approve-release"}},
	}
	pipe, err := d.Submitter.SubmitPipeline(ctx, "Release 4.3", inv, steps,
		seedOpts(ctx, d, "api", "", "admin",
			map[string]string{"env": "prod", "ticket": "REL-43", "change": "REL-43"})...)
	if err != nil {
		log.Warn("demo: seed the waiting release: " + err.Error())
		return
	}
	waitAtStep(ctx, d.Runs, pipe.ID)
	stepClock(d)
}

// waitAtStep polls until the workflow id waits at an approval step, or ends, or a timeout elapses.
func waitAtStep(ctx context.Context, store run.Store, id string) {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if pending, err := run.PendingApprovalSteps(ctx, store, id); err == nil && len(pending) > 0 {
			return
		}
		if r, err := store.Get(ctx, id); err == nil && r.Status.Terminal() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
