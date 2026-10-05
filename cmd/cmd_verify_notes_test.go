package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
)

// TestPrintOutcomeNamesWhatAPolicyNoted pins what verify prints for a run that went ahead past a
// warning. The note is in the digest-verified outcome, and a reader of the verdict has to see it
// beside the rules that let the run through, not have to open the JSON to find it.
func TestPrintOutcomeNamesWhatAPolicyNoted(t *testing.T) {
	t.Parallel()
	exit := 0
	note := "staging-advice (no change ticket on the run, rego sha256:0123456789ab)"
	body, err := json.Marshal(outcome.Record{
		RunID: "run_n", Status: "succeeded", ExitCode: &exit, LogSHA256: "abc",
		PolicyNotes: []string{note},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var out strings.Builder
	printOutcome(&out, body)
	if want := "policy note    " + note; !strings.Contains(out.String(), want) {
		t.Errorf("the printed outcome is missing %q:\n%s", want, out.String())
	}

	bare, err := json.Marshal(outcome.Record{RunID: "run_b", Status: "succeeded", ExitCode: &exit,
		LogSHA256: "abc"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	out.Reset()
	printOutcome(&out, bare)
	if strings.Contains(out.String(), "policy note") {
		t.Errorf("a run nothing noted printed a policy note:\n%s", out.String())
	}
}
