package server

import (
	"testing"
)

// TestDoctorFindingsNameSomethingYouCanOpen covers a report an operator cannot act on.
//
// A name is optional on a template, a schedule and a credential, and the API creates unnamed ones
// without complaint. The schedule tutorial's own copyable command produces one. The doctor then
// listed "schedule  | Fires template tpl_abc123, which no longer exists" with an empty space where
// the identity should be, twice over, and a reader working through a list of problems had no way to
// tell which object each one meant.
func TestDoctorFindingsNameSomethingYouCanOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		ID   string
		Want string
	}{{ // Test 0: A named object is called by its name.
		Name: "named", In: "nightly audit", ID: "sch_1", Want: "nightly audit",
	}, { // Test 1: An unnamed one falls back to the id, which is what the reader searches for.
		Name: "unnamed", In: "", ID: "sch_50acef79e54db9c5", Want: "sch_50acef79e54db9c5",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if got := namedOr(test.In, test.ID); got != test.Want {
				t.Errorf("%s: namedOr(%q, %q) = %q, want %q: a finding nobody can act on is not a "+
					"finding", test.Name, test.In, test.ID, got, test.Want)
			}
		})
	}
}
