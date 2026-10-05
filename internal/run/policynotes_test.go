package run

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestPolicyNotesAreTheRunsOwnAndReachTheAPI pins the three things a run's policy notes need from
// the run model. A clone owns its notes, since a store hands clones to every reader and a shared
// backing array would let any reader rewrite evidence. The notes are serialized, because the run
// page and every integration read them from the API, and omitted when there are none. And a run
// derived from this one does not inherit them, because the derived request is judged again.
func TestPolicyNotesAreTheRunsOwnAndReachTheAPI(t *testing.T) {
	t.Parallel()
	note := "staging-advice (no change ticket on the run, rego sha256:0123456789ab)"
	orig := &Run{ID: "run_noted", Tool: ToolBash, Command: "uptime", PolicyNotes: []string{note}}

	clone := orig.Clone()
	clone.PolicyNotes[0] = "forged"
	if diff := cmp.Diff([]string{note}, orig.PolicyNotes); diff != "" {
		t.Errorf("writing through the clone rewrote the original's notes (-want +got):\n%s", diff)
	}

	body, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(body), `"policy_notes":["staging-advice (no change ticket`) {
		t.Errorf("the serialized run does not carry its notes: %s", body)
	}
	bare, err := json.Marshal(&Run{ID: "run_bare"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(bare), "policy_notes") {
		t.Errorf("a run nothing noted carries policy_notes: %s", bare)
	}

	derived := &Run{ID: "run_derived"}
	ApplyOptions(derived, orig.ExecutionOptions())
	if len(derived.PolicyNotes) != 0 {
		t.Errorf("a derived run inherited %v, want its own notes from its own judgment",
			derived.PolicyNotes)
	}
}

// TestSanitizeCleansPolicyNotes pins that a note goes through the same cleaning as every other text
// field before a store writes it. A module builds a note from text this install did not choose, and
// a byte PostgreSQL refuses would fail the write that records the run.
func TestSanitizeCleansPolicyNotes(t *testing.T) {
	t.Parallel()
	r := &Run{ID: "run_dirty", PolicyNotes: []string{"clean", "bad\x00byte"}}
	r.Sanitize()
	for _, note := range r.PolicyNotes {
		if strings.ContainsRune(note, 0) {
			t.Errorf("note %q still holds a NUL byte after Sanitize", note)
		}
	}
	if r.PolicyNotes[0] != "clean" {
		t.Errorf("Sanitize rewrote a clean note to %q", r.PolicyNotes[0])
	}
}
