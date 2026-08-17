// Package witness watches a SwitchTender server's span beat feed from outside it. It keeps a
// signed checkpoint of what it has seen and raises a finding when a beat goes missing, an observed
// beat comes back rewritten, or the head regresses. A chain proves what it holds was not altered;
// it cannot prove nothing was removed from the end, because the same process decides what happens
// and what gets written down. An outside witness that remembers is what closes that.
package witness

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/beatfeed"
	"github.com/kordloom/switchtender/identity"
)

// FeedLimit is how many beats the witness asks the feed for, and recentCap is how many it
// remembers. They are the same number on purpose: a beat the feed still serves but the witness has
// forgotten is re-adopted as if first seen, so a rewrite of it raises nothing. Remembering less
// than is served is the hole, not an economy.
const (
	FeedLimit = 1000
	recentCap = FeedLimit
)

// Beat is one span beat as the feed serves it. It is the shared feed contract the server produces
// and the witness consumes, so the two cannot drift.
type Beat = beatfeed.Beat

// Observed is what the witness remembers of one beat.
type Observed struct {
	// Seq is the chain position the beat carried when first seen.
	Seq int64 `json:"seq"`
	// Head is the hash the beat carried when first seen.
	Head string `json:"head"`
}

// Checkpoint is the witness's signed memory of a server's beat stream.
type Checkpoint struct {
	// Server is the watched server's base URL.
	Server string `json:"server"`
	// LastBeat is the highest beat number observed.
	LastBeat int64 `json:"last_beat"`
	// LastSeq is the chain position of that beat.
	LastSeq int64 `json:"last_seq"`
	// LastHead is the hash of that beat.
	LastHead string `json:"last_head"`
	// Recent maps observed beat numbers to what they carried, bounded to the newest recentCap.
	Recent map[int64]Observed `json:"recent"`
	// Positions maps observed chain positions to the link seen at that position, bounded the same
	// way. The beat number is the watched server's own counter and it can start a replaced chain on
	// fresh numbers; the chain position is the coordinate it cannot renumber, so this is the memory
	// that catches a history replaced wholesale rather than edited in place.
	Positions map[int64]string `json:"positions,omitempty"`
	// ObservedAt is when the checkpoint was taken.
	ObservedAt time.Time `json:"observed_at"`
	// PublicKey is the hex key that signed this checkpoint.
	PublicKey string `json:"public_key,omitempty"`
	// Sig is the hex signature over the canonical checkpoint content.
	Sig string `json:"sig,omitempty"`
}

// StaleAfter is how long the newest beat may stand unchanged before the witness reports the feed as
// stopped. A span beat is emitted on a schedule rather than only on a change, so a day of no movement is
// far outside any plausible interval, while an hour is not: a server beating every thirty minutes that
// misses one beat to a restart is not a finding.
const StaleAfter = 24 * time.Hour

// Finding is one problem the witness can attest to.
type Finding struct {
	// Kind names the condition: missing_beat, rewritten_beat, or head_regression for what the
	// record shows, and witness_blind or witness_seeing for whether this witness can see at all.
	// A reader routing on it should treat unknown kinds as findings rather than drop them.
	Kind string `json:"kind"`
	// Detail says what was expected and what was seen, in words an operator can act on.
	Detail string `json:"detail"`
	// Key, when set, names the underlying event stably while Detail moves with each observation,
	// so a reporter can tell "the same truncation, observed again" from a new event. Empty means
	// Detail itself is stable and identifies the event.
	Key string `json:"-"`
}

// maxBeatJump bounds how far past the last witnessed beat a served beat may claim to be. A feed is
// allowed to run far ahead of a witness that was down, but a number vastly beyond the plausible is
// a value the watched server chose to wedge the checkpoint with.
const maxBeatJump = 1 << 40

