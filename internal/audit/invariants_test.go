package audit_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// These are the properties a receipt is sold on. Each one is stated here as a thing that must hold
// for any chain, rather than as a case somebody thought to write down.
//
// The defect that motivated the file: an approval recorded twenty minutes after the run it released
// verified clean, because the rule that time must advance existed and had been applied to exactly
// one entry type. The rule was understood. Its scope was one case. That is the shape to guard
// against, so these tests assert the invariant across every entry kind and every position in a
// chain rather than on a single hand-built example.

// chainOf builds a signed bundle over entries and returns its report.
func chainOf(t *testing.T, entries []*audit.Entry) (*audit.BundleReport, []byte, audit.Identity) {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	b, err := audit.BuildBundle(entries, id, "v-test", time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	return rep, signed, id
}

// linked assigns sequence and links across a run of entries.
func linked(entries []*audit.Entry) []*audit.Entry {
	var prev *audit.Entry
	for _, e := range entries {
		audit.Link(prev, e)
		prev = e
	}
	return entries
}

// TestEveryFieldTheChainShowsIsAlsoCommitted is the invariant a receipt exists for. A field a
// verifier displays but the chain does not fix is a field anyone holding the receipt can rewrite,
// which makes it worth no more than an assertion on a web page.
//
// This walks every string field of every claim, changes it, and requires verification to fail. A
// field that survives being rewritten is a field the receipt should not be showing.
func TestEveryFieldTheChainShowsIsAlsoCommitted(t *testing.T) {
	// Not parallel: chainOf sets an environment variable to isolate the signing identity.
	at := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC)
	entries := linked([]*audit.Entry{
		{ID: audit.NewID(), At: at, Actor: "deploy-bot", ActorType: "token",
			Method: "POST", Path: "/v1/runs"},
		{ID: audit.NewID(), At: at.Add(time.Second), Actor: "admin", ActorType: "user",
			Method: audit.MethodDecision, Path: "/runs/r1/decision/approved"},
		{ID: audit.NewID(), At: at.Add(2 * time.Second), Actor: "system:dispatcher",
			ActorType: "system", Method: audit.MethodRun, Path: "/runs/r1/outcome/succeeded"},
	})
	_, signed, id := chainOf(t, entries)

	var doc map[string]any
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("the signed bundle is not JSON: %v", err)
	}
	claims, _ := doc["claims"].([]any)
	if len(claims) == 0 {
		t.Fatal("the bundle carries no claims")
	}

	tampered := 0
	for ci := range claims {
		claim, _ := claims[ci].(map[string]any)
		payload, _ := claim["payload"].(map[string]any)
		for field, val := range payload {
			s, ok := val.(string)
			if !ok || s == "" {
				continue
			}
			t.Run(fmt.Sprintf("claim_%d_%s", ci, field), func(t *testing.T) {
				payload[field] = s + "-rewritten"
				forged, err := json.Marshal(doc)
				payload[field] = s // restore before any assertion can abort the loop
				if err != nil {
					t.Fatalf("Marshal() error = %v", err)
				}
				rep, err := audit.VerifyBundle(forged, id.KeyID())
				if err != nil {
					return // refusing to parse a forgery is a pass
				}
				if rep.OK() {
					t.Errorf("rewriting claim %d field %q left the receipt verifying, so the chain "+
						"does not commit to a field the receipt shows", ci, field)
				}
			})
			tampered++
		}
	}
	if tampered < 6 {
		t.Errorf("only %d fields were exercised; the fixture no longer covers the payload shape", tampered)
	}
}

// TestApprovalsPrecedeRunsAtEveryPosition generalizes the demo defect. The original had the
// approval second of three. A rule applied to one arrangement is the bug that produced this file, so
// the property is asserted wherever the pair sits in the chain and however far apart they are.
func TestApprovalsPrecedeRunsAtEveryPosition(t *testing.T) {
	// Not parallel: chainOf sets an environment variable to isolate the signing identity.
	base := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC)
	filler := func(n int, at time.Time) []*audit.Entry {
		var out []*audit.Entry
		for i := range n {
			out = append(out, &audit.Entry{
				ID: audit.NewID(), At: at.Add(time.Duration(i) * time.Second),
				Actor: "admin", ActorType: "user", Method: "POST",
				Path: fmt.Sprintf("/v1/templates/%d", i),
			})
		}
		return out
	}
	for _, lead := range []int{0, 1, 5} {
		for _, gap := range []time.Duration{time.Second, time.Minute, 20 * time.Minute} {
			name := fmt.Sprintf("lead_%d_gap_%s", lead, gap)
			t.Run(name, func(t *testing.T) {
				start := base.Add(time.Duration(lead) * time.Second)
				// The run executes, then the approval lands after it. Always wrong.
				es := append(filler(lead, base), []*audit.Entry{
					{ID: audit.NewID(), At: start.Add(time.Second), Actor: "system:dispatcher",
						ActorType: "system", Method: audit.MethodRun,
						Path: "/runs/r/outcome/succeeded"},
					{ID: audit.NewID(), At: start.Add(time.Second + gap), Actor: "admin",
						ActorType: "user", Method: audit.MethodDecision,
						Path: "/runs/r/decision/approved"},
				}...)
				rep, _, _ := chainOf(t, linked(es))
				if rep.ApprovalPrecedesRun {
					t.Errorf("an approval %s after the run it released was accepted", gap)
				}
				if rep.OK() {
					t.Errorf("the receipt verified with the gate bypassed; problems: %v", rep.TimeProblems)
				}
			})
		}
	}
}

