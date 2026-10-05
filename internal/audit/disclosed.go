package audit

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Disclosed member states, the three a verifier reports for a member the chain link does not
// commit: checked against a commitment, redacted, or unchecked.
const (
	// MemberChecked is a disclosed member held against a commitment that held.
	MemberChecked = "checked"
	// MemberUnchecked is a disclosed member nothing this verifier reaches commits.
	MemberUnchecked = "unchecked"
	// MemberRedacted is a disclosed member that states a privacy redaction removed what it stood for.
	MemberRedacted = "redacted"
)

// boundMembers are the payload members the chain link commits, so none of them is disclosed.
var boundMembers = map[string]bool{
	"actor": true, "method": true, "path": true, "actor_type": true, "on_behalf_of": true,
	"content_digest": true, "install_id": true,
}

// disclosedMember is what this product knows about one member it discloses beside the link: the
// commitment it is held against, the member it travels with, if any, and the records it is read on.
type disclosedMember struct {
	// by names the commitment.
	by string
	// with is the member this one travels with, so the pair is one record.
	with string
	// records names the record kinds whose claims carry and check the member. A member that names
	// any is read only on a claim of one of them, and is an ordinary member everywhere else.
	records []string
}

// Record kinds, the records a claim is by the method and path its link commits. They are the kinds
// LoomSeal's schema/claim-members.json declares, so this verifier reads a member on exactly the
// claims the open one does.
const (
	// recordDecision is an approval decision, any entry of MethodDecision.
	recordDecision = "decision"
	// recordCorrection is a correction to a decision's reason, an entry of MethodReason at the path
	// a correction is recorded at.
	recordCorrection = "correction"
	// recordOutcome is a run's outcome, an entry of MethodRun at the path an outcome is recorded at.
	recordOutcome = "outcome"
)

// Details the classification gives for a record member it did not read and for a record checked
// against the legacy unkeyed digest form, worded as the open format words them.
const (
	// elsewhereDetail is why a record member on a claim that is not its record is unchecked.
	elsewhereDetail = "the claim is not a record that carries it, so nothing commits it"
	// legacyDetail is what a record checked against the legacy unkeyed form was held against.
	legacyDetail = "the entry's content_digest in the legacy unkeyed form, which anyone who can " +
		"guess the body can confirm"
)

// recordKindOf names the record a claim is by the method and path its link commits, read by their
// exact names, or returns empty for a claim that is no record.
func recordKindOf(payload map[string]any) string {
	method, _ := payload["method"].(string)
	path, _ := payload["path"].(string)
	switch {
	case method == MethodDecision:
		return recordDecision
	case method == MethodReason &&
		pathMatches("/runs/{run}/decisions/{decision}/corrections/{correction}", path):
		return recordCorrection
	case method == MethodRun && pathMatches("/runs/{run}/outcome/{status}", path):
		return recordOutcome
	}
	return ""
}

// pathMatches reports whether path fits pattern segment by segment, a {name} segment matching any
// non-empty segment and every other segment matching only itself.
func pathMatches(pattern, path string) bool {
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if w != got[i] {
			return false
		}
	}
	return true
}

