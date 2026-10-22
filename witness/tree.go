package witness

import (
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/kordloom/loomseal/merkle"
)

// maxProofHashes bounds how many hashes a push's consistency proof may carry. A proof grows with
// the logarithm of the tree size, so between trees of at most maxBeatJump entries it is never
// longer than forty-one hashes, and a longer one is not a proof of any tree this witness accepts.
// The bound is checked before a hash is decoded or folded, so one push costs a fixed amount of
// hashing however it was built.
const maxProofHashes = 64

// CheckPush holds one push from an enrolled install against the witness's memory of that install.
// It returns the checkpoint the witness holds after the push and every finding, and like Check it
// is pure. prev may be nil before the first push.
//
// The beat is held by Check as a one-element feed with the install id as the subject, so every
// rule a pulled feed answers to applies to a push unchanged. The tree is then held to the root the
// witness last adopted: the push's consistency proof must show that its root at its size grew from
// the held root by appending only. A beat number alone can be reissued and a head alone can be
// recomputed over a rewritten chain, but a rewrite at or before the held size changes the root
// there, and no proof reaches the held root from it. That is why a push without a proof is never
// countersigned.
//
// A push is adopted only when its beat is the newest the witness has seen, Check adopted it, and
// its tree extends the held one. The first push has no held tree to extend, so its root is adopted
// with a first_root finding naming the size, which says from where the witness's memory holds. A
// push that is not adopted leaves prev's memory standing, so a caller tells a countersigned push
// from a refused one by whether the returned checkpoint holds the push's beat, root, and size, and
// a history refused once is refused again on every push built on it. An identical repeat of the
// push the witness last adopted adopts nothing and raises nothing, whatever base it proves from,
// so a retry answers what the first attempt did. Three pushes are errors, which return no memory
// and raise nothing: a push out of form is ErrMalformed, a push proving from a size the witness
// does not hold is ErrProofBase naming the size it does hold, and a push proving from the held size
// while claiming a smaller tree is ErrStale naming both, because from one push a replica's
// superseded beat and a chain cut back look the same.
func CheckPush(prev *Checkpoint, installID string, p Push, now time.Time) (*Checkpoint, []Finding, error) {
	if installID == "" {
		return nil, nil, fmt.Errorf("%w: a push is held against an install, and none was named",
			ErrMalformed)
	}
	if p.InstallID != installID {
		return nil, nil, fmt.Errorf("%w: the push names install %q, not %q", ErrMalformed,
			clip(p.InstallID, 48), clip(installID, 48))
	}
	if prev != nil && prev.InstallID != "" && prev.InstallID != installID {
		return nil, nil, fmt.Errorf("checkpoint remembers install %s, not %s; use one memory per "+
			"install", prev.InstallID, installID)
	}
	next, found, err := Check(prev, installID, []Beat{p.Beat()}, now)
	if err != nil {
		return nil, nil, err
	}
	// A stalled feed describes a pulled server whose newest beat stopped moving. A push is not a
	// feed, and riding on a refusal the finding would put a second accusation with a false detail
	// beside the one the push earned.
	found = slices.DeleteFunc(found, func(f Finding) bool { return f.Kind == "stalled_feed" })
	// A push the witness cannot check is an error, not testimony: it said nothing checkable about
	// the chain. What Check says about a beat it had to discard describes an empty feed, and read as
	// a push it would accuse the chain of losing its tail when all that arrived was a push out of
	// form.
	if why := pushProblem(&p); why != "" {
		return nil, nil, fmt.Errorf("%w: this push cannot be checked, so nothing it claims was "+
			"adopted: %s", ErrMalformed, why)
	}
	if holdsPush(prev, &p) {
		return held(prev, installID), nil, nil
	}
	base := heldSize(prev)
	if p.FromSize != base {
		return nil, nil, fmt.Errorf("%w: this witness holds the tree at size %d, and the push "+
			"proves from size %d", ErrProofBase, base, p.FromSize)
	}
	// No proof runs from a larger tree to a smaller one, so a push behind the held size would read
	// as a rewrite. It is a replica's superseded beat or a chain cut back, and one push cannot say
	// which. A cut cannot hide: the chain reaches the held size again with another root, which the
	// tree rule below refuses, or every bundle it makes ends before what was witnessed.
	if p.Size < base {
		return nil, nil, fmt.Errorf("%w: this witness holds the tree at size %d, and the push "+
			"claims size %d, behind it", ErrStale, base, p.Size)
	}
	first := prev == nil || prev.Root == ""
	if !first && !merkle.VerifyConsistency(base, p.Size, hashBytes(prev.Root), hashBytes(p.Root),
		hashList(p.Proof)) {
		return held(prev, installID), append(found, Finding{Kind: "rewritten_history",
			Detail: fmt.Sprintf("the push's tree at size %d with root %s does not extend the tree "+
				"this witness holds at size %d with root %s, so the history it countersigned was "+
				"rewritten", p.Size, p.Root, base, prev.Root),
			Key: fmt.Sprintf("rewritten_history from witnessed tree size %d", base)}), nil
	}
	// Check holds a beat back when it contradicts what was witnessed, and its findings say why.
	if !holdsBeat(next, &p) {
		return held(prev, installID), found, nil
	}
	// One beat names one tree. The same beat pushed again over a larger tree is not a retry, and
	// adopting it would have the witness countersign two trees under one beat.
	if !first && p.BeatNumber == prev.LastBeat {
		return held(prev, installID), append(found, Finding{Kind: "duplicate_beat",
			Detail: fmt.Sprintf("beat %d was countersigned at tree size %d and is pushed again at "+
				"tree size %d, so one beat claims two trees", p.BeatNumber, prev.Size, p.Size),
			Key: fmt.Sprintf("duplicate_beat %d over tree size %d", p.BeatNumber, prev.Size)}), nil
	}
	next.InstallID, next.Size, next.Root = installID, p.Size, p.Root
	if first {
		found = append(found, Finding{Kind: "first_root", Detail: fmt.Sprintf(
			"this witness held no tree for install %s, so its memory starts here: the tree at size "+
				"%d with root %s is fixed from now on, and what the chain held before this push "+
				"was never seen by this witness", installID, p.Size, p.Root),
			Key: fmt.Sprintf("first_root at tree size %d", p.Size)})
	}
	return next, found, nil
}

