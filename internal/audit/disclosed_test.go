package audit

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDisclosedMembersAreReportedInThreeStates pins how this product's verifier reports a member
// the chain link does not commit. A span beat's members are bound to the path the link commits, so
// they are checked, and a beat whose members were changed after signing fails. A member this
// product never discloses is reported unchecked and counted, so a document carrying one cannot read
// as plain VERIFIED.
func TestDisclosedMembersAreReportedInThreeStates(t *testing.T) {
	tests := []struct {
		// Edit changes a claim's payload after the bundle is built and before it is signed.
		Edit func(claims []BundleClaim)
		// WantOK is whether the document verifies.
		WantOK bool
		// WantUnchecked is how many disclosed records the report counts unchecked.
		WantUnchecked int
		// WantState is the state the span beat's cadence_s is reported in.
		WantState string
	}{{ // Test 0: A span beat's members agree with its committed path and are checked.
		WantOK: true, WantState: MemberChecked,
	}, { // Test 1: A cadence widened after the fact disagrees with the path and fails.
		Edit:   func(claims []BundleClaim) { claims[1].Payload["cadence_s"] = float64(3600) },
		WantOK: false, WantUnchecked: 4, WantState: MemberUnchecked,
	}, { // Test 2: A member this product never discloses is unchecked and counted.
		Edit:   func(claims []BundleClaim) { claims[0].Payload["note"] = "carried beside the link" },
		WantOK: true, WantUnchecked: 1, WantState: MemberChecked,
	}, { // Test 3: A name differing from a span member only in case fails the beat.
		Edit:   func(claims []BundleClaim) { claims[1].Payload["Cadence_s"] = float64(3600) },
		WantOK: false, WantUnchecked: 5, WantState: MemberUnchecked,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
			id, err := LoadIdentity(t.TempDir())
			if err != nil {
				t.Fatalf("LoadIdentity() error = %v", err)
			}
			at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
			entries := []*Entry{
				{ID: "aud_001", Seq: 1, At: at, Actor: "release-token", Method: "POST",
					Path: "/v1/runs", InstallID: id.InstallID},
				{ID: "aud_002", Seq: 2, At: at.Add(time.Second), Actor: SpanActor,
					Method: SpanMethod, Path: SpanPath(1, 1, 60), InstallID: id.InstallID},
			}
			var prev *Entry
			for _, e := range entries {
				Link(prev, e)
				prev = e
			}
			doc, err := BuildBundle(entries, id, "1.102.0", at.Add(time.Minute))
			if err != nil {
				t.Fatalf("BuildBundle() error = %v", err)
			}
			if test.Edit != nil {
				test.Edit(doc.Claims)
			}
			signed, err := SignBundleDoc(doc, id.Private())
			if err != nil {
				t.Fatalf("SignBundleDoc() error = %v", err)
			}
			rep, err := VerifyBundle(signed, id.KeyID())
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if rep.OK() != test.WantOK {
				t.Fatalf("OK() = %v, want %v: %+v", rep.OK(), test.WantOK, rep)
			}
			if rep.DisclosedUnchecked != test.WantUnchecked {
				t.Errorf("DisclosedUnchecked = %d, want %d: %+v", rep.DisclosedUnchecked,
					test.WantUnchecked, rep.Disclosed)
			}
			var cadence string
			for _, m := range rep.Disclosed {
				if m.Claim == 1 && m.Member == "cadence_s" {
					cadence = m.State
				}
				if m.Member == "note" && !strings.Contains(m.Detail, "does not disclose") {
					t.Errorf("the undisclosed member reads %q, want why nothing checks it", m.Detail)
				}
			}
			if cadence != test.WantState {
				t.Errorf("cadence_s is %q, want %q: %+v", cadence, test.WantState, rep.Disclosed)
			}
		})
	}
}