// plausibleBeats drops the beats a witness cannot responsibly remember and returns a finding naming
// what was refused. A beat is implausible when its number or chain position is not positive, or its
// head is not a chain link's 64 hex characters. Refusing beats the witness cannot check keeps
// nonsense out of signed memory, where it would stand as testimony.
func plausibleBeats(beats []Beat) ([]Beat, []Finding) {
	kept := make([]Beat, 0, len(beats))
	refused := 0
	var why string
	for _, b := range beats {
		switch {
		case b.Beat < 1, b.Seq < 1, b.Beat > maxBeatJump:
			refused++
			if why == "" {
				why = fmt.Sprintf("beat %d at chain position %d is out of range", b.Beat, b.Seq)
			}
		case !plausibleHead(b.Head):
			refused++
			if why == "" {
				why = fmt.Sprintf("beat %d carries %q, which is not a chain link", b.Beat, clip(b.Head, 24))
			}
		default:
			kept = append(kept, b)
		}
	}
	if refused == 0 {
		return kept, nil
	}
	return kept, []Finding{{Kind: "malformed_feed", Detail: fmt.Sprintf(
		"the feed served %d beat(s) this witness refused to remember: %s", refused, why)}}
}

// plausibleHead reports whether s could be a chain link: present, and no longer than one. The
// charset is deliberately not policed here, because a head the witness cannot parse is still a head
// it can remember and compare, and comparison is what catches a rewrite.
func plausibleHead(s string) bool {
	return s != "" && len(s) <= maxHeadLen
}