// pushProblem reports why a push cannot be checked, or empty when it can. The beat and the tree are
// bounded the way a pulled beat is, and the head, the root, and every proof hash must be a sha256
// digest in lowercase hex: that is the one form the witness compares and countersigns, and a
// second spelling of one value would read as a different value.
func pushProblem(p *Push) string {
	switch {
	case len(p.Proof) > maxProofHashes:
		return fmt.Sprintf("its consistency proof carries %d hashes, more than the %d any tree "+
			"this witness accepts can need", len(p.Proof), maxProofHashes)
	case p.BeatNumber < 1, p.BeatNumber > maxBeatJump, p.Seq < 1, p.Seq > maxBeatJump:
		return fmt.Sprintf("beat %d at chain position %d is out of range", p.BeatNumber, p.Seq)
	case p.Size < 1, p.Size > maxBeatJump:
		return fmt.Sprintf("tree size %d is out of range", p.Size)
	case !hashHex.MatchString(p.Head):
		return fmt.Sprintf("its head %q is not a chain link in lowercase hex", clip(p.Head, 24))
	case !hashHex.MatchString(p.Root):
		return fmt.Sprintf("its tree root %q is not a digest in lowercase hex", clip(p.Root, 24))
	}
	for i, h := range p.Proof {
		if !hashHex.MatchString(h) {
			return fmt.Sprintf("proof hash %d of %d is not a digest in lowercase hex", i+1,
				len(p.Proof))
		}
	}
	return ""
}

// heldSize returns the tree size c holds, zero when it holds no tree.
func heldSize(c *Checkpoint) int64 {
	if c == nil || c.Root == "" {
		return 0
	}
	return c.Size
}

// holdsBeat reports whether c holds p's beat as the newest beat it has adopted.
func holdsBeat(c *Checkpoint, p *Push) bool {
	return c != nil && c.LastBeat == p.BeatNumber && c.LastSeq == p.Seq && c.LastHead == p.Head
}

// holdsPush reports whether c holds everything p claims: its beat as the newest, and its root at
// its size. A checkpoint that holds a push is one the witness countersigns that push from.
func holdsPush(c *Checkpoint, p *Push) bool {
	return holdsBeat(c, p) && c.Root != "" && c.Root == p.Root && c.Size == p.Size
}

// held returns a copy of the memory prev holds, the checkpoint a push that adopts nothing leaves
// standing. Before the first push that memory is empty and names only the install.
func held(prev *Checkpoint, installID string) *Checkpoint {
	if prev == nil {
		return &Checkpoint{Server: installID, InstallID: installID, Recent: map[int64]Observed{},
			Positions: map[int64]string{}}
	}
	c := *prev
	c.Recent = maps.Clone(prev.Recent)
	c.Positions = maps.Clone(prev.Positions)
	return &c
}

// hashBytes decodes one hex digest, or returns nil when it is not hex, which no proof verifies
// against.
func hashBytes(s string) []byte {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return raw
}

// hashList decodes a proof's hex hashes in order. A hash that is not hex decodes to nil, which the
// verifier refuses as a hash of the wrong length.
func hashList(in []string) [][]byte {
	out := make([][]byte, 0, len(in))
	for _, h := range in {
		out = append(out, hashBytes(h))
	}
	return out
}
