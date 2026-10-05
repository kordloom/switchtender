package dossier

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRunMetaSaysWhatAPolicyNoted pins the dossier for a run that went ahead past a warning. The
// document handed to an auditor lists each note, and a run nothing noted gains no row for it.
func TestRunMetaSaysWhatAPolicyNoted(t *testing.T) {
	t.Parallel()
	notes := []string{
		"staging-advice (no change ticket on the run, rego sha256:0123456789ab)",
		`step "deploy": release-advice (an agent asked, rego sha256:abcdef012345)`,
	}
	tests := []struct {
		// Notes are the run's notes.
		Notes []string
		// WantRows are the Policy note rows the dossier must carry.
		WantRows []string
	}{{ // Test 0: Each note is its own row.
		Notes: notes, WantRows: notes,
	}, { // Test 1: No notes, no rows.
		Notes: nil, WantRows: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rows := runMeta(&run.Run{ID: "run_noted", Playbook: "site.yml", PolicyNotes: test.Notes,
				CreatedAt: evidenceTime})
			var got []string
			for _, row := range rows {
				if row.K == "Policy note" {
					got = append(got, row.V)
				}
			}
			if diff := cmp.Diff(test.WantRows, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("policy note rows mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
