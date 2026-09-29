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
// An anchor names a coordinate and the value that was at it. Three things can be true of one:
//
//   - The chain reaches that coordinate and the value matches. The chain still contains the history
//     the anchor was taken over.
//   - The chain reaches that coordinate and the value differs. History was rewritten under the
//     anchor.
//   - The chain ends before that coordinate. Entries that existed when the anchor was taken are
//     gone.
//
// Only the first is a pass. The other two are the finding, and an unanchored chain proves neither,
// which is why an install that never anchors gets no assurance from this rather than a false one.
//
// A linear anchor's coordinate is an entry hash at a chain position. A tree anchor's is the Merkle
// root over the first Seq entries, so it is checked by recomputing that tree with the leaves bound
// to installID, exactly as TreeHead builds it, never against the linear hash map.
func CheckAnchors(entries []*Entry, anchors []*Anchor, installID string) (ok bool, results []AnchorCheck) {
	s := NewAnchorScanner(anchors, installID)
	for _, e := range entries {
		s.Feed(e)
	}
	return s.Results()
}

// anchorVerdicts turns what the scan captured at anchored coordinates into per-anchor verdicts.
func anchorVerdicts(s *AnchorScanner) (ok bool, results []AnchorCheck) {
	ok = true
	results = make([]AnchorCheck, 0, len(s.anchors))
	for _, a := range s.anchors {
		res := AnchorCheck{Anchor: a}
		switch a.Shape {
		case AnchorShapeTree:
			res = treeVerdict(s, a)
		case AnchorShapeLinear:
			res = linearVerdict(s, a)
		default:
			res.Problem = fmt.Sprintf("this anchor carries the unknown coordinate shape %q, so it "+
				"cannot be checked", a.Shape)
		}
		if !res.Reached {
			ok = false
		}
		results = append(results, res)
	}
	return ok, results
}

// linearVerdict holds one linear anchor against the entry hash captured at its position.
func linearVerdict(s *AnchorScanner, a *Anchor) AnchorCheck {
	res := AnchorCheck{Anchor: a}
	switch link, found := s.atSeq[a.Seq]; {
	case !found && a.Seq > s.highest:
		res.Problem = fmt.Sprintf("the chain ends at entry %d, and this anchor was taken over "+
			"entry %d, so %d entries that existed when it was taken are missing",
			s.highest, a.Seq, a.Seq-s.highest)
	case !found:
		res.Problem = fmt.Sprintf("the chain has no entry %d, which this anchor was taken over",
			a.Seq)
	case link != a.Link:
		res.Problem = fmt.Sprintf("entry %d is now link %s, and this anchor recorded %s, so "+
			"the history under the anchor was rewritten", a.Seq, link, a.Link)
	default:
		res.Reached = true
	}
	return res
}

// treeVerdict holds one tree anchor against the root recomputed at its size.
func treeVerdict(s *AnchorScanner, a *Anchor) AnchorCheck {
	res := AnchorCheck{Anchor: a}
	switch root, found := s.rootAt[a.Seq]; {
	case s.installID == "":
		res.Problem = "this install's identity is unavailable, so the tree this anchor fixes a " +
			"root of cannot be recomputed and the anchor cannot be checked"
	case s.treeErr != nil:
		res.Problem = fmt.Sprintf("the chain's entries could not be hashed into the tree this "+
			"anchor fixes a root of: %v", s.treeErr)
	case !found && a.Seq > s.fed:
		res.Problem = fmt.Sprintf("the chain holds %d entries, and this anchor fixed the root over "+
			"the first %d, so %d entries that existed when it was taken are missing",
			s.fed, a.Seq, a.Seq-s.fed)
	case !found:
		res.Problem = fmt.Sprintf("no root could be recomputed at tree size %d", a.Seq)
	case root != a.Link && a.InstallID != "" && a.InstallID != s.installID:
		// A tree root is computed over leaves bound to the install's identity, so the same untouched
		// chain under a different identity produces a different root. Two ordinary events cause that: a
		// database restored without the key file that made it, and a deployment where each replica
		// mints its own key. Calling either one a rewrite teaches an operator to disbelieve the message
		// that matters.
		res.Problem = fmt.Sprintf("this anchor was taken by install %s and this process is install "+
			"%s, so the tree it fixed a root of cannot be recomputed here: restore the producer key "+
			"that made this chain, or point this process at it, and check again. Nothing here says "+
			"the history changed", a.InstallID, s.installID)
	case root != a.Link:
		res.Problem = fmt.Sprintf("the tree over the first %d entries now has root %s, and this "+
			"anchor recorded %s, so the history under the anchor was rewritten", a.Seq, root, a.Link)
	default:
		res.Reached = true
	}
	return res
}
