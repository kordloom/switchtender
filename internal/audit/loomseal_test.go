package audit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/audit"
)

// buildChain returns a linked chain whose entries include the characters where a naive JSON encoder
// disagrees with canonical JSON, so a bundle built from it only verifies if the link construction is
// the one the format specifies.
func bundleChain(t *testing.T) []*audit.Entry {
	t.Helper()
	at := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
	paths := []string{
		"/v1/templates",
		"/v1/projects/a&b",
		"/v1/p/<script>",
		"/v1/runs/plain",
	}
	var chain []*audit.Entry
	var prev *audit.Entry
	for i, p := range paths {
		e := &audit.Entry{
			ID:     "ae_" + p[len(p)-1:],
			At:     at.Add(time.Duration(i) * time.Minute),
			Actor:  "admin",
			Method: "POST",
			Path:   p,
		}
		audit.Link(prev, e)
		chain = append(chain, e)
		prev = e
	}
	return chain
}

// TestBundleRoundTrip builds, signs, and re-verifies a bundle with the same chain checks a third
// party would run: every link recomputes from the claim payloads alone, and the head matches.
func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	chain := bundleChain(t)

	b, err := audit.BuildBundle(chain, id, "1.34.1", time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("signed bundle is not JSON: %v", err)
	}
	if doc["loomseal"] != "0.1" {
		t.Errorf("loomseal = %v, want 0.1", doc["loomseal"])
	}
	sigs, _ := doc["signatures"].([]any)
	if len(sigs) != 1 {
		t.Fatalf("signatures = %d, want 1", len(sigs))
	}
	sig, _ := sigs[0].(map[string]any)
	producer, _ := doc["producer"].(map[string]any)
	if sig["key_id"] != producer["key_id"] {
		t.Errorf("signature key_id %v does not match producer key_id %v", sig["key_id"], producer["key_id"])
	}
	if sig["alg"] != "ed25519" {
		t.Errorf("alg = %v, want ed25519", sig["alg"])
	}

	chainMember, _ := doc["chain"].(map[string]any)
	if chainMember["profile"] != audit.ChainProfile {
		t.Errorf("profile = %v, want %s", chainMember["profile"], audit.ChainProfile)
	}
	// The published schema requires keyed to be present rather than inferred from the profile, and
	// constrains subject.type to a fixed vocabulary. The reference verifier does not enforce the
	// schema, so a bundle can verify there and still be rejected by a verifier that does. Both of
	// these were wrong on the first attempt and only the schema caught them.
	if _, ok := chainMember["keyed"]; !ok {
		t.Error("chain.keyed is absent, the schema requires it even for an unkeyed profile")
	}
	if chainMember["keyed"] != false {
		t.Errorf("chain.keyed = %v, want false for an unkeyed profile", chainMember["keyed"])
	}
	subject, _ := doc["subject"].(map[string]any)
	switch subject["type"] {
	case "url", "fleet", "repo":
	default:
		t.Errorf("subject.type = %v, want one of the schema's url, fleet, or repo", subject["type"])
	}

	// Every claim's link must recompute from its own payload, which is what makes the profile
	// verifiable by someone holding nothing but the bundle.
	claims, _ := doc["claims"].([]any)
	if len(claims) != len(chain) {
		t.Fatalf("claims = %d, want %d", len(claims), len(chain))
	}
	for i, raw := range claims {
		c, _ := raw.(map[string]any)
		cc, _ := c["chain"].(map[string]any)
		p, _ := c["payload"].(map[string]any)
		at, _ := time.Parse(time.RFC3339Nano, c["at"].(string))
		recomputed := audit.EntryHash(&audit.Entry{
			Seq:      int64(cc["seq"].(float64)),
			At:       at,
			Actor:    p["actor"].(string),
			Method:   p["method"].(string),
			Path:     p["path"].(string),
			PrevHash: cc["prev"].(string),
		})
		if recomputed != cc["link"] {
			t.Errorf("claim %d link does not recompute: got %s, want %v", i, recomputed, cc["link"])
		}
	}

	// The identity persisted, so a second load signs as the same producer rather than a new one.
	again, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() second call error = %v", err)
	}
	if again.KeyID() != id.KeyID() {
		t.Errorf("key changed across loads: %s then %s", id.KeyID(), again.KeyID())
	}
	info, err := os.Stat(filepath.Join(dir, audit.IdentityFile))
	if err != nil {
		t.Fatalf("Stat(identity) error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("identity file mode = %o, want 600, the signing seed must not be world readable", perm)
	}
}