// recordMembers lists, in name order, the members a record of kind is read for.
func recordMembers(kind string) []string {
	var out []string
	for name, m := range knownDisclosures[ClaimType] {
		if slices.Contains(m.records, kind) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// caseVariant finds a payload member whose name differs from one of members only in case, and
// returns it. A reader folding case, as encoding/json does onto a struct field, would take it for
// the member a check read by its exact name, so a claim carrying one is refused rather than read
// either way.
func caseVariant(payload map[string]any, members []string) (string, bool) {
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, m := range members {
			if name != m && strings.EqualFold(name, m) {
				return name, true
			}
		}
	}
	return "", false
}

// firstKeyed returns the index of the first audit claim whose content digest is in the keyed or the
// exact form, or -1 when none is. Every entry this product records after it is keyed, so no entry
// after that claim predates the keyed form.
func firstKeyed(claims []BundleClaim) int {
	for i, c := range claims {
		if c.Type != ClaimType {
			continue
		}
		digest, _ := c.Payload["content_digest"].(string)
		if strings.HasPrefix(digest, keyedDigestPrefix) || strings.HasPrefix(digest, ExactDigestPrefix) {
			return i
		}
	}
	return -1
}

// isUnkeyed reports whether a content digest is in the legacy unkeyed form.
func isUnkeyed(digest string) bool {
	return strings.HasPrefix(digest, "sha256:")
}

// knownDisclosures are the members this product discloses beside the link, by claim type. It is the
// same set the open format declares in LoomSeal's schema/claim-members.json, and the
// cross-verification holds the two to each other, so a member this product starts disclosing
// without the format declaring it fails that check rather than riding unchecked.
var knownDisclosures = map[string]map[string]disclosedMember{
	ClaimType: {
		"decision_body": {by: "the entry's content_digest", records: []string{recordDecision}},
		"decision_nonce": {by: "the entry's content_digest", with: "decision_body",
			records: []string{recordDecision}},
		"correction_body": {by: "the entry's content_digest", records: []string{recordCorrection}},
		"correction_nonce": {by: "the entry's content_digest", with: "correction_body",
			records: []string{recordCorrection}},
		"reason_text": {by: "the reason_commitment its record body carries",
			records: []string{recordDecision, recordCorrection}},
		"reason_random": {by: "the reason_commitment its record body carries", with: "reason_text",
			records: []string{recordDecision, recordCorrection}},
		"reason_redacted": {by: "a privacy redaction removed the reason and kept its commitment",
			records: []string{recordDecision, recordCorrection}},
		"spec_body": {by: "the spec_digest a verified decision or outcome names",
			records: []string{recordOutcome}},
		"outcome_body": {by: "the entry's content_digest", records: []string{recordOutcome}},
		"outcome_nonce": {by: "the entry's content_digest", with: "outcome_body",
			records: []string{recordOutcome}},
	},
	SpanClaimType: {
		"stream":    {by: "the format, which defines the chain stream alone"},
		"beat":      {by: "the span path the link commits"},
		"count":     {by: "the span path the link commits"},
		"cadence_s": {by: "the span path the link commits"},
	},
}

// DisclosedMember is one payload member the chain link does not commit, and what this verifier held
// it against.
type DisclosedMember struct {
	// Claim is the claim's index in the document.
	Claim int `json:"claim"`
	// Member is the payload member's name.
	Member string `json:"member"`
	// State is checked, unchecked, or redacted.
	State string `json:"state"`
	// Detail names the commitment it was checked against, the redaction, or why it is unchecked.
	Detail string `json:"detail"`
	// With names the member this one travels with, so a reader counts the pair as one record.
	With string `json:"with,omitempty"`
}

// memberSettled is what a check established about one member.
type memberSettled struct {
	// state is checked, unchecked, or redacted.
	state string
	// detail is why, for a member a check left unchecked.
	detail string
}

// settle records what a check established about one member of one claim.
func (r *BundleReport) settle(claim int, member, state, detail string) {
	if r.memberStates == nil {
		r.memberStates = map[int]map[string]memberSettled{}
	}
	if r.memberStates[claim] == nil {
		r.memberStates[claim] = map[string]memberSettled{}
	}
	r.memberStates[claim][member] = memberSettled{state: state, detail: detail}
}

// settleChecked records each member as checked, for a check that held over all of them.
func (r *BundleReport) settleChecked(claim int, members ...string) {
	for _, m := range members {
		r.settle(claim, m, MemberChecked, "")
	}
}

// settleReason records the reason members a verified record's reason left in the given state.
func (r *BundleReport) settleReason(claim int, state string) {
	switch state {
	case ReasonVerified:
		r.settleChecked(claim, "reason_text", "reason_random")
	case ReasonRedacted:
		r.settle(claim, "reason_redacted", MemberRedacted, "")
	}
}

// classifyDisclosed lists every member the chain link does not commit, in the state the checks left
// it: checked, redacted, or unchecked. A member no check confirmed is unchecked, and so is a member
// this product never discloses, since nothing here commits it. A tree's leaf commits every member,
// so nothing in a tree bundle is disclosed beside it.
func classifyDisclosed(b *Bundle, rep *BundleReport) {
	if b.Chain != nil && b.Chain.Profile == TreeProfile {
		return
	}
	for i, c := range b.Claims {
		names := make([]string, 0, len(c.Payload))
		for name := range c.Payload {
			if !boundMembers[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		kind := ""
		if c.Type == ClaimType {
			kind = recordKindOf(c.Payload)
		}
		for _, name := range names {
			known, isKnown := knownDisclosures[c.Type][name]
			// A member counts with the one it travels with only when that one is on the same claim.
			// Alone, it is a record of its own, or a stray nonce could ride unchecked and uncounted.
			with := known.with
			if _, present := c.Payload[with]; !present {
				with = ""
			}
			m := DisclosedMember{Claim: i, Member: name, With: with}
			switch settled := rep.memberStates[i][name]; {
			case settled.state == MemberChecked:
				m.State, m.Detail = settled.state, known.by
				if settled.detail != "" {
					m.Detail = settled.detail
				}
			case settled.state == MemberRedacted:
				category, _ := c.Payload[name].(string)
				m.State, m.Detail = settled.state, category+", "+known.by
			case settled.state == MemberUnchecked:
				m.State, m.Detail = settled.state, settled.detail
			case len(known.records) > 0 && !slices.Contains(known.records, kind):
				m.State, m.Detail = MemberUnchecked, elsewhereDetail
			case isKnown:
				m.State, m.Detail = MemberUnchecked, "its check did not pass"
			default:
				m.State = MemberUnchecked
				m.Detail = "this product does not disclose " + strconv.Quote(name) + " on a " +
					c.Type + " claim, and the chain link does not commit it"
			}
			rep.Disclosed = append(rep.Disclosed, m)
			if m.State == MemberUnchecked && m.With == "" {
				rep.DisclosedUnchecked++
			}
		}
	}
}

// verifySpanBinding binds each span beat's members to the path its link commits. The link commits
// actor, method, and path, and a beat's members ride beside them, so without this a producer
// re-signing its own bundle could widen the cadence to hide a gap or renumber its beats with every
// link still recomputing. The path is the one SpanPath writes, so a beat whose members disagree
// with it is not the beat the chain recorded.
func verifySpanBinding(claims []BundleClaim, rep *BundleReport) {
	spanMembers := make([]string, 0, len(knownDisclosures[SpanClaimType]))
	for name := range knownDisclosures[SpanClaimType] {
		spanMembers = append(spanMembers, name)
	}
	sort.Strings(spanMembers)
	for i, c := range claims {
		if c.Type != SpanClaimType {
			continue
		}
		// A name that differs from a span member only in case would show a reader that folds case a
		// beat or a cadence the path never committed.
		if variant, found := caseVariant(c.Payload, spanMembers); found {
			rep.CaseVariants = append(rep.CaseVariants, fmt.Sprintf("claim %d %s", i, variant))
			continue
		}
		path, _ := c.Payload["path"].(string)
		beat, count, cadence, ok := ParseSpanPath(path)
		stream, _ := c.Payload["stream"].(string)
		if !ok || stream != "chain" || !sameInt(c.Payload["beat"], beat) ||
			!sameInt(c.Payload["count"], count) || !sameInt(c.Payload["cadence_s"], int64(cadence)) {
			rep.SpansUnbound = true
			continue
		}
		rep.settleChecked(i, "stream", "beat", "count", "cadence_s")
	}
}

// sameInt reports whether a decoded JSON number is exactly want.
func sameInt(v any, want int64) bool {
	f, ok := v.(float64)
	return ok && f == float64(want)
}
