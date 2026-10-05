package outcome

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
)

// sampleIssuance is a complete token issuance.
func sampleIssuance() TokenIssuance {
	return TokenIssuance{
		RunID: "run_abc", CredentialID: "cred_aws",
		KeyID: "Yk1v6dS0fXv3r8Q2m2n0yQZ9Jtq6cZr2dJm4X8nQ0aA", TokenID: "9f86d081884c7d659a2feaa0c55ad015",
		ExpiresAt: time.Date(2026, 10, 1, 9, 15, 0, 0, time.UTC),
	}
}

// TestTokenPath pins the chain path an issuance is recorded under and the refusals of an issuance
// that cannot name what it must.
func TestTokenPath(t *testing.T) {
	t.Parallel()
	child := sampleIssuance()
	child.ParentRunID = "run_parent"
	odd := sampleIssuance()
	odd.CredentialID = "cred/../with slash"
	noKey := sampleIssuance()
	noKey.KeyID = ""
	noRun := sampleIssuance()
	noRun.RunID = ""
	noExpiry := sampleIssuance()
	noExpiry.ExpiresAt = time.Time{}
	tests := []struct {
		In       TokenIssuance
		WantPath string
		Want     error
	}{{ // Test 0: A top-level run.
		In: sampleIssuance(),
		WantPath: "/runs/run_abc/federation/cred_aws/kid/Yk1v6dS0fXv3r8Q2m2n0yQZ9Jtq6cZr2dJm4X8nQ0aA" +
			"/jti/9f86d081884c7d659a2feaa0c55ad015/exp/1790846100",
	}, { // Test 1: A step names the run it belongs to last.
		In: child,
		WantPath: "/runs/run_abc/federation/cred_aws/kid/Yk1v6dS0fXv3r8Q2m2n0yQZ9Jtq6cZr2dJm4X8nQ0aA" +
			"/jti/9f86d081884c7d659a2feaa0c55ad015/exp/1790846100/parent/run_parent",
	}, { // Test 2: An identifier holding a slash is escaped, so it cannot add a segment.
		In: odd,
		WantPath: "/runs/run_abc/federation/cred%2F..%2Fwith%20slash/kid/" +
			"Yk1v6dS0fXv3r8Q2m2n0yQZ9Jtq6cZr2dJm4X8nQ0aA/jti/9f86d081884c7d659a2feaa0c55ad015" +
			"/exp/1790846100",
	}, { // Test 3: No signing key is refused, since naming it is the point.
		In: noKey, Want: ErrIssuance,
	}, { // Test 4: No run is refused.
		In: noRun, Want: ErrIssuance,
	}, { // Test 5: No expiry is refused.
		In: noExpiry, Want: ErrIssuance,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := TokenPath(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("TokenPath() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantPath, got); diff != "" {
				t.Errorf("TokenPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseTokenPath pins that every path TokenPath writes reads back to the issuance it came from,
// and that a near miss is not read as an issuance.
func TestParseTokenPath(t *testing.T) {
	t.Parallel()
	child := sampleIssuance()
	child.ParentRunID = "run_parent"
	odd := sampleIssuance()
	odd.CredentialID = "cred/../with slash"
	tests := []struct {
		Path       string
		WantIssued TokenIssuance
		WantOK     bool
	}{{ // Test 0: A top-level run's issuance.
		Path: mustTokenPath(t, sampleIssuance()), WantIssued: sampleIssuance(), WantOK: true,
	}, { // Test 1: A step's issuance keeps its parent.
		Path: mustTokenPath(t, child), WantIssued: child, WantOK: true,
	}, { // Test 2: An escaped identifier reads back as written.
		Path: mustTokenPath(t, odd), WantIssued: odd, WantOK: true,
	}, { // Test 3: An outcome path is not an issuance.
		Path: "/runs/run_abc/outcome/succeeded", WantOK: false,
	}, { // Test 4: A segment out of place is not an issuance.
		Path: "/runs/run_abc/federation/cred_aws/key/k/jti/j/exp/1", WantOK: false,
	}, { // Test 5: An expiry that is not a number is not an issuance.
		Path: "/runs/run_abc/federation/cred_aws/kid/k/jti/j/exp/soon", WantOK: false,
	}, { // Test 6: A trailing pair other than parent is not an issuance.
		Path: "/runs/run_abc/federation/cred_aws/kid/k/jti/j/exp/1/child/run_x", WantOK: false,
	}, { // Test 7: A form TokenPath would not write, an unescaped space, is not an issuance.
		Path: "/runs/run abc/federation/cred_aws/kid/k/jti/j/exp/1", WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, ok := ParseTokenPath(test.Path)
			if ok != test.WantOK {
				t.Fatalf("ParseTokenPath(%q) ok = %v, want %v", test.Path, ok, test.WantOK)
			}
			if diff := cmp.Diff(test.WantIssued, got); diff != "" {
				t.Errorf("ParseTokenPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// mustTokenPath returns TokenPath of ti, failing the test on an error.
func mustTokenPath(t *testing.T, ti TokenIssuance) string {
	t.Helper()
	p, err := TokenPath(ti)
	if err != nil {
		t.Fatalf("TokenPath() error = %v", err)
	}
	return p
}

// TestCommitTokenIssuance pins the chain entry an issuance commits: the token method, the issuer as
// a system actor on behalf of the launcher, the issuance time, the path, and no content digest,
// since everything it attests is in the path. A refused issuance appends nothing.
func TestCommitTokenIssuance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	ti := sampleIssuance()
	ti.IssuedAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ti.OnBehalfOf = "operator-one"
	if err := CommitTokenIssuance(ctx, audits, ti, "system:federation"); err != nil {
		t.Fatalf("CommitTokenIssuance() error = %v", err)
	}
	bad := ti
	bad.KeyID = ""
	err := CommitTokenIssuance(ctx, audits, bad, "system:federation")
	if !errors.Is(err, ErrIssuance) {
		t.Fatalf("CommitTokenIssuance() of an issuance with no key error = %v, want %v", err,
			ErrIssuance)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("chain holds %d entries, want the one valid issuance", len(chain))
	}
	e := chain[0]
	got := []string{e.Actor, e.ActorType, e.OnBehalfOf, e.Method, e.Path, e.ContentDigest}
	want := []string{"system:federation", "system", "operator-one", audit.MethodToken,
		mustTokenPath(t, ti), ""}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("entry mismatch (-want +got):\n%s", diff)
	}
	if !e.At.Equal(ti.IssuedAt) {
		t.Errorf("entry time = %v, want the issuance time %v", e.At, ti.IssuedAt)
	}
}