// TestAGenuineChainVerifiesAtEveryLength is the control. A guard that fails everything is worthless,
// so the same construction must verify clean across chain lengths and entry mixes.
func TestAGenuineChainVerifiesAtEveryLength(t *testing.T) {
	// Not parallel: chainOf sets an environment variable to isolate the signing identity.
	base := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 2, 3, 8, 40} {
		t.Run(fmt.Sprintf("entries_%d", n), func(t *testing.T) {
			var es []*audit.Entry
			for i := range n {
				at := base.Add(time.Duration(i) * time.Second)
				e := &audit.Entry{ID: audit.NewID(), At: at, Actor: "admin", ActorType: "user"}
				switch i % 3 {
				case 0:
					e.Method, e.Path = "POST", fmt.Sprintf("/v1/runs/%d", i)
				case 1:
					e.Method, e.Path = audit.MethodDecision, fmt.Sprintf("/runs/r%d/decision/approved", i)
				default:
					e.Method, e.Path = audit.MethodRun, fmt.Sprintf("/runs/r%d/outcome/succeeded", i-1)
				}
				es = append(es, e)
			}
			rep, _, _ := chainOf(t, linked(es))
			if !rep.OK() {
				t.Errorf("a genuine %d-entry chain failed: chain=%v sig=%v anchors=%v spec=%v order=%v %v",
					n, rep.ChainOK, rep.SignatureOK, rep.AnchorsOK, rep.SpecConsistent,
					rep.ApprovalPrecedesRun, rep.TimeProblems)
			}
			if len(rep.TimeProblems) > 0 {
				t.Errorf("a genuine chain reported time problems: %v", rep.TimeProblems)
			}
		})
	}
}

// TestDroppingAnyClaimBreaksTheChain covers truncation and excision. A holder who can remove the
// entry that embarrasses them, and still hand over something that verifies, has a receipt that
// proves nothing about what is missing.
func TestDroppingAnyClaimBreaksTheChain(t *testing.T) {
	// Not parallel: chainOf sets an environment variable to isolate the signing identity.
	base := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC)
	var es []*audit.Entry
	for i := range 5 {
		es = append(es, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Second),
			Actor: "admin", ActorType: "user", Method: "POST",
			Path: fmt.Sprintf("/v1/runs/%d", i),
		})
	}
	_, signed, id := chainOf(t, linked(es))

	for drop := range 5 {
		t.Run(fmt.Sprintf("drop_claim_%d", drop), func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(signed, &doc); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			claims, _ := doc["claims"].([]any)
			if drop >= len(claims) {
				t.Skip("fixture shorter than expected")
			}
			doc["claims"] = append(append([]any{}, claims[:drop]...), claims[drop+1:]...)
			forged, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			rep, err := audit.VerifyBundle(forged, id.KeyID())
			if err != nil {
				return // refusing to parse it is a pass
			}
			if rep.OK() {
				t.Errorf("removing claim %d left the receipt verifying, so a holder can excise an "+
					"entry and still hand over a receipt that passes", drop)
			}
		})
	}
}

// TestAReceiptFromAnotherInstallDoesNotVerifyHere pins provenance. Evidence lifted from one install
// and presented as another's would let anyone borrow a clean history.
func TestAReceiptFromAnotherInstallDoesNotVerifyHere(t *testing.T) {
	// Not parallel: chainOf sets an environment variable to isolate the signing identity.
	base := time.Date(2026, 9, 9, 7, 0, 0, 0, time.UTC)
	es := linked([]*audit.Entry{{
		ID: audit.NewID(), At: base, Actor: "admin", ActorType: "user",
		Method: "POST", Path: "/v1/runs/1",
	}})
	_, signed, _ := chainOf(t, es)

	// A different install, so a different key.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	other, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, other.KeyID())
	if err != nil {
		return // refusing outright is a pass
	}
	if rep.OK() {
		t.Error("a receipt signed by one install verified against another install's pinned key")
	}
	if !strings.Contains(fmt.Sprint(rep.SignatureOK), "false") && rep.SignatureOK {
		t.Error("the signature check passed for a key that did not sign it")
	}
}
