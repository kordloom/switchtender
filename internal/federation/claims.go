package federation

import (
	"fmt"
	"strconv"
	"strings"
)

// Run types a token names in its run_type claim and subject.
const (
	// RunTypeApply is a run that may change what it targets.
	RunTypeApply = "apply"
	// RunTypeDryRun is a run in its tool's no-change mode: Ansible check mode, a Terraform plan.
	RunTypeDryRun = "dry_run"
)

// Launcher types a token names in its launcher_type claim.
const (
	// LauncherPerson is a run a signed-in person, an API token, or the command line launched.
	LauncherPerson = "person"
	// LauncherAgent is a run an AI agent's token launched.
	LauncherAgent = "agent"
	// LauncherSchedule is a run a schedule fired.
	LauncherSchedule = "schedule"
	// LauncherTrigger is a run a webhook trigger fired.
	LauncherTrigger = "trigger"
	// LauncherPipeline is a step a pipeline run started.
	LauncherPipeline = "pipeline"
	// LauncherSystem is a run nothing attributable launched, such as a seeded demo run.
	LauncherSystem = "system"
)

// Purposes a token names in its purpose claim: why it was minted.
const (
	// PurposeRun is a token for a run executing.
	PurposeRun = "run"
	// PurposeGateDownload is a token for the module download the approval gate runs, with the
	// run's own credentials, before it records the run. A gate that refuses the run leaves the
	// token naming a run id no run was recorded under, and this says why.
	PurposeGateDownload = "gate_download"
	// PurposeReviewPrecheck is a token for the module download a pull request review runs before it
	// plans, to learn whether the plan is safe to run at all. No run exists yet, so the token names
	// no run, and names the pull request and commit instead.
	PurposeReviewPrecheck = "review_precheck"
)

// noneSegment stands in for an empty subject segment, so a run with no project still has a subject
// whose shape a trust policy pattern can match.
const noneSegment = "none"

// Claims is the payload of a run identity token. The registered claims come first, then the claims
// that identify the run. The names are part of the product's contract with every cloud trust policy
// written against them, so they are stable and documented in docs/federation.md.
//
// Every claim is always present, empty when it does not apply, so a policy condition that reads one
// never meets a missing claim.
type Claims struct {
	// Issuer is the iss claim, the issuer URL the cloud fetches the discovery document from.
	Issuer string `json:"iss"`
	// Subject is the sub claim, built by Subject from the run's organization, project, template,
	// environment, run type, and approval.
	Subject string `json:"sub"`
	// Audience is the aud claim, the relying party the token is for, set by the credential.
	Audience string `json:"aud"`
	// ExpiresAt is the exp claim, in seconds since the epoch.
	ExpiresAt int64 `json:"exp"`
	// IssuedAt is the iat claim, in seconds since the epoch.
	IssuedAt int64 `json:"iat"`
	// NotBefore is the nbf claim, in seconds since the epoch.
	NotBefore int64 `json:"nbf"`
	// ID is the jti claim, unique per token.
	ID string `json:"jti"`
	// RunID is the run the token was minted for, empty for a review pre-check, which no run exists
	// for.
	RunID string `json:"run_id"`
	// Purpose says why the token was minted: run, gate_download, or review_precheck.
	Purpose string `json:"purpose"`
	// PullRequest is the pull or merge request number a review's plan or pre-check is for, empty
	// for anything else.
	PullRequest string `json:"pull_request"`
	// ParentRunID is the pipeline or split run this run is a step or shard of, empty for a
	// top-level run.
	ParentRunID string `json:"parent_run_id"`
	// OrgID is the organization that owns the run.
	OrgID string `json:"org_id"`
	// ProjectID is the git project the run reads from.
	ProjectID string `json:"project_id"`
	// TemplateID is the job template the run executes.
	TemplateID string `json:"template_id"`
	// Environment is the environment the credential names, set by whoever manages the credential
	// and never by whoever launches the run.
	Environment string `json:"environment"`
	// CredentialID is the federated credential the token was minted for.
	CredentialID string `json:"credential_id"`
	// Tool is the engine the run executes with: ansible, terraform, opentofu, bash, powershell,
	// python, or go.
	Tool string `json:"tool"`
	// RunType is apply or dry_run.
	RunType string `json:"run_type"`
	// Source is what fired the run: api, template, schedule, trigger, rerun, reconcile, or propose.
	Source string `json:"source"`
	// CommitSHA is the project commit the run executes, empty for a run with no project.
	CommitSHA string `json:"commit_sha"`
	// LauncherType is who or what launched the run: person, agent, schedule, trigger, pipeline, or
	// system.
	LauncherType string `json:"launcher_type"`
	// Actor is the name the launching credential recorded.
	Actor string `json:"actor"`
	// ActorType is how the launcher authenticated: session, token, cli, agent, or webhook.
	ActorType string `json:"actor_type"`
	// ActorUserID is the account behind the launching credential.
	ActorUserID string `json:"actor_user_id"`
	// Approved reports that an approval decision released the run, recorded in the audit chain and
	// bound to the run's spec.
	Approved bool `json:"approved"`
	// ApprovedBy is the approver the decision names, empty when the run was not approved or the
	// install keeps no audit trail to read the decision from.
	ApprovedBy string `json:"approved_by"`
	// ApprovedByType is how the approver authenticated.
	ApprovedByType string `json:"approved_by_type"`
	// ApprovalPolicy is the approval rule that held the run, empty when no rule held it.
	ApprovalPolicy string `json:"approval_policy"`
}

// Subject builds the sub claim for c: the pairs org, project, template, env, run_type, and
// approved, joined by colons, for example
//
//	org:org_acme:project:proj_web:template:tpl_deploy:env:prod:run_type:apply:approved:true
//
// An empty segment reads none. The segments run from the broadest scope to the narrowest, so a
// trust policy that allows anything below a point matches it with one trailing wildcard. Ids rather
// than names fill the segments: an id cannot be taken by renaming another object, and a name can.
func Subject(c Claims) (string, error) {
	segments := []struct {
		name, value string
	}{
		{"org", c.OrgID}, {"project", c.ProjectID}, {"template", c.TemplateID},
		{"env", c.Environment}, {"run_type", c.RunType},
	}
	parts := make([]string, 0, 2*len(segments)+2)
	for _, s := range segments {
		v := s.value
		if v == "" {
			v = noneSegment
		}
		// A colon inside a value would move every segment after it, so one run could read as
		// another to a policy that matches by position.
		if strings.ContainsAny(v, ":*?") {
			return "", fmt.Errorf("%w: the %s %q contains a separator or wildcard", ErrSubject, s.name, v)
		}
		parts = append(parts, s.name, v)
	}
	parts = append(parts, "approved", strconv.FormatBool(c.Approved))
	return strings.Join(parts, ":"), nil
}
