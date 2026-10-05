package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// maxRunAncestry bounds how far a run's identity walks up through parents to find the run that was
// launched and approved. A step sits under its pipeline and a shard under its split, so four is
// ample, and the bound keeps a corrupted parent link from looping.
const maxRunAncestry = 4

// WithFederation lets a run that carries a federated credential mint an identity token from issuer.
// Without it such a run is refused at submit and fails at execution with the reason.
func WithFederation(issuer *federation.Issuer) Option {
	return func(c *config) { c.federation = issuer }
}

// tokenCommitter is the actor a token issuance entry names: the issuer, acting for whoever launched
// the run.
const tokenCommitter = "system:federation"

// TokenEvidence returns the recorder that commits each federated token issuance to audits as a
// chain entry naming the run, the credential, the id of the key that signed the token, the token's
// id, and its expiry. A process that federates hands it to its issuer, so a token's signing key is
// on record before the token reaches a tool or a cloud, whichever process minted it and whatever
// minted it for.
func TokenEvidence(audits audit.Store) federation.IssuanceRecorder {
	if audits == nil {
		panic("dispatch: TokenEvidence needs an audit store")
	}
	return federation.IssuanceRecorderFunc(func(ctx context.Context, is federation.Issuance) error {
		return outcome.CommitTokenIssuance(ctx, audits, outcome.TokenIssuance{
			RunID: is.RunID, ParentRunID: is.ParentRunID, CredentialID: is.CredentialID,
			KeyID: is.KeyID, TokenID: is.TokenID, IssuedAt: is.IssuedAt, ExpiresAt: is.ExpiresAt,
			OnBehalfOf: is.Actor,
		}, tokenCommitter)
	})
}

// checkFederated confirms a federated credential can mint on this process and that the run carries
// no second credential of the same kind, whose variables would silently replace this one's, and
// returns its parsed settings. seen records the kinds already counted, by credential id.
func (d *Dispatcher) checkFederated(c *credential.Credential,
	seen map[credential.Kind]string) (federation.Config, error) {
	if d.federation == nil {
		return federation.Config{}, fmt.Errorf("%w: credential %q is %s, which needs an issuer URL "+
			"set with --federation-issuer on every serve and worker that executes runs",
			federation.ErrNotConfigured, c.Name, c.Kind)
	}
	if prior, dup := seen[c.Kind]; dup && prior != c.ID {
		return federation.Config{}, fmt.Errorf("%w: %s and %s are both %s", federation.ErrDuplicate,
			prior, c.ID, c.Kind)
	}
	seen[c.Kind] = c.ID
	if c.Source != "" && c.Source != credential.SourceLocal {
		return federation.Config{}, fmt.Errorf("%w: credential %q is %s, which stores no secret and "+
			"so reads from no source", federation.ErrSetting, c.Name, c.Kind)
	}
	cfg, err := federation.ParseSettings(c.Kind, c.Settings)
	if err != nil {
		return federation.Config{}, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	return cfg, nil
}

// mintPurposeKey is the context key carrying why a federated token is being minted.
type mintPurposeKey struct{}

// withMintPurpose returns ctx saying a token minted under it is for purpose.
func withMintPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, mintPurposeKey{}, purpose)
}

// mintPurposeOf returns why a token minted under ctx is being minted: a run executing, unless the
// gate or a review's pre-check said otherwise.
func mintPurposeOf(ctx context.Context) string {
	if p, ok := ctx.Value(mintPurposeKey{}).(string); ok && p != "" {
		return p
	}
	return federation.PurposeRun
}

// deliverFederated mints the run's identity token for c and returns what it contributes to the run,
// writing any file into dir.
func (d *Dispatcher) deliverFederated(ctx context.Context, r *run.Run, c *credential.Credential,
	dryRun bool, dir string, seen map[credential.Kind]string) (federation.Delivery, error) {
	cfg, err := d.checkFederated(c, seen)
	if err != nil {
		return federation.Delivery{}, err
	}
	delivery, err := d.federation.Deliver(ctx, cfg, d.runClaims(ctx, r, c.ID, dryRun), dir)
	if err != nil {
		return delivery, fmt.Errorf("credential %q: %w", c.Name, err)
	}
	return delivery, nil
}

