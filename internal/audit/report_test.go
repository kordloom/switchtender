package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// reportAt is a fixed render time so reports are deterministic under test.
var reportAt = time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)

// newSigner returns a signer from a fixed seed for report tests.
func newSigner(t *testing.T) *audit.Signer {
	t.Helper()
	s, err := audit.NewSigner(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	return s
}

// TestReportVerified proves a sound signed export renders a verified report carrying the head hash and
// the signing key an auditor pins.
func TestReportVerified(t *testing.T) {
	t.Parallel()
	signer := newSigner(t)
	exp := audit.BuildExport(buildChain(3), signer, signAt)
	html, err := audit.Report(exp, reportAt)
	if err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	s := string(html)
	for _, want := range []string{
		`class="banner verified"`,
		"signature verified",
		exp.HeadHash,
		signer.PublicKeyHex(),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("verified report missing %q", want)
		}
	}
}

// TestReportUnsigned proves an unsigned but intact export renders as integrity-proven, attribution-not.
func TestReportUnsigned(t *testing.T) {
	t.Parallel()
	exp := audit.BuildExport(buildChain(2), nil, signAt)
	html, err := audit.Report(exp, reportAt)
	if err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	s := string(html)
	if !strings.Contains(s, `class="banner unsigned"`) {
		t.Error("unsigned report missing the unsigned banner")
	}
	if !strings.Contains(s, "attribution is not") {
		t.Error("unsigned report should state attribution is not proven")
	}
}

// TestReportTampered proves an export whose chain was altered renders as broken and not to be trusted.
func TestReportTampered(t *testing.T) {
	t.Parallel()
	exp := audit.BuildExport(buildChain(3), newSigner(t), signAt)
	// Alter a middle entry so its stored hash no longer matches the recomputed one.
	exp.Entries[1].Path = "/tampered"
	html, err := audit.Report(exp, reportAt)
	if err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	s := string(html)
	if !strings.Contains(s, `class="banner broken"`) {
		t.Error("tampered report missing the broken banner")
	}
	if !strings.Contains(s, "must not be trusted") {
		t.Error("tampered report should warn the export must not be trusted")
	}
}

// TestReportSignatureInvalid proves an intact chain with a bad signature renders as signature-invalid,
// distinct from a broken chain.
func TestReportSignatureInvalid(t *testing.T) {
	t.Parallel()
	exp := audit.BuildExport(buildChain(3), newSigner(t), signAt)
	// Keep the chain intact but replace the signature with a valid-length, wrong value.
	exp.Signature = strings.Repeat("0", len(exp.Signature))
	html, err := audit.Report(exp, reportAt)
	if err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	if !strings.Contains(string(html), `class="banner signature-invalid"`) {
		t.Error("report with a bad signature missing the signature-invalid banner")
	}
}
