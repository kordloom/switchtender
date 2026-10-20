package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	_ "modernc.org/sqlite"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// seededChain is a SQLite install holding a short chain with one receiptable run in the middle of
// it, so a break can be placed before that run's own entries as well as after them. The middle is
// what matters: a run whose entries opened or closed the chain could not tell a check over the
// whole chain apart from one over the run's own segment, which is the difference these tests are
// about.
type seededChain struct {
	// DB is the SQLite file the commands are pointed at.
	DB string
	// RunID names the receiptable run.
	RunID string
	// Entries is how many entries the seeded chain holds.
	Entries int
}

// seedChainDB writes that install into a temporary directory and returns where it is.
//
// Times carry no nanoseconds, because the bundle builder refuses an entry recorded at nanosecond
// precision and that refusal would stand in for the break a test is about.
func seedChainDB(t *testing.T) seededChain {
	t.Helper()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	filler := func(minute int) {
		if err := store.Audits().Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: at.Add(time.Duration(minute) * time.Minute), Actor: "alice",
			ActorType: "session", Method: "POST", Path: "/v1/projects",
		}); err != nil {
			t.Fatalf("Append(filler at minute %d) error = %v", minute, err)
		}
	}

	filler(0)
	creation := &audit.Entry{
		ID: audit.NewID(), At: at.Add(time.Minute), Actor: "operator-jane", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := store.Audits().Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	r := &run.Run{
		ID: "run_receipted", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "operator-jane", ActorType: "session", AuditReceipt: audit.Receipt(creation),
		CreatedAt: at, StartedAt: &at,
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	filler(2)

	ended := at.Add(3 * time.Minute)
	r.Status, r.EndedAt = run.StatusSucceeded, &ended
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	err = outcome.Commit(ctx, store.Audits(), store.Runs(), r, "system:dispatcher", nil)
	if err != nil {
		t.Fatalf("outcome.Commit() error = %v", err)
	}
	filler(4)

	return seededChain{DB: db, RunID: r.ID, Entries: 5}
}

// editChain applies one SQL statement to a seeded database, standing in for whoever reaches the
// file the install trusts. The commands read the chain back through the store, so an edit here is
// exactly the tamper an operator running these commands is trying to detect.
func editChain(t *testing.T, db, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatalf("exec %q error = %v", statement, err)
	}
}

// refusalReason returns the reason code a refusal opens with, which is the value the HTTP endpoints
// answer in their reason field, so a test can hold the two forms of the same refusal against
// each other.
func refusalReason(err error) string {
	if err == nil {
		return ""
	}
	reason, _, found := strings.Cut(err.Error(), ": ")
	if !found {
		return ""
	}
	return reason
}

// storedChain returns a seeded chain as its store hands it back, oldest first, for the checks that
// work on entries rather than on a database.
func storedChain(t *testing.T, entries int) []*audit.Entry {
	t.Helper()
	ctx := context.Background()
	store := audit.NewMemStore()
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for i := range entries {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: at.Add(time.Duration(i) * time.Minute), Actor: "alice",
			ActorType: "session", Method: "POST", Path: "/v1/projects",
		}); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	return chain
}

