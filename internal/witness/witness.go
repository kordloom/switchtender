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

	"github.com/kordloom/switchtender/internal/audit"
)

// FeedLimit is how many beats the witness asks the feed for, and recentCap is how many it
// remembers. They are the same number on purpose: a beat the feed still serves but the witness has
// forgotten is re-adopted as if first seen, so a rewrite of it raises nothing. Remembering less
// than is served is the hole, not an economy.
const (
	FeedLimit = 1000
	recentCap = FeedLimit
)

// Beat is one span beat as the feed serves it.
type Beat struct {
	// Beat is the beat number, starting at one and rising by exactly one per beat.
	Beat int64 `json:"beat"`
	// At is when the beat was appended, RFC 3339.
	At string `json:"at"`
	// Seq is the beat entry's position in the audit chain.
	Seq int64 `json:"seq"`
	// Head is the beat entry's chain hash, the head the beat attests.
	Head string `json:"head"`
}

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
	// ObservedAt is when the checkpoint was taken.
	ObservedAt time.Time `json:"observed_at"`
	// PublicKey is the hex key that signed this checkpoint.
	PublicKey string `json:"public_key,omitempty"`
	// Sig is the hex signature over the canonical checkpoint content.
	Sig string `json:"sig,omitempty"`
}

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

// Check holds a fresh read of the feed against the previous checkpoint. It returns the next
// checkpoint and every finding, and it is pure, so what the witness alerts on is testable without
// a server. prev may be nil on the first watch.
func Check(prev *Checkpoint, server string, beats []Beat, now time.Time) (*Checkpoint, []Finding, error) {
	// Stored normalized, so a checkpoint written from one spelling matches a watch spelled the
	// other way without every reader having to remember to normalize.
	next := &Checkpoint{Server: NormalizeServer(server), Recent: map[int64]Observed{}, ObservedAt: now}
	var findings []Finding
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
	}

	// The feed numbers beats contiguously by construction, so a gap inside one answer means the
	// entries between were removed.
	for i := 1; i < len(beats); i++ {
		if beats[i].Beat != beats[i-1].Beat+1 {
			findings = append(findings, Finding{Kind: "missing_beat", Detail: fmt.Sprintf(
				"the feed jumps from beat %d to beat %d, so %d beat(s) between them are gone",
				beats[i-1].Beat, beats[i].Beat, beats[i].Beat-beats[i-1].Beat-1)})
		}
	}

	rewroteSomething := false
	for _, b := range beats {
		// First write wins: the witness's memory is its testimony, so a rewrite is reported on
		// every watch rather than adopted after one alert and attested away.
		seen, ok := next.Recent[b.Beat]
		if ok && (seen.Seq != b.Seq || seen.Head != b.Head) {
			findings = append(findings, Finding{Kind: "rewritten_beat", Detail: fmt.Sprintf(
				"beat %d was seq %d link %s when witnessed and is now seq %d link %s, so the "+
					"history under it was rewritten", b.Beat, seen.Seq, seen.Head, b.Seq, b.Head)})
			rewroteSomething = true
			continue
		}
		if !ok {
			next.Recent[b.Beat] = Observed{Seq: b.Seq, Head: b.Head}
		}
	}

	if len(beats) > 0 {
		newest := beats[len(beats)-1]
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
		case rewroteSomething:
			// The finding was already raised by the rewrite walk above; the memory stands.
		default:
			next.LastBeat, next.LastSeq, next.LastHead = newest.Beat, newest.Seq, newest.Head
		}
	} else if prev != nil && prev.LastBeat > 0 {
		findings = append(findings, Finding{Kind: "head_regression", Detail: fmt.Sprintf(
			"the feed is empty and beat %d was already witnessed, so the chain lost its tail",
			prev.LastBeat),
			Key: fmt.Sprintf("head_regression behind witnessed beat %d", prev.LastBeat)})
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
func signBytes(id audit.Identity, content []byte) (publicKey, sig string) {
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
func Sign(c *Checkpoint, id audit.Identity) error {
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
func Save(path string, c *Checkpoint, id audit.Identity) error {
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
