package audit_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/audit"
)

// signAt is a fixed time so signed exports are deterministic under test.
var signAt = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// TestNewSigner covers the seed cases: empty is off, valid works, malformed or wrong-length fails.
func TestNewSigner(t *testing.T) {
	t.Parallel()
	if s, err := audit.NewSigner(""); s != nil || err != nil {
		t.Errorf("empty seed: signer=%v err=%v, want nil,nil", s, err)
	}
	if _, err := audit.NewSigner("zz"); err == nil {
		t.Error("bad hex seed: want an error")
	}
	if _, err := audit.NewSigner(strings.Repeat("a", 10)); err == nil {
		t.Error("short seed: want an error")
	}
	if s, err := audit.NewSigner(strings.Repeat("a", 64)); err != nil || s == nil {
		t.Errorf("valid seed: signer=%v err=%v", s, err)
	}
}

// TestBuildAndVerifyExport covers a signed export verifying and an unsigned export proving integrity
// but reporting no signature.
func TestBuildAndVerifyExport(t *testing.T) {
	t.Parallel()
	signer, err := audit.NewSigner(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}

	exp := audit.BuildExport(buildChain(3), signer, signAt)
	if exp.Algo != audit.SignAlgo || exp.PublicKey != signer.PublicKeyHex() {
		t.Errorf("export meta = %+v, want algo %s and the signer public key", exp, audit.SignAlgo)
	}
	if signed, err := audit.VerifyExport(exp); err != nil || !signed {
		t.Fatalf("signed export: signed=%v err=%v, want true,nil", signed, err)
	}

	un := audit.BuildExport(buildChain(2), nil, signAt)
	if signed, err := audit.VerifyExport(un); err != nil || signed {
		t.Fatalf("unsigned export: signed=%v err=%v, want false,nil", signed, err)
	}
}

// TestVerifyExportJSONRoundTrip confirms an export verifies after a JSON round-trip, the path the
// offline CLI takes, so entry times survive serialization.
func TestVerifyExportJSONRoundTrip(t *testing.T) {
	t.Parallel()
	signer, _ := audit.NewSigner(strings.Repeat("b", 64))
	data, err := json.Marshal(audit.BuildExport(buildChain(4), signer, signAt))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got audit.Export
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if signed, err := audit.VerifyExport(&got); err != nil || !signed {
		t.Fatalf("round-trip verify: signed=%v err=%v, want true,nil", signed, err)
	}
}

// TestVerifyExportDetectsTamper covers an edited entry, a flipped signature, and a swapped key.
func TestVerifyExportDetectsTamper(t *testing.T) {
	t.Parallel()
	signer, _ := audit.NewSigner(strings.Repeat("c", 64))

	// Test 0: Editing an entry breaks the chain before the signature is even checked.
	tampered := audit.BuildExport(buildChain(3), signer, signAt)
	tampered.Entries[1].Actor = "mallory"
	if _, err := audit.VerifyExport(tampered); !errors.Is(err, audit.ErrChainBroken) {
		t.Errorf("edited entry: err = %v, want ErrChainBroken", err)
	}

	// Test 1: A flipped signature is rejected.
	badSig := audit.BuildExport(buildChain(3), signer, signAt)
	badSig.Signature = flipLast(badSig.Signature)
	if _, err := audit.VerifyExport(badSig); !errors.Is(err, audit.ErrBadSignature) {
		t.Errorf("flipped signature: err = %v, want ErrBadSignature", err)
	}

	// Test 2: A signature that claims a different public key is rejected.
	other, _ := audit.NewSigner(strings.Repeat("d", 64))
	wrongKey := audit.BuildExport(buildChain(3), signer, signAt)
	wrongKey.PublicKey = other.PublicKeyHex()
	if _, err := audit.VerifyExport(wrongKey); !errors.Is(err, audit.ErrBadSignature) {
		t.Errorf("swapped key: err = %v, want ErrBadSignature", err)
	}
}

// flipLast returns s with its last hex digit changed, keeping it valid hex.
func flipLast(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[len(b)-1] == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	return string(b)
}