// TestRefuseUnpublishableChain pins the coordinates and the reason codes the commands refuse with,
// which are the ones GET /v1/audit/bundle and GET /v1/runs/{id}/receipt answer with. The two used
// to disagree: the endpoints refused a chain that does not verify while the commands signed over
// it, so an operator who suspected tampering got the reassuring answer from the tool they reached
// for first. Reason and position are what let one answer be read against the other, so both are
// pinned here rather than only the fact of the refusal.
func TestRefuseUnpublishableChain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Edit alters the intact chain and returns what the store would hand back afterwards. Nil
		// leaves it intact.
		Edit func([]*audit.Entry) []*audit.Entry
		// Name labels the state of the chain.
		Name string
		// Artifact is what could not be published, which travels into the message.
		Artifact string
		// WantReason is the reason code the refusal must open with, empty when it must not refuse.
		WantReason string
		// WantHas is text the refusal must carry.
		WantHas []string
		// WantLacks is text the refusal must not carry.
		WantLacks []string
		// Anchored records an anchor over the chain's head before the edit is applied, which is how
		// a chain that hash-verifies but has lost its tail is caught.
		Anchored bool
	}{{ // Test 0: The control. An intact chain with no anchors is publishable.
		Name: "intact", Artifact: "a bundle",
	}, { // Test 1: A break at the first entry, which a windowed bundle and a run's own segment both
		// look straight past.
		Name: "break at the head of the chain", Artifact: "a bundle",
		Edit: func(entries []*audit.Entry) []*audit.Entry {
			entries[0].Actor = "mallory"
			return entries
		},
		WantReason: reasonChainBreak,
		WantHas: []string{
			"entry 1 of 5", "sequence 1", "as a bundle", "altered, reordered, or removed",
			"not a fault in this command", "GET /v1/audit/verify",
		},
	}, { // Test 2: A break at the last entry, so the position moves with the tamper.
		Name: "break at the tail of the chain", Artifact: "a receipt",
		Edit: func(entries []*audit.Entry) []*audit.Entry {
			entries[4].Path = "/v1/runs/deleted"
			return entries
		},
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 5 of 5", "sequence 5", "as a receipt"},
		WantLacks:  []string{"as a bundle"},
	}, { // Test 3: An entry removed, so the entry after it links to something no longer there and
		// the break is named at the entry that lost its link rather than at the gap.
		Name: "an entry removed", Artifact: "a bundle",
		Edit: func(entries []*audit.Entry) []*audit.Entry {
			return entries[1:]
		},
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 1 of 4", "sequence 2"},
	}, { // Test 4: A blanked sequence, so the break has no coordinate in the trail to name and the
		// refusal must not print one anyway.
		Name: "a blanked sequence", Artifact: "a bundle",
		Edit: func(entries []*audit.Entry) []*audit.Entry {
			entries[0].Seq = 0
			return entries
		},
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 1 of 5"},
		WantLacks:  []string{"sequence"},
	}, { // Test 5: The control for anchoring. A chain that still reaches its anchor is publishable.
		Name: "intact and anchored", Artifact: "a bundle", Anchored: true,
	}, { // Test 6: A chain cut back below its anchor. Every remaining link recomputes, because a
		// prefix of a valid chain is itself a valid chain, so only the anchor catches this.
		Name: "cut back below its anchor", Artifact: "a receipt", Anchored: true,
		Edit: func(entries []*audit.Entry) []*audit.Entry {
			return entries[:3]
		},
		WantReason: reasonAnchorUnsatisfied,
		WantHas: []string{
			"the chain ends at entry 3", "anchor was taken over entry 5", "a receipt drawn from it",
			"not a fault in this command",
		},
	}, { // Test 7: A window handed to the guard instead of the whole chain. The walk verifies from
		// genesis, so a slice that does not open the chain is refused rather than agreed with. That
		// is what stops the defect coming back as a caller quietly passing the same window it signs:
		// the window verifies on its own under VerifyRange, which is exactly why the builder never
		// saw the break.
		Name: "a window handed to the guard", Artifact: "a bundle",
		Edit: func(entries []*audit.Entry) []*audit.Entry {
			return entries[3:]
		},
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 1 of 2"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entries := storedChain(t, 5)
			var anchors []*audit.Anchor
			if test.Anchored {
				head := entries[len(entries)-1]
				anchors = append(anchors, &audit.Anchor{
					ID: "anc_test", Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeLinear,
					Seq: head.Seq, Link: head.Hash, At: head.At, InstallID: "in_test",
				})
			}
			if test.Edit != nil {
				entries = test.Edit(entries)
			}

			err := refuseUnpublishableChain(entries, anchors, audit.Identity{InstallID: "in_test"}, test.Artifact)
			if test.WantReason == "" {
				if err != nil {
					t.Fatalf("refuseUnpublishableChain() error = %v, want it to publish", err)
				}
				return
			}
			if err == nil {
				t.Fatal("refuseUnpublishableChain() error = nil, want it to refuse")
			}
			if diff := cmp.Diff(test.WantReason, refusalReason(err), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reason code mismatch (-want +got):\n%s\nrefusal: %v", diff, err)
			}
			for _, want := range test.WantHas {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			for _, unwanted := range test.WantLacks {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("refusal %q still carries %q", err, unwanted)
				}
			}
		})
	}
}