// runClaims gathers the identity a token states for r. What launched the run and whether it was
// approved are read from the run that was launched, the top of r's parent chain, because a shard or
// a pipeline step is released by its parent's approval and carries its parent's requester.
func (d *Dispatcher) runClaims(ctx context.Context, r *run.Run, credID string, dryRun bool) federation.Claims {
	root := d.launchedRun(ctx, r)
	c := federation.Claims{
		RunID: r.ID, Purpose: mintPurposeOf(ctx), PullRequest: r.Labels[run.LabelPullRequest],
		OrgID: r.OrgID, ProjectID: r.ProjectID, TemplateID: r.TemplateID,
		CredentialID: credID, Tool: run.NormalizeTool(r.Tool), Source: root.Source,
		CommitSHA: r.CommitSHA, LauncherType: launcherType(r, root),
		Actor: root.Actor, ActorType: root.ActorType, ActorUserID: root.ActorUserID,
		ApprovalPolicy: root.HeldByPolicy, RunType: federation.RunTypeApply,
	}
	if r.ParentID != nil {
		c.ParentRunID = *r.ParentID
	}
	if dryRun {
		c.RunType = federation.RunTypeDryRun
	}
	// A review's pre-check asks before any run exists, so its token names no run. It names the pull
	// request and the commit the pre-check read, which is what it was for.
	if c.Purpose == federation.PurposeReviewPrecheck {
		c.RunID = ""
		if c.CommitSHA == "" {
			c.CommitSHA = r.PinnedCommit
		}
	}
	// The digest is stamped only by an approval decision that the chain recorded, and execution has
	// already refused a run whose spec moved since, so its presence is what approved means here.
	if root.ApprovedSpecDigest != "" {
		c.Approved = true
		c.ApprovedBy, c.ApprovedByType = d.approverOf(ctx, root)
	}
	return c
}

// launchedRun walks r's parent chain to the run that was launched, returning r itself when it has
// no parent or a parent cannot be read.
func (d *Dispatcher) launchedRun(ctx context.Context, r *run.Run) *run.Run {
	cur := r
	for range maxRunAncestry {
		if cur.ParentID == nil || *cur.ParentID == "" {
			return cur
		}
		parent, err := d.store.Get(ctx, *cur.ParentID)
		if err != nil {
			d.log.Warn("dispatch: read parent for run identity: "+err.Error(),
				zap.String("run_id", cur.ID))
			return cur
		}
		cur = parent
	}
	return cur
}

// launcherType names who or what launched a run, in the vocabulary the launcher_type claim uses.
func launcherType(r, root *run.Run) string {
	switch {
	case r.StepName != "" || r.StepIndex != nil:
		return federation.LauncherPipeline
	case root.Source == "schedule":
		return federation.LauncherSchedule
	case root.Source == "trigger" || root.ActorType == "webhook":
		return federation.LauncherTrigger
	case root.ActorType == "agent":
		return federation.LauncherAgent
	case root.Actor != "" || root.ActorUserID != "" || root.ActorType != "":
		return federation.LauncherPerson
	default:
		return federation.LauncherSystem
	}
}

// approverOf reads who approved r from the decision entry the audit chain recorded for it. The scan
// starts at the entry that recorded r's request, since a decision always follows the request it
// decides, and stops at the first approval for r. It answers empty when the install keeps no trail
// or the run carries no receipt to start from, rather than reading the whole chain on every launch.
func (d *Dispatcher) approverOf(ctx context.Context, r *run.Run) (string, string) {
	if d.audits == nil || r.AuditReceipt == "" {
		return "", ""
	}
	seqText, _, _ := strings.Cut(r.AuditReceipt, ":")
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq < 1 {
		return "", ""
	}
	path := "/runs/" + r.ID + "/decision/approved"
	var name, kind string
	err = d.audits.ChainScan(ctx, seq-1, func(e *audit.Entry) error {
		if e.Method == audit.MethodDecision && e.Path == path {
			name, kind = e.Actor, e.ActorType
			return errDecisionFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDecisionFound) {
		d.log.Warn("dispatch: read approval for run identity: "+err.Error(), zap.String("run_id", r.ID))
		return "", ""
	}
	return name, kind
}
