package audit

import "fmt"

// AnchorCheck is the result of holding a chain against the anchors recorded over it.
type AnchorCheck struct {
	// Anchor is the anchor this result is about.
	Anchor *Anchor
	// Reached is true when the chain still contains the anchored link at the anchored position.
	Reached bool
	// Problem describes what is wrong when Reached is false, in words an operator can act on.
	Problem string
}

// CheckAnchors holds a chain against the anchors recorded over it and reports what each one proves.
//
// A hash chain shows that nothing in it was altered. It cannot show that nothing was removed from
// the end, because a prefix of a valid chain is itself a valid chain: drop the last thousand entries
// and what remains still verifies, with the same genesis and an unbroken run of links. That is the
// one thing anchoring exists to catch, and nothing was checking it. Verification reported a healthy
// chain while the evidence that disproved it sat unread in the same database.
//
// An anchor names a sequence and the link that was at it. Three things can be true of one:
//
//   - The chain reaches that sequence and the link matches. The chain still contains the history the
//     anchor was taken over.
//   - The chain reaches that sequence and the link differs. History was rewritten under the anchor.
//   - The chain ends before that sequence. Entries that existed when the anchor was taken are gone.
//
// Only the first is a pass. The other two are the finding, and an unanchored chain proves neither,
// which is why an install that never anchors gets no assurance from this rather than a false one.
func CheckAnchors(entries []*Entry, anchors []*Anchor) (ok bool, results []AnchorCheck) {
	byPosition := make(map[int64]string, len(entries))
	var highest int64
	for _, e := range entries {
		byPosition[e.Seq] = e.Hash
		if e.Seq > highest {
			highest = e.Seq
		}
	}

	ok = true
	results = make([]AnchorCheck, 0, len(anchors))
	for _, a := range anchors {
		res := AnchorCheck{Anchor: a}
		switch link, found := byPosition[a.Seq]; {
		case !found && a.Seq > highest:
			res.Problem = fmt.Sprintf("the chain ends at entry %d, and this anchor was taken over "+
				"entry %d, so %d entries that existed when it was taken are missing",
				highest, a.Seq, a.Seq-highest)
		case !found:
			res.Problem = fmt.Sprintf("the chain has no entry %d, which this anchor was taken over",
				a.Seq)
		case link != a.Link:
			res.Problem = fmt.Sprintf("entry %d is now link %s, and this anchor recorded %s, so "+
				"the history under the anchor was rewritten", a.Seq, link, a.Link)
		default:
			res.Reached = true
		}
		if !res.Reached {
			ok = false
		}
		results = append(results, res)
	}
	return ok, results
}
