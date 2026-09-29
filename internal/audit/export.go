package audit

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// SignAlgo names the signature algorithm a signed export uses.
const SignAlgo = "ed25519"

// Signer signs an audit chain head with ed25519 so a signed export can be verified offline, without
// trusting the server that produced it.
type Signer struct {
	// priv is the ed25519 private key.
	priv ed25519.PrivateKey
}

// NewSigner returns a Signer from a hex-encoded 32-byte seed. An empty seed returns a nil Signer, so
// signing is simply off; a malformed seed is an error so a misconfigured key is not ignored silently.
func NewSigner(hexSeed string) (*Signer, error) {
	if hexSeed == "" {
		return nil, nil
	}
	seed, err := hex.DecodeString(hexSeed)
	if err != nil {
		return nil, fmt.Errorf("audit signer: decode seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("audit signer: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Signer{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// PublicKeyHex returns the hex-encoded public key an auditor pins to verify an export.
func (s *Signer) PublicKeyHex() string {
	return hex.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

// Export is a portable, self-verifying snapshot of the audit chain. An auditor recomputes the chain
// from Entries, confirms it ends at HeadHash, and checks Signature against PublicKey, all offline.
type Export struct {
	// Entries is the full chain, oldest first.
	Entries []*Entry `json:"entries"`
	// Count is the number of entries.
	Count int `json:"count"`
	// HeadHash is the last entry's hash, which transitively commits to the whole chain.
	HeadHash string `json:"head_hash"`
	// Algo names the signature algorithm, empty when the export is unsigned.
	Algo string `json:"algo,omitempty"`
	// PublicKey is the hex ed25519 key that verifies Signature, empty when unsigned.
	PublicKey string `json:"public_key,omitempty"`
	// Signature is the hex ed25519 signature over the signed message, empty when unsigned.
	Signature string `json:"signature,omitempty"`
	// SignedAt is the RFC3339 time the export was signed, empty when unsigned.
	SignedAt string `json:"signed_at,omitempty"`
}

// exportMessage is the canonical bytes an export signs and verifies: the algorithm tag, entry count,
// head hash, and signing time, so a signature pins this exact chain length and head at this moment.
func exportMessage(count int, headHash, signedAt string) []byte {
	return []byte("switchtender-audit\n" + strconv.Itoa(count) + "\n" + headHash + "\n" + signedAt)
}

// BuildExport packages entries into an Export, signing the head with signer when it is not nil. The
// caller passes now so the signature time is explicit and tests are deterministic.
func BuildExport(entries []*Entry, signer *Signer, now time.Time) *Export {
	exp := &Export{Entries: entries, Count: len(entries)}
	if n := len(entries); n > 0 {
		exp.HeadHash = entries[n-1].Hash
	}
	if signer != nil {
		exp.Algo = SignAlgo
		exp.PublicKey = signer.PublicKeyHex()
		exp.SignedAt = now.UTC().Format(time.RFC3339Nano)
		sig := ed25519.Sign(signer.priv, exportMessage(exp.Count, exp.HeadHash, exp.SignedAt))
		exp.Signature = hex.EncodeToString(sig)
	}
	return exp
}

// Export verification errors.
var (
	// ErrChainBroken means an entry's hash or link did not check out.
	ErrChainBroken = errors.New("audit chain broken")
	// ErrHeadMismatch means the recomputed head does not match the export's head hash.
	ErrHeadMismatch = errors.New("audit head hash does not match the chain")
	// ErrBadSignature means the signature did not verify against the public key.
	ErrBadSignature = errors.New("audit signature invalid")
)

// VerifyExport checks that an export is sound: the chain verifies, it ends at HeadHash, and, when
// signed, the signature checks out against the embedded public key. It returns whether the export
// carried a valid signature and an error describing the first failure. A caller establishes trust by
// comparing the embedded public key against one it pins out of band.
func VerifyExport(exp *Export) (signed bool, err error) {
	if ok, at := Verify(exp.Entries); !ok {
		return false, fmt.Errorf("%w at entry %d", ErrChainBroken, at)
	}
	head := ""
	if n := len(exp.Entries); n > 0 {
		head = exp.Entries[n-1].Hash
	}
	if head != exp.HeadHash {
		return false, ErrHeadMismatch
	}
	if exp.Signature == "" {
		return false, nil
	}
	pub, err := hex.DecodeString(exp.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("%w: public key", ErrBadSignature)
	}
	sig, err := hex.DecodeString(exp.Signature)
	if err != nil {
		return false, fmt.Errorf("%w: signature encoding", ErrBadSignature)
	}
	if !ed25519.Verify(pub, exportMessage(exp.Count, exp.HeadHash, exp.SignedAt), sig) {
		return false, ErrBadSignature
	}
	return true, nil
}