// TestAuditBundleHoldsTheWholeChainBeforeWindowing is the end-to-end pin on the windowing defect.
//
// The command held the whole chain against the anchors but handed the builder the windowed slice,
// and the builder is the only thing that checked the links, so the object that was checked and the
// object that was signed were not the same one. A break older than a --limit range was therefore
// never looked at: the command wrote a signed bundle, printed the fingerprint to publish, and the
// offline verifier read it and reported that nothing had been altered, all while GET
// /v1/audit/bundle was refusing the same database.
func TestAuditBundleHoldsTheWholeChainBeforeWindowing(t *testing.T) {
	// Not parallel: the bundle command reads package-level flag variables.
	tests := []struct {
		// Name labels the state of the chain.
		Name string
		// Edit is the SQL tamper applied to the seeded database, empty to leave it intact.
		Edit string
		// Limit is --limit, the window cut from the newest entries.
		Limit int
		// From and To are --from-seq and --to-seq, the window by sequence.
		From, To int64
		// WantReason is the reason code the refusal must open with, empty when the bundle must be
		// written.
		WantReason string
		// WantHas is text the refusal must carry.
		WantHas []string
		// WantClaims is how many claims the written bundle must carry, read only when it is written.
		WantClaims int
		// WantFirst is the sequence of the first claim, read only when it is set.
		WantFirst int64
	}{{ // Test 0: The control. An intact chain still bundles, windowed, and the bundle verifies.
		Name: "intact and windowed", Limit: 2, WantClaims: 2,
	}, { // Test 1: The defect. The break is at entry 1 and the window is the last two entries, so
		// every claim that would be signed is genuinely past it.
		Name: "a break older than the window", Limit: 2,
		Edit:       "UPDATE audit_entries SET actor = 'mallory' WHERE seq = 1",
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 1 of 5", "sequence 1", "as a bundle"},
	}, { // Test 2: The same break with no window, which the builder always caught, so this pins that
		// the coordinates did not move when the check did.
		Name: "a break with no window",
		Edit: "UPDATE audit_entries SET actor = 'mallory' WHERE seq = 1", WantReason: reasonChainBreak,
		WantHas: []string{"entry 1 of 5", "sequence 1"},
	}, { // Test 3: An entry deleted from the middle, which is the tamper a windowed export makes
		// invisible: the window is contiguous and recomputes on its own.
		Name: "an entry deleted below the window", Limit: 2,
		Edit:       "DELETE FROM audit_entries WHERE seq = 2",
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 2 of 4", "sequence 3"},
	}, { // Test 4: A window by sequence from the middle carries exactly that slice and verifies.
		Name: "intact and windowed by sequence", From: 2, To: 4, WantClaims: 3, WantFirst: 2,
	}, { // Test 5: A break below a window by sequence is refused, as it is below a --limit window.
		Name: "a break older than the sequence window", From: 3, To: 5,
		Edit:       "UPDATE audit_entries SET actor = 'mallory' WHERE seq = 1",
		WantReason: reasonChainBreak,
		WantHas:    []string{"entry 1 of 5", "sequence 1"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			seed := seedChainDB(t)
			if test.Edit != "" {
				editChain(t, seed.DB, test.Edit)
			}
			out := filepath.Join(filepath.Dir(seed.DB), "bundle.json")
			bundleDB, bundleOut, bundleLimit = seed.DB, out, test.Limit
			bundleFromSeq, bundleToSeq = test.From, test.To
			t.Cleanup(func() {
				bundleDB, bundleOut, bundleLimit, bundleKeyDir = defaultDBPath, "", 0, ""
				bundleFromSeq, bundleToSeq = 0, 0
			})

			err := runAuditBundle(testCommand(), nil)
			if test.WantReason != "" {
				if err == nil {
					t.Fatal("runAuditBundle() error = nil, want it to refuse a chain that does not verify")
				}
				if diff := cmp.Diff(test.WantReason, refusalReason(err), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("reason code mismatch (-want +got):\n%s\nrefusal: %v", diff, err)
				}
				for _, want := range test.WantHas {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not name %q", err, want)
					}
				}
				// Nothing signed may reach the disk. A refusal that still leaves the artifact behind
				// is not a refusal, because the file is what gets handed on.
				if _, serr := os.Stat(out); serr == nil {
					t.Error("the command refused and wrote the bundle anyway")
				}
				return
			}
			if err != nil {
				t.Fatalf("runAuditBundle() error = %v, want a bundle", err)
			}
			signed, rerr := os.ReadFile(out)
			if rerr != nil {
				t.Fatalf("ReadFile() error = %v", rerr)
			}
			id, ierr := installIdentity(seed.DB)
			if ierr != nil {
				t.Fatalf("loadProducerIdentity() error = %v", ierr)
			}
			report, verr := audit.VerifyBundle(signed, id.KeyID())
			if verr != nil {
				t.Fatalf("VerifyBundle() error = %v", verr)
			}
			if !report.OK() {
				t.Errorf("the written bundle does not verify: %+v", report)
			}
			if diff := cmp.Diff(test.WantClaims, report.ClaimCount); diff != "" {
				t.Errorf("claim count mismatch (-want +got):\n%s", diff)
			}
			if test.WantFirst != 0 {
				var b audit.Bundle
				if err := json.Unmarshal(signed, &b); err != nil {
					t.Fatalf("Unmarshal() error = %v", err)
				}
				if diff := cmp.Diff(test.WantFirst, b.Claims[0].Chain.Seq); diff != "" {
					t.Errorf("first claim sequence mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestABundleWindowIsBoundedBeforeTheChainIsRead pins the command to the endpoint's ceiling. The
// command had none: it read the whole chain to cut a window from it, and held about 1.7 GB to bundle
// 100,000 entries, so the tool an operator reaches for on the server's own box could take it down.
// A window that could never be served is refused before the store is opened, and an unwindowed
// export past the ceiling is refused with the flags that would serve it.
func TestABundleWindowIsBoundedBeforeTheChainIsRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Limit, From, and To are the flags as passed.
		Limit    int
		From, To int64
		// Count and Highest describe the walked chain.
		Count   int
		Highest int64
		// WantAfter and WantLast are the slice served, as the sequence before it and its last.
		WantAfter, WantLast int64
		// WantMsg is a distinctive part of the refusal, empty when the slice is served.
		WantMsg string
	}{{ // Test 0: No window on a chain under the ceiling carries the whole chain.
		Count: 10, Highest: 10, WantAfter: 0, WantLast: 10,
	}, { // Test 1: A limit carries the newest entries.
		Limit: 4, Count: 10, Highest: 10, WantAfter: 6, WantLast: 10,
	}, { // Test 2: A limit wider than the chain carries the chain.
		Limit: 50, Count: 10, Highest: 10, WantAfter: 0, WantLast: 10,
	}, { // Test 3: A window by sequence carries exactly its slice.
		From: 20, To: 40, Count: 60, Highest: 60, WantAfter: 19, WantLast: 40,
	}, { // Test 4: A window past the head is refused rather than cut short.
		From: 50, To: 61, Count: 60, Highest: 60, WantMsg: "past the newest entry",
	}, { // Test 5: The defect. An unwindowed export past the ceiling names the flags.
		Count: maxBundleEntries + 1, Highest: maxBundleEntries + 1, WantMsg: "--from-seq and --to-seq",
	}, { // Test 6: A window into the same chain is served.
		From: 1, To: 10_000, Count: maxBundleEntries + 1, Highest: maxBundleEntries + 1,
		WantAfter: 0, WantLast: 10_000,
	}, { // Test 7: A limit past the ceiling.
		Limit: maxBundleEntries + 1, WantMsg: "at most",
	}, { // Test 8: A negative limit.
		Limit: -1, WantMsg: "a count",
	}, { // Test 9: Both kinds of window.
		Limit: 5, From: 1, To: 5, WantMsg: "not both",
	}, { // Test 10: One end alone.
		From: 3, WantMsg: "go together",
	}, { // Test 11: The other end alone.
		To: 3, WantMsg: "go together",
	}, { // Test 12: Ends reversed.
		From: 9, To: 3, WantMsg: "is past --to-seq",
	}, { // Test 13: One past the ceiling.
		From: 1, To: maxBundleEntries + 1, WantMsg: "more than the 25000",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkBundleWindow(test.Limit, test.From, test.To)
			var after, last int64
			if err == nil {
				after, last, err = bundleSlice(test.Count, test.Highest, test.Limit, test.From, test.To)
			}
			if test.WantMsg != "" {
				if err == nil || !strings.Contains(err.Error(), test.WantMsg) {
					t.Fatalf("error = %v, want one saying %q", err, test.WantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want the slice served", err)
			}
			if diff := cmp.Diff([]int64{test.WantAfter, test.WantLast}, []int64{after, last}); diff != "" {
				t.Errorf("after, last mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
