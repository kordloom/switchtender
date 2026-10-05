package run

import (
	"context"
	"encoding/json"
	"fmt"
)

// Initiator is the identity evidence of a run an AI agent asked for: which agent asked, the account
// whose authority it acted under, and who issued it that authority. An agent can be provisioned by
// an organization admin for somebody else, so the account an agent is bound to and the person who
// minted its token are recorded separately rather than assumed to be one person.
//
// Every value is observed from the agent's token when the request arrives and recorded at that
// moment, never looked up later: a token can be revoked, renamed, or rebound long before anybody
// reads the evidence, and the record has to say who acted then.
type Initiator struct {
	// InitiatedBy is the agent, by its token's label, the actor the chain names on its requests.
	InitiatedBy string `json:"initiated_by"`
	// CredentialID is the agent token's id, which tells two agents sharing a label apart.
	CredentialID string `json:"credential_id,omitempty"`
	// BoundTo is the account the agent's token is bound to, whose authority it acts under. When a
	// rule requires an independent approver, this account counts as the requester.
	BoundTo string `json:"bound_to,omitempty"`
	// ProvisionedBy names who minted the agent's token, as the token recorded its issuer. Empty for
	// a token minted before issuers were recorded.
	ProvisionedBy string `json:"provisioned_by,omitempty"`
	// ProvisionedByType is how the issuer authenticated when minting: session, token, cli, or system.
	ProvisionedByType string `json:"provisioned_by_type,omitempty"`
}

// Clone returns a copy of i, nil for nil.
func (i *Initiator) Clone() *Initiator {
	if i == nil {
		return nil
	}
	out := *i
	return &out
}

// WithInitiator records the identity evidence of the agent that asked for the run. A nil initiator
// is a no-op, so a derived run passes its source's value without checking it first.
func WithInitiator(i *Initiator) SubmitOption {
	return func(r *Run) {
		if i != nil {
			r.Initiator = i.Clone()
		}
	}
}

// WithRequireReason carries a rule's reason requirement onto a run it holds, keeping the stricter
// of what the run already asks and what the rule asks, so a requirement is never lowered.
func WithRequireReason(requirement string) SubmitOption {
	return func(r *Run) {
		if reasonRank(requirement) > reasonRank(r.RequireReason) {
			r.RequireReason = requirement
		}
	}
}

// reasonRank orders reason requirements: none, then denials, then always.
func reasonRank(requirement string) int {
	switch requirement {
	case "always":
		return 2
	case "denials":
		return 1
	}
	return 0
}

// initiatorKey types the context value carrying the agent identity of the request in flight.
type initiatorKey struct{}

// WithInitiatorContext carries the identity evidence of the agent making the request, so a run
// created while handling it records who asked, whose authority it used, and who provisioned it. It
// is set by the auth gate for an agent's token, at the same point the actor and the audit receipt
// are placed on the context. A nil initiator leaves the context as it is.
func WithInitiatorContext(ctx context.Context, i *Initiator) context.Context {
	if i == nil {
		return ctx
	}
	return context.WithValue(ctx, initiatorKey{}, i.Clone())
}

// InitiatorFrom returns the agent identity carried on ctx, nil when the request in flight was not
// made by an agent.
func InitiatorFrom(ctx context.Context) *Initiator {
	i, _ := ctx.Value(initiatorKey{}).(*Initiator)
	return i.Clone()
}

// InitiatorColumn encodes an initiator for storage, the empty string for a run no agent asked for.
func InitiatorColumn(i *Initiator) string {
	if i == nil {
		return ""
	}
	b, err := json.Marshal(i)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseInitiatorColumn decodes a stored initiator. An empty column is a run no agent asked for. A
// column that does not decode is reported rather than read as empty, because the initiator is
// evidence: reading a corrupted one as absent would present an agent's run as a person's.
func ParseInitiatorColumn(s string) (*Initiator, error) {
	if s == "" {
		return nil, nil
	}
	var out Initiator
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse run initiator: %w", err)
	}
	return &out, nil
}
