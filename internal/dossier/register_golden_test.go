package dossier

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// collapsedDigest returns the sha256 of doc after every run of whitespace is collapsed to one space,
// so a golden pins the words and markup an auditor receives and not the template's indentation.
func collapsedDigest(doc string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(doc), " ")))
	return hex.EncodeToString(sum[:])
}

// goldenBase is the instant every golden input is built around, so the documents are reproducible.
var goldenBase = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// goldenTruncated is a register cut at its cap: an unanchored chain, a held run, a rejection made on
// behalf of an account, and a dry run the gate did not find change free.
func goldenTruncated() *RegisterInput {
	return &RegisterInput{
		From: goldenBase, To: goldenBase.Add(7 * 24 * time.Hour), GeneratedAt: goldenBase,
		Limit: 2, Truncated: true, CoveredTo: goldenBase.Add(2 * time.Hour),
		Runs: []*run.Run{{
			ID: "run_held", Playbook: "site.yml", Status: run.StatusSucceeded, Actor: "deploy-bot",
			Source: "template", SourceID: "tpl_web", HeldByPolicy: "prod needs a person",
			CreatedAt: goldenBase,
		}, {
			ID: "run_dry", Tool: "plugin-tool", Command: "plan everything", DryRun: true,
			Status: run.StatusSucceeded, Actor: "root", CreatedAt: goldenBase.Add(time.Hour),
		}},
		Decisions: map[string]Decision{
			"run_held": {Verdict: "Rejected", Actor: "approver", OnBehalfOf: "ops", At: goldenBase,
				Seq: 7},
		},
		ChainOK: true, ChainCount: 12, Head: &audit.Entry{Seq: 12, Hash: "fedcba"},
	}
}

// goldenPruned is a whole period that retention thinned, over a broken chain, holding a failed
// script run with no actor and an inline secret. pruned sets how many changes retention removed,
// so the singular and the plural sentence are both pinned.
func goldenPruned(pruned int) *RegisterInput {
	return &RegisterInput{
		From: goldenBase, To: goldenBase.Add(24 * time.Hour), GeneratedAt: goldenBase,
		Limit: MaxRegisterRuns, Pruned: pruned,
		Runs: []*run.Run{{
			ID: "run_script", Tool: run.ToolBash, Status: run.StatusFailed,
			Command: "export DB_PASSWORD=hunter2secret\npsql -c 'select 1'", CreatedAt: goldenBase,
		}},
		Decisions: map[string]Decision{},
		ChainOK:   false, ChainBrokeAt: 4, ChainCount: 6,
	}
}

// TestRegisterDocumentMatchesItsGolden pins the change register document, word for word, against
// digests captured before the view the template renders was split out into BuildRegister.
//
// The register is the document mailed to an auditor, and that split moved its two notice sentences
// out of the template and into the view. A refactor that changes a word the auditor reads, drops a
// row's decision, or reorders a column fails here even when every narrower test still passes.
//
// To regenerate after an intended change to the document, run
// go test -run TestRegisterDocumentMatchesItsGolden ./internal/dossier and copy each digest the
// failure prints into WantDigest.
func TestRegisterDocumentMatchesItsGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Render       func(t *testing.T) string
		WantContains string
		WantDigest   string
	}{{ // Test 0: The sample register, carrying one of everything the template can show.
		Name: "sample", Render: sampleRegister, WantContains: "Approved by root",
		WantDigest: "126a6588e3849e791f3440773fccda0dbb8dd3994d0470fc95f87512fc98e912",
	}, { // Test 1: A register cut at its cap says so, with its bound and where it stops.
		Name: "truncated", Render: renderGolden(goldenTruncated()),
		WantContains: "carries the earliest 2 and stops at 2026-07-01T14:00:00Z",
		WantDigest:   "4ab1ad920cf9b2049d201924afc5d39c94bff790597cdcbd60ef839de1b8613b",
	}, { // Test 2: One change pruned by retention is named in the singular.
		Name: "pruned one", Render: renderGolden(goldenPruned(1)),
		WantContains: "The chain records 1 further change in this period",
		WantDigest:   "28368173a5adc5a4a068926ae08bc7f078f276e7ca95c01835b9de4dd7bc895e",
	}, { // Test 3: Several pruned changes are named in the plural.
		Name: "pruned three", Render: renderGolden(goldenPruned(3)),
		WantContains: "The chain records 3 further changes in this period",
		WantDigest:   "51c650cd524eee448e8fa25fd36a6f1aa802c9e7bc4e022e078d2cf2c4dc6fb2",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := test.Render(t)
			// The golden only pins what its input reaches, so each case first proves it reached the
			// part of the document it exists for.
			if collapsed := strings.Join(strings.Fields(doc), " "); !strings.Contains(collapsed,
				test.WantContains) {
				t.Fatalf("%s: the document does not carry %q, so this golden pins nothing", test.Name,
					test.WantContains)
			}
			got := collapsedDigest(doc)
			if diff := cmp.Diff(test.WantDigest, got); diff != "" {
				t.Errorf("%s: the register document changed (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// renderGolden returns a renderer for one golden input, failing the test if the document does not
// render at all.
func renderGolden(in *RegisterInput) func(t *testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		doc, err := RenderRegister(in)
		if err != nil {
			t.Fatalf("RenderRegister() error = %v", err)
		}
		return string(doc)
	}
}
