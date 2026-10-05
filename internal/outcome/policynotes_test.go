package outcome

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestBodyCommitsWhatAPolicyNoted pins the signed outcome of a run a policy noted. The run went
// ahead past a warning, so the record the chain commits has to say what the warning was, or the
// receipt for it reads exactly like one for a run nothing flagged. A run nothing noted carries no
// key at all, so its record reduces to the bytes it always did.
func TestBodyCommitsWhatAPolicyNoted(t *testing.T) {
	t.Parallel()
	note := `staging-advice (no "ticket" label, rego sha256:0123456789ab)`
	tests := []struct {
		// Notes are the run's notes.
		Notes []string
		// WantJSON is the fragment the body must carry, when WantKey.
		WantJSON string
		// WantKey is whether the body carries policy_notes at all.
		WantKey bool
	}{{ // Test 0: A noted run commits its note.
		Notes:    []string{note},
		WantJSON: `"policy_notes":["staging-advice (no \"ticket\" label, rego sha256:0123456789ab)"]`,
		WantKey:  true,
	}, { // Test 1: A run nothing noted commits no key.
		Notes: nil, WantKey: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded,
				ExitCode: intPtr(0), PolicyNotes: test.Notes}
			body, err := Body(context.Background(), &fakeRunStore{}, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if got := strings.Contains(string(body), `"policy_notes"`); got != test.WantKey {
				t.Fatalf("body = %s, carries policy_notes = %v, want %v", body, got, test.WantKey)
			}
			if test.WantKey && !strings.Contains(string(body), test.WantJSON) {
				t.Errorf("body = %s, want it to carry %s", body, test.WantJSON)
			}
			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if diff := cmp.Diff(len(test.Notes), len(rec.PolicyNotes)); diff != "" {
				t.Errorf("parsed notes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