// TestBundleMapsSpanBeats pins the span claim mapping: a well-formed span entry becomes the
// spec-owned span claim with a numeric payload, its chain coordinates stay exactly as stored, and
// everything else, including a span-marked entry whose path does not parse, stays a generic claim.
func TestBundleMapsSpanBeats(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	ctx := context.Background()
	store := audit.NewMemStore()
	base := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
	for i, path := range []string{"/v1/templates", "/v1/projects"} {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "admin", Method: "POST", Path: path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	if _, err := store.AppendSpanBeat(ctx, base.Add(time.Hour), 60); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	// A span-marked entry whose path does not round-trip, which must stay a generic claim rather
	// than become a malformed span one.
	if err := store.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base.Add(2 * time.Hour),
		Actor: audit.SpanActor, Method: audit.SpanMethod, Path: "/span/oops",
	}); err != nil {
		t.Fatalf("Append() near-miss error = %v", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}

	b, err := audit.BuildBundle(chain, id, "test", base.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	if len(b.Claims) != 4 {
		t.Fatalf("claims = %d, want 4", len(b.Claims))
	}

	span := b.Claims[2]
	if span.Type != audit.SpanClaimType {
		t.Errorf("span claim type = %q, want %q", span.Type, audit.SpanClaimType)
	}
	// The payload numbers must be numbers, not strings, per the span claim registry entry. The
	// actor, method, and path members stay alongside them because the chain link commits to those
	// three, and a verifier recomputes every link from the claim payload alone.
	wantPayload := map[string]any{
		"stream": "chain", "cadence_s": int64(60), "beat": int64(1), "count": int64(2),
		"actor": audit.SpanActor, "method": audit.SpanMethod, "path": audit.SpanPath(1, 2, 60),
	}
	if diff := cmp.Diff(wantPayload, span.Payload); diff != "" {
		t.Errorf("span payload mismatch (-want +got):\n%s", diff)
	}
	if span.Chain.Seq != chain[2].Seq || span.Chain.Prev != chain[2].PrevHash ||
		span.Chain.Link != chain[2].Hash {
		t.Errorf("span claim chain = %+v, want the stored coordinates of entry 3", span.Chain)
	}

	// The generic entries and the near-miss keep the generic type and the actor, method, and path
	// payload the chain link commits to.
	for _, i := range []int{0, 1, 3} {
		claim, e := b.Claims[i], chain[i]
		if claim.Type != audit.ClaimType {
			t.Errorf("claim %d type = %q, want %q", i, claim.Type, audit.ClaimType)
		}
		want := map[string]any{"actor": e.Actor, "method": e.Method, "path": e.Path}
		if diff := cmp.Diff(want, claim.Payload); diff != "" {
			t.Errorf("claim %d payload mismatch (-want +got):\n%s", i, diff)
		}
	}
}

// TestBundleSpanClaimLinksRecompute pins that every claim link in a bundled chain holding a span
// beat recomputes from the bundle document alone, the way the reference verifier does: SHA-256
// over the canonical JSON array of the sequence, the at bytes exactly as bundled, the payload's
// actor, method, and path members, and the previous link. The span mapping used to replace the
// payload, dropping the three members the link commits to, so the verifier hashed empty strings
// and every bundle holding a span beat failed chain verification at the auditor.
func TestBundleSpanClaimLinksRecompute(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	ctx := context.Background()
	store := audit.NewMemStore()
	base := time.Date(2026, 8, 3, 4, 32, 10, 435251000, time.UTC)
	for i := range 3 {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Second),
			Actor: "cli:operator", Method: "CLI", Path: "/cli/user/create",
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	if _, err := store.AppendSpanBeat(ctx, base.Add(time.Minute), 1); err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	b, err := audit.BuildBundle(chain, id, "test", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// The decoding below mirrors the verifier: it reads only the bundle bytes, and a payload
	// member the bundle does not carry is an empty string, never recovered from the store.
	var doc struct {
		Claims []struct {
			At      string `json:"at"`
			Payload struct {
				Actor  string `json:"actor"`
				Method string `json:"method"`
				Path   string `json:"path"`
			} `json:"payload"`
			Chain struct {
				Seq  int64  `json:"seq"`
				Prev string `json:"prev"`
				Link string `json:"link"`
			} `json:"chain"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(doc.Claims) != len(chain) {
		t.Fatalf("claims = %d, want %d", len(doc.Claims), len(chain))
	}
	for i, c := range doc.Claims {
		payload, err := jcs.Serialize([]any{
			strconv.FormatInt(c.Chain.Seq, 10), c.At,
			c.Payload.Actor, c.Payload.Method, c.Payload.Path, c.Chain.Prev,
		})
		if err != nil {
			t.Fatalf("claim %d: jcs.Serialize() error = %v", i, err)
		}
		sum := sha256.Sum256(payload)
		if diff := cmp.Diff(c.Chain.Link, hex.EncodeToString(sum[:])); diff != "" {
			t.Errorf("claim %d link does not recompute from the bundle alone (-want +got):\n%s",
				i, diff)
		}
	}
}

// TestBundleRefusesUnverifiableEntry covers an entry recorded before times were truncated to
// microseconds. Its link cannot be recomputed by a verifier that carries time at microsecond
// precision, and the entry cannot be repaired because its own hash commits to the time it holds.
// Emitting it anyway would produce a bundle that verifies here and fails at the auditor.
func TestBundleRefusesUnverifiableEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	// A time carrying nanoseconds, as an older build on Linux would have recorded.
	e := &audit.Entry{
		At:     time.Date(2026, 7, 27, 8, 0, 14, 140904281, time.UTC),
		Actor:  "admin",
		Method: "POST",
		Path:   "/v1/templates",
		Seq:    1,
	}
	e.Hash = audit.EntryHash(e)
	if _, err := audit.BuildBundle([]*audit.Entry{e}, id, "test", time.Now()); err == nil {
		t.Fatal("BuildBundle accepted an entry no third party can verify")
	}
}

// TestIdentityConcurrentFirstStart covers several processes starting against a fresh state directory
// at once. Each wrote the same fixed temporary file and renamed over the others, so they handed out
// different in-memory keys and none matched what was finally on disk. An install has one identity;
// two would mean two producers claiming to be the same install.
func TestIdentityConcurrentFirstStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	const racers = 12
	keys := make([]string, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := audit.LoadIdentity(dir)
			if err != nil {
				t.Errorf("LoadIdentity() error = %v", err)
				return
			}
			keys[i] = id.KeyID()
		}()
	}
	close(start)
	wg.Wait()

	onDisk, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() after the race error = %v", err)
	}
	for i, k := range keys {
		if k != onDisk.KeyID() {
			t.Fatalf("racer %d got key %s, but the install's key is %s: two producers would claim "+
				"to be the same install", i, k, onDisk.KeyID())
		}
	}
}

// TestIdentityEnvKeyDerivesItsOwnInstallID covers an operator setting the documented environment
// key over an install that already has a file. The id used to come from the file while the key came
// from the environment, so bundles were signed by one key and attributed to another install.
func TestIdentityEnvKeyDerivesItsOwnInstallID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	fromFile, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	// A different key, supplied the documented way.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", strings.Repeat("ab", 32))
	fromEnv, err := audit.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity() with the environment key error = %v", err)
	}
	if fromEnv.KeyID() == fromFile.KeyID() {
		t.Fatal("the environment key was ignored")
	}
	if fromEnv.InstallID == fromFile.InstallID {
		t.Error("the environment key kept the file's install id, so a bundle would be signed by " +
			"one key and attributed to another install")
	}
}

// TestBuildBundleRefusesAnUnsoundChain pins that a bundle is refused when the entries it would
// claim do not actually verify.
//
// Contiguity was checked arithmetically, by counting sequence numbers up from the first entry.
// That says nothing about the links: a reordered chain whose prev no longer names the entry before
// it, an entry whose content was edited after it was hashed, and a genesis claim carrying a
// previous link all counted up correctly and all built a bundle. Every one of them is rejected by
// the reference verifier, so the failure landed on the auditor holding the bundle rather than on
// the install producing it.
func TestBuildBundleRefusesAnUnsoundChain(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	tests := []struct {
		Name    string
		Damage  func(entries []*audit.Entry)
		WantErr bool
	}{{ // Test 0: An intact chain still builds, so the check is not simply refusing everything.
		Name: "intact", Damage: func([]*audit.Entry) {}, WantErr: false,
	}, { // Test 1: A link that no longer names the entry before it.
		Name: "broken link", WantErr: true,
		Damage: func(e []*audit.Entry) { e[2].PrevHash = e[0].PrevHash },
	}, { // Test 2: Content edited after the entry was hashed.
		Name: "edited content", WantErr: true,
		Damage: func(e []*audit.Entry) { e[2].Path = "/v1/credentials/rotated" },
	}, { // Test 3: A genesis claim carrying a previous link, which the reference verifier rejects.
		Name: "genesis with a prev", WantErr: true,
		Damage: func(e []*audit.Entry) {
			e[0].PrevHash = "00"
			e[0].Hash = audit.EntryHash(e[0])
		},
	}, { // Test 4: Two entries swapped, which keeps every sequence number present.
		Name: "reordered", WantErr: true,
		Damage: func(e []*audit.Entry) { e[1], e[2] = e[2], e[1] },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entries := buildChain(4)
			test.Damage(entries)
			_, err := audit.BuildBundle(entries, id, "v1.0.0", time.Now())
			if test.WantErr && err == nil {
				t.Error("a bundle was built from a chain that does not verify, so it would be " +
					"rejected by the auditor it was handed to")
			}
			if !test.WantErr && err != nil {
				t.Errorf("BuildBundle() on an intact chain error = %v", err)
			}
		})
	}
}

// TestBuildBundleRefusesNonAdvancingSpanBeats covers a chain minted before beat times were clamped,
// on a machine whose clock stepped backward between two beats.
//
// A verifier fails a bundle outright when a beat's time does not strictly advance past the beat
// before it, where a cadence gap is only reported. A link commits to the time its entry holds, so
// neither beat of such a pair can be repaired and every bundle covering both fails forever. It is
// refused at build time for the same reason a nanosecond entry is: a bundle that builds here and
// fails at the auditor has already been handed over by the time anyone finds out.
func TestBuildBundleRefusesNonAdvancingSpanBeats(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	tests := []struct {
		Name    string
		Steps   []time.Duration
		WantErr bool
	}{{ // Test 0: Beats a minute apart build, so the check is not refusing every beat chain.
		Name: "advancing", Steps: []time.Duration{0, time.Minute, 2 * time.Minute}, WantErr: false,
	}, { // Test 1: The clock stepped backward, so the third beat precedes the second.
		Name: "backward", Steps: []time.Duration{0, 2 * time.Minute, time.Minute}, WantErr: true,
	}, { // Test 2: Equal times do not advance either, which is what the verifier tests for.
		Name: "equal", Steps: []time.Duration{0, time.Minute, time.Minute}, WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			base := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
			entries := make([]*audit.Entry, 0, len(test.Steps))
			var prev *audit.Entry
			for i, step := range test.Steps {
				e := audit.NewSpanEntry(base.Add(step), int64(i+1), 0, 60)
				audit.Link(prev, e)
				entries = append(entries, e)
				prev = e
			}
			// The chain itself is sound; only the beat times are wrong, so the refusal under test is
			// the one being reached rather than the range check ahead of it.
			if ok, brokeAt := audit.Verify(entries); !ok {
				t.Fatalf("the test chain does not verify at %d", brokeAt)
			}
			_, err := audit.BuildBundle(entries, id, "v1.0.0", time.Now())
			if test.WantErr {
				if !errors.Is(err, audit.ErrExport) {
					t.Errorf("BuildBundle() error = %v, want ErrExport: a bundle whose beats do not "+
						"advance is rejected by every verifier and cannot be repaired", err)
				}
				return
			}
			if err != nil {
				t.Errorf("BuildBundle() on advancing beats error = %v", err)
			}
		})
	}
}

// TestBuildBundleAcceptsAPartialChain pins that bundling part of a chain still works, since the
// range check must not require the slice to start at genesis. A bundle built with --limit covers
// the most recent entries, not the whole history.
func TestBuildBundleAcceptsAPartialChain(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	entries := buildChain(6)
	if _, err := audit.BuildBundle(entries[3:], id, "v1.0.0", time.Now()); err != nil {
		t.Errorf("BuildBundle() on the tail of a chain error = %v, want a bundle", err)
	}
}
