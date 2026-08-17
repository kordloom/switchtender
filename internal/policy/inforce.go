package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxNamedRules bounds how many rule labels a description carries. An install with hundreds of rules
// would otherwise put hundreds of strings on every run record and in every receipt; the digest still
// covers all of them, and the count says how many there were.
const maxNamedRules = 40

// InForceSet describes the approval rules that were in force at one moment: a digest over the whole
// set and the labels of the rules in it.
//
// It exists because the boundary could only prove what it stopped. A held run names the rule that held
// it and the decision that released it is signed, so the record is strong for a change somebody
// questioned. For a change that sailed through, nothing was recorded at all, which made "no rule
// applied to this run" and "there were no rules" the same record. An operator who deleted a gate an
// hour before a change, or loosened a threshold and put it back, left nothing an auditor could see. A
// run now carries this, so two runs can be compared and a set that changed between them is visible.
type InForceSet struct {
	// Digest is the hex SHA-256 over the canonical form of every rule in the set, order-independent.
	// It changes when a rule is added, removed, or edited in any way that affects what it does.
	Digest string `json:"digest"`
	// Count is how many rules were in force, which is the number the digest covers whether or not
	// every one of them is named below.
	Count int `json:"count"`
	// Rules are the rules in force, each rendered as its label and what it does, sorted. Capped at
	// maxNamedRules; the digest and the count always cover the whole set.
	Rules []string `json:"rules,omitempty"`
}

// InForce describes the rule set as it stands. A nil or empty set is described as empty rather than
// left blank: "there were no rules" is the fact an auditor most needs to be able to read.
func InForce(policies []*Policy) InForceSet {
	canonical := make([]string, 0, len(policies))
	labels := make([]string, 0, len(policies))
	for _, p := range policies {
		if p == nil {
			continue
		}
		canonical = append(canonical, canonicalRule(p))
		labels = append(labels, describeRule(p))
	}
	// Sorting is what makes the digest a property of the set rather than of the order a store happened
	// to return it in: two servers reading the same policies must agree, and a reordered list is not a
	// change anybody made.
	sort.Strings(canonical)
	sort.Strings(labels)
	sum := sha256.Sum256([]byte(strings.Join(canonical, "\n")))
	out := InForceSet{Digest: hex.EncodeToString(sum[:]), Count: len(canonical)}
	if len(labels) > maxNamedRules {
		labels = append(labels[:maxNamedRules:maxNamedRules],
			fmt.Sprintf("and %d more rules, all covered by the digest", len(labels)-maxNamedRules))
	}
	out.Rules = labels
	return out
}

// canonicalRule renders one rule as the bytes the digest covers: every field that decides what the rule
// does, and nothing that does not. The id, the name, and the creation time are left out on purpose, so
// renaming a rule is not reported as a change to what it enforces, while every criterion and every
// effect is.
func canonicalRule(p *Policy) string {
	shape := struct {
		Tool            string `json:"tool,omitempty"`
		CommandContains string `json:"command_contains,omitempty"`
		InventoryID     string `json:"inventory_id,omitempty"`
		ActorKind       string `json:"actor_kind,omitempty"`
		Actor           string `json:"actor,omitempty"`
		MinRisk         string `json:"min_risk,omitempty"`
		Effect          string `json:"effect,omitempty"`
		ExcludeDryRun   bool   `json:"exclude_dry_run,omitempty"`
		MaxDestroy      int    `json:"max_destroy"`
		DistinctApprove bool   `json:"require_distinct_approver,omitempty"`
	}{
		Tool: p.Tool, CommandContains: p.CommandContains, InventoryID: p.InventoryID,
		ActorKind: p.ActorKind, Actor: p.Actor, MinRisk: p.MinRisk, Effect: p.Effect,
		ExcludeDryRun: p.ExcludeDryRun, MaxDestroy: p.MaxDestroy,
		DistinctApprove: p.RequireDistinctApprover,
	}
	raw, err := json.Marshal(shape)
	if err != nil {
		// A rule that cannot be encoded still has to change the digest rather than vanish from it.
		return fmt.Sprintf("unencodable rule %s", p.Label())
	}
	return string(raw)
}

// describeRule renders one rule the way a person reads it: what it is called and what it does.
func describeRule(p *Policy) string {
	effect := "requires approval"
	switch {
	case p.Denies():
		effect = "denies"
	case p.MaxDestroy >= 0:
		effect = fmt.Sprintf("requires approval over %d destroys", p.MaxDestroy)
	}
	if p.RequireDistinctApprover {
		effect += ", by someone other than the requester"
	}
	return p.Label() + ": " + effect
}
