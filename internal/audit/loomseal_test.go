package audit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
