// Package loomsealtest holds what the LoomSeal cross-verification shares across packages: running
// the format's independent Python reference verifier over a document this product produced, and
// requiring it to agree with the Go verifier on the same bytes.
package loomsealtest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PythonEnv names the Python interpreter that runs the reference verifier. Empty runs python3.
const PythonEnv = "SWITCHTENDER_LOOMVERIFY_PYTHON"

// fullSuiteEnv turns an unavailable reference verifier from a skip into a failure.
const fullSuiteEnv = "SWITCHTENDER_REQUIRE_FULL_SUITE"

// report is the part of a verifier's JSON report the two implementations must agree on.
type report struct {
	// OK is the verdict.
	OK bool `json:"ok"`
	// Problems say why a document did not verify.
	Problems []string `json:"problems"`
	// LegacyRecords are the record bodies verified under the legacy unkeyed digest form, reported by
	// LoomSeal from 1.7.0 on.
	LegacyRecords []string `json:"legacy_records"`
	// Disclosed are the members beside the chain links and their states, reported by LoomSeal from
	// 1.7.0 on.
	Disclosed []struct {
		// Claim is the claim's index.
		Claim int `json:"claim"`
		// Member is the payload member.
		Member string `json:"member"`
		// State is checked, unchecked, or redacted.
		State string `json:"state"`
		// Detail is what it was held against, or why not.
		Detail string `json:"detail"`
	} `json:"disclosed"`
}

// RequireReferenceAgreement runs the reference verifier in repo, a LoomSeal checkout, over the
// document at path, and fails t unless it agrees with goReport, the JSON report the Go verifier
// gave for the same document: the same verdict, the same disclosed members in the same states, and
// the same records named as legacy.
// It also fails when either names a member as undeclared, which is this product disclosing
// something the format's schema/claim-members.json does not declare, so a new disclosure cannot
// ship unchecked.
//
// The agreement is held from the LoomSeal release that ships the member declaration, 1.7.0, which
// is also the release whose reference verifier stopped disagreeing with the Go one over an anchor
// on a consistency root. An older checkout is logged and left alone. A checkout from that release
// on without a Python that runs the reference skips the agreement, unless the full suite was
// demanded, where it fails instead.
func RequireReferenceAgreement(t testing.TB, repo, path string, goReport []byte) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(repo, "schema", "claim-members.json")); err != nil {
		t.Log("the pinned LoomSeal predates the member declaration, so its reference verifier is " +
			"not held to agreement")
		return
	}
	script := filepath.Join(repo, "reference", "loomverify.py")
	if _, err := os.Stat(script); err != nil {
		standDown(t, "the LoomSeal checkout has no reference verifier at "+script)
		return
	}
	python := os.Getenv(PythonEnv)
	if python == "" {
		python = "python3"
	}
	cmd := exec.Command(python, script, path)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, _ := cmd.Output()
	var ref report
	if err := json.Unmarshal(out, &ref); err != nil {
		standDown(t, fmt.Sprintf("the reference verifier did not run under %s: %v", python, err))
		return
	}
	var got report
	if err := json.Unmarshal(goReport, &got); err != nil {
		t.Fatalf("the Go verifier's report is not JSON: %v\n%s", err, goReport)
	}
	if got.OK != ref.OK {
		t.Fatalf("the Go verifier says ok=%v and the reference says ok=%v on the same document:\n"+
			"go: %v\nreference: %v", got.OK, ref.OK, got.Problems, ref.Problems)
	}
	if !got.OK {
		return
	}
	goStates, refStates := states(got), states(ref)
	if goStates != refStates {
		t.Errorf("the verifiers disagree on the disclosed members:\ngo:        %s\nreference: %s",
			goStates, refStates)
	}
	if goLegacy, refLegacy := strings.Join(got.LegacyRecords, ", "),
		strings.Join(ref.LegacyRecords, ", "); goLegacy != refLegacy {
		t.Errorf("the verifiers disagree on the legacy records:\ngo:        %s\nreference: %s",
			goLegacy, refLegacy)
	}
	for _, r := range []report{got, ref} {
		for _, m := range r.Disclosed {
			if strings.HasPrefix(m.Detail, "not declared") {
				t.Errorf("this product discloses %s on claim %d, which LoomSeal does not declare: %s",
					m.Member, m.Claim, m.Detail)
			}
		}
	}
}

// states renders a report's disclosed members as one comparable line.
func states(r report) string {
	parts := make([]string, 0, len(r.Disclosed))
	for _, m := range r.Disclosed {
		parts = append(parts, fmt.Sprintf("claim %d %s %s", m.Claim, m.Member, m.State))
	}
	return strings.Join(parts, "; ")
}

// standDown skips the agreement, or fails it when the full suite was demanded.
func standDown(t testing.TB, why string) {
	t.Helper()
	if os.Getenv(fullSuiteEnv) == "1" {
		t.Fatalf("%s is set and %s, so the reference verifier never checked this document",
			fullSuiteEnv, why)
	}
	t.Log("skipping the reference verifier: " + why)
}