// Check holds a fresh read of the feed against the previous checkpoint. It returns the next
// checkpoint and every finding, and it is pure, so what the witness alerts on is testable without
// a server. prev may be nil on the first watch.
func Check(prev *Checkpoint, server string, beats []Beat, now time.Time) (*Checkpoint, []Finding, error) {
	// Stored normalized, so a checkpoint written from one spelling matches a watch spelled the
	// other way without every reader having to remember to normalize.
	next := &Checkpoint{Server: NormalizeServer(server), Recent: map[int64]Observed{},
		Positions: map[int64]string{}, ObservedAt: now}
	var findings []Finding
	// rewound records that this poll saw history replaced, rewound, or rewritten. No head from such
	// a poll is adopted: every beat after a rewritten one is built on the replaced history, so
	// advancing onto one signs the forged chain into the witness's own testimony.
	rewound := false
	// A checkpoint is memory of one server's stream. Held against another server it invents
	// findings from the difference between two unrelated chains and overwrites the memory that
	// would have caught a real rewrite, so the mismatch is refused rather than reported.
	if prev != nil && prev.Server != "" && !sameServer(prev.Server, server) {
		return nil, nil, fmt.Errorf("checkpoint remembers %s, not %s; use one state file per server",
			prev.Server, server)
	}
	if prev != nil {
		next.LastBeat, next.LastSeq, next.LastHead = prev.LastBeat, prev.LastSeq, prev.LastHead
		for n, o := range prev.Recent {
			next.Recent[n] = o
		}
		for seq, link := range prev.Positions {
			next.Positions[seq] = link
		}
	}

	// A beat the witness cannot make sense of is refused rather than adopted into signed memory: an
	// unchecked number or head is a value the watched server chose, and adopting one wedges the
	// checkpoint at whatever it says while raising nothing.
	beats, malformed := plausibleBeats(beats)
	findings = append(findings, malformed...)

	// The feed numbers beats contiguously by construction, so a gap means the entries between were
	// removed. The walk is summarized rather than reported per gap: a hostile feed can serve a
	// thousand beats with a thousand gaps, and one finding per gap turns each poll into a thousand
	// records and a thousand notifications.
	gaps, missing := 0, int64(0)
	for i := 1; i < len(beats); i++ {
		if beats[i].Beat != beats[i-1].Beat+1 {
			// A repeat or a step backwards inside one answer is not a gap; the old arithmetic
			// reported it as a negative count of missing beats.
			if beats[i].Beat <= beats[i-1].Beat {
				findings = append(findings, Finding{Kind: "duplicate_beat", Detail: fmt.Sprintf(
					"the feed serves beat %d after beat %d, so it is not ordered and one of them is "+
						"a repeat", beats[i].Beat, beats[i-1].Beat)})
				continue
			}
			gaps++
			missing += beats[i].Beat - beats[i-1].Beat - 1
		}
	}
	if gaps > 0 {
		findings = append(findings, Finding{Kind: "missing_beat", Detail: fmt.Sprintf(
			"the feed skips %d beat(s) across %d gap(s) between beat %d and beat %d, so entries "+
				"between them are gone", missing, gaps, beats[0].Beat, beats[len(beats)-1].Beat)})
	}

	// A gap ACROSS polls was invisible: the walk above only compares beats inside one answer, so a
	// feed that jumped from the witnessed beat to a much later one was adopted in silence. The
	// witness remembers where it stopped, so it is the one party that can see that gap.
	if prev != nil && prev.LastBeat > 0 && len(beats) > 0 && beats[0].Beat > prev.LastBeat+1 {
		findings = append(findings, Finding{Kind: "missing_beat", Detail: fmt.Sprintf(
			"the oldest beat served is %d and beat %d was already witnessed, so %d beat(s) between "+
				"them never appeared in any answer", beats[0].Beat, prev.LastBeat,
			beats[0].Beat-prev.LastBeat-1),
			Key: fmt.Sprintf("missing_beat after witnessed beat %d", prev.LastBeat)})
	}



	for _, b := range beats {
		// First write wins: the witness's memory is its testimony, so a rewrite is reported on
		// every watch rather than adopted after one alert and attested away.
		seen, ok := next.Recent[b.Beat]
		if ok && (seen.Seq != b.Seq || seen.Head != b.Head) {
			findings = append(findings, Finding{Kind: "rewritten_beat", Detail: fmt.Sprintf(
				"beat %d was seq %d link %s when witnessed and is now seq %d link %s, so the "+
					"history under it was rewritten", b.Beat, seen.Seq, seen.Head, b.Seq, b.Head)})
			rewound = true
			continue
		}
		if !ok {
			next.Recent[b.Beat] = Observed{Seq: b.Seq, Head: b.Head}
		}
		// The same comparison in coordinate space. A chain replaced wholesale and served under
		// fresh beat numbers presents no beat this witness remembers, so the walk above adopts it
		// in silence; the position it claims is one the witness has already seen carrying a
		// different link, and that is the same history contradicting itself.
		if link, seen := next.Positions[b.Seq]; seen {
			if link != b.Head {
				findings = append(findings, Finding{Kind: "rewritten_history", Detail: fmt.Sprintf(
					"chain position %d was link %s when witnessed and beat %d now reports link %s, "+
						"so the history at that position was replaced", b.Seq, link, b.Beat, b.Head),
					Key: fmt.Sprintf("rewritten_history at chain position %d", b.Seq)})
				rewound = true
			}
			continue
		}
		next.Positions[b.Seq] = b.Head
	}

	if len(beats) > 0 {
		newest := beats[len(beats)-1]
		// The chain position is the coordinate the watched party cannot renumber. Rewrite detection
		// keyed on the beat number alone, so replacing the whole chain and serving it under fresh
		// beat numbers raised nothing and was attested clean: every number was new, so
		// first-write-wins adopted it. A chain only appends, so its newest position never moves
		// back, whatever the beats are called. A beat number that also went backwards is the same
		// event seen from the other side, and head_regression below already names it.
		if prev != nil && prev.LastSeq > 0 && newest.Seq < prev.LastSeq && newest.Beat >= next.LastBeat {
			findings = append(findings, Finding{Kind: "seq_regression", Detail: fmt.Sprintf(
				"beat %d reports chain position %d and position %d was already witnessed, so the "+
					"chain is shorter than it was: its history was replaced or rewound while the "+
					"beat numbering kept climbing", newest.Beat, newest.Seq, prev.LastSeq),
				Key: fmt.Sprintf("seq_regression behind witnessed seq %d", prev.LastSeq)})
			rewound = true
		}
		// No head from a watch that saw a rewrite is adopted, wherever the rewrite landed. Every
		// beat after a rewritten one is built on the rewritten history, so advancing onto a beat
		// that merely looks new signs the forged chain into the witness's own testimony just as
		// surely as adopting the rewritten beat itself would.
		switch {
		case newest.Beat < next.LastBeat:
			// The key anchors on the witnessed beat, which does not move while the feed's newest
			// climbs back toward it, so one truncation stays one event however long it stands.
			findings = append(findings, Finding{Kind: "head_regression", Detail: fmt.Sprintf(
				"the newest beat is %d and beat %d was already witnessed, so the chain lost its "+
					"tail", newest.Beat, next.LastBeat),
				Key: fmt.Sprintf("head_regression behind witnessed beat %d", next.LastBeat)})
		case rewound:
			// The finding was already raised by the rewrite walk above; the memory stands.
		default:
			next.LastBeat, next.LastSeq, next.LastHead = newest.Beat, newest.Seq, newest.Head
		}
	} else if prev != nil && prev.LastBeat > 0 {
		findings = append(findings, Finding{Kind: "head_regression", Detail: fmt.Sprintf(
			"the feed is empty and beat %d was already witnessed, so the chain lost its tail",
			prev.LastBeat),
			Key: fmt.Sprintf("head_regression behind witnessed beat %d", prev.LastBeat)})
	} else {
		// A witness with nothing to witness is not a witness, and it must not sign a checkpoint that
		// reads as one. An empty feed with no memory behind it used to produce no findings at all: the
		// checkpoint saved, the command exited zero, and a nightly cron reported success every night
		// while covering nothing. The usual cause is a server whose span beat was never turned on,
		// which is a configuration to fix rather than an attack, so the finding says so.
		findings = append(findings, Finding{Kind: "empty_feed", Detail: "this server served no beats " +
			"at all, so there is nothing to witness: start the span beat on it, with serve " +
			"--beat-interval, or point the witness at a server that emits beats",
			Key: "empty_feed"})
	}

	// A feed that stopped moving reads exactly like a healthy quiet one, and that is the state an
	// operator who stopped recording wants to be in. A span beat is emitted on a schedule whether or
	// not the chain changed, so a newest beat that has not advanced for longer than any plausible beat
	// interval means the beats stopped, the server is replaying a fixed answer, or the chain is frozen.
	// The witness cannot tell which from here, and does not guess: it reports that the feed has not
	// moved and for how long.
	if prev != nil && prev.LastBeat > 0 && next.LastBeat == prev.LastBeat &&
		next.LastSeq == prev.LastSeq && next.LastHead == prev.LastHead &&
		!prev.ObservedAt.IsZero() && now.Sub(prev.ObservedAt) > StaleAfter {
		findings = append(findings, Finding{Kind: "stalled_feed", Detail: fmt.Sprintf(
			"beat %d at chain position %d has been the newest since %s, %s ago, so the feed has "+
				"stopped moving: either the beats stopped, the chain stopped appending, or this "+
				"answer is a replay", next.LastBeat, next.LastSeq,
			prev.ObservedAt.UTC().Format(time.RFC3339), now.Sub(prev.ObservedAt).Round(time.Minute)),
			Key: fmt.Sprintf("stalled_feed at beat %d", next.LastBeat)})
		// The witnessed time is not advanced past the observation that first stood still, so the
		// finding keeps naming when the feed actually stopped rather than resetting each poll.
		next.ObservedAt = prev.ObservedAt
	}

	// Forget the oldest remembered positions past the cap, never the newest.
	if len(next.Positions) > recentCap {
		seqs := make([]int64, 0, len(next.Positions))
		for seq := range next.Positions {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs[:len(next.Positions)-recentCap] {
			delete(next.Positions, seq)
		}
	}

	// Forget the oldest remembered beats past the cap, never the newest.
	if len(next.Recent) > recentCap {
		nums := make([]int64, 0, len(next.Recent))
		for n := range next.Recent {
			nums = append(nums, n)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		for _, n := range nums[:len(nums)-recentCap] {
			delete(next.Recent, n)
		}
	}
	return next, findings, nil
}

// sameServer reports whether two spellings address the same server. The witness normalizes the
// base URL before it fetches, so a checkpoint written from one spelling must not refuse a watch
// spelled with a trailing slash: refusing means no beat is ever compared again, which blinds the
// witness permanently while it reports itself healthy.
func sameServer(a, b string) bool {
	return NormalizeServer(a) == NormalizeServer(b)
}

// NormalizeServer returns the base URL in the one form the witness watches and records, so the
// checkpoint, the pin, and the feed request all agree on what "the same server" means. The scheme
// and host are lowercased because they are case-insensitive on the wire: two case-variant
// spellings of one server must not become two memories, each blind to what the other witnessed.
func NormalizeServer(base string) string {
	trimmed := strings.TrimRight(base, "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimmed
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// signedContent is the canonical bytes a checkpoint signature covers: the checkpoint with its
// signature fields cleared.
func signedContent(c *Checkpoint) ([]byte, error) {
	cp := *c
	cp.PublicKey, cp.Sig = "", ""
	return json.Marshal(&cp)
}

// signBytes signs content with the witness identity, returning the hex key and signature. Every
// document the witness signs, its checkpoints and its attestations, goes through this one
// envelope, so there is a single answer to what a witness signature means.
func signBytes(id identity.Identity, content []byte) (publicKey, sig string) {
	return id.PublicKeyHex(), hex.EncodeToString(ed25519.Sign(id.Private(), content))
}

// verifyBytes checks an envelope signature over content. subject names the document in errors.
func verifyBytes(subject, publicKey, sig string, content []byte) error {
	if sig == "" || publicKey == "" {
		return fmt.Errorf("%s is unsigned", subject)
	}
	pub, err := hex.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%s public key is malformed", subject)
	}
	raw, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%s signature is malformed", subject)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), content, raw) {
		return fmt.Errorf("%s signature does not verify; the document was altered", subject)
	}
	return nil
}

// Sign stamps the checkpoint with the witness identity, so a tampered state file is detectable.
func Sign(c *Checkpoint, id identity.Identity) error {
	content, err := signedContent(c)
	if err != nil {
		return fmt.Errorf("sign checkpoint: %w", err)
	}
	c.PublicKey, c.Sig = signBytes(id, content)
	return nil
}

// Verify checks the checkpoint's signature. It reports the signer's hex key so a caller can pin
// it across restarts.
func Verify(c *Checkpoint) (publicKey string, err error) {
	content, err := signedContent(c)
	if err != nil {
		return "", fmt.Errorf("verify checkpoint: %w", err)
	}
	if err := verifyBytes("checkpoint", c.PublicKey, c.Sig, content); err != nil {
		return "", err
	}
	return c.PublicKey, nil
}

// Load reads and verifies a checkpoint file, pinning its signer to expectKey when one is given. A
// missing file returns nil with no error, the first watch. A present file that fails verification
// or was signed by another key is an error, never silently accepted.
func Load(path, expectKey string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	var c Checkpoint
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse checkpoint: %w", err)
	}
	signer, err := Verify(&c)
	if err != nil {
		return nil, err
	}
	// The signature alone proves only that the file is self-consistent, which a forger who
	// generates their own key satisfies trivially. Pinning the signer to this witness's own key is
	// what makes a replaced state file detectable.
	if expectKey != "" && signer != expectKey {
		return nil, fmt.Errorf("checkpoint was signed by %s, not by this witness (%s); "+
			"the state file was replaced", signer, expectKey)
	}
	return &c, nil
}

// Save signs and writes the checkpoint atomically, so a crash mid-write never leaves a state file
// that fails verification on the next start.
func Save(path string, c *Checkpoint, id identity.Identity) error {
	if err := Sign(c, id); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	return nil
}

// DefaultStatePath returns where the checkpoint lives when the operator does not say: beside the
// working directory, named for the witness.
func DefaultStatePath() string {
	return filepath.Join(".", "switchtender-witness.json")
}
