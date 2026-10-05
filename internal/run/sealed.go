package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// SealedDigest binds one sealed secret answer by its ciphertext. The digest is taken over the sealed
// value exactly as stored, never over the answer, so it tells a reader which sealed answer a run
// carries and nothing about what the answer is: the ciphertext is sealed under a key only the server
// holds, with a fresh nonce each time, so no guess at the answer can be checked against it.
//
// It is a list entry rather than a map value because the variable is data here, not a key. A map
// keyed by variable put a digest under a key such as db_password, and every redaction pass in the
// product masks the value under a key that reads as secret, which would have masked the digest out of
// the very spec record that has to commit to it.
type SealedDigest struct {
	// Var is the variable the sealed answer is for.
	Var string `json:"var"`
	// SHA256 is the hex SHA-256 of the sealed answer as stored.
	SHA256 string `json:"sha256"`
}

// SealedDigestsOf returns the digest of each sealed answer, sorted by variable, or nil for none.
func SealedDigestsOf(sealed map[string]string) []SealedDigest {
	if len(sealed) == 0 {
		return nil
	}
	out := make([]SealedDigest, 0, len(sealed))
	for _, name := range SealedVarNames(sealed) {
		out = append(out, SealedDigest{Var: name, SHA256: sealedDigest(sealed[name])})
	}
	return out
}

// sealedDigest returns the hex SHA-256 of one sealed value.
func sealedDigest(sealed string) string {
	sum := sha256.Sum256([]byte(sealed))
	return hex.EncodeToString(sum[:])
}

// RestoreSealed puts a stored run's sealed answers back on it, with the digests fixed when the run
// was created. The digests are taken as stored and never recomputed: a run created before digests
// existed keeps none, so its spec, and every receipt and approval already issued over it, reads as it
// did, and a run whose ciphertext was changed in storage keeps the digest it was created with, which
// is what lets the executor see the change.
func RestoreSealed(r *Run, sealed map[string]string, digests []SealedDigest) {
	if len(sealed) > 0 {
		r.SealedVars = sealed
		r.SealedNames = SealedVarNames(sealed)
	}
	r.SealedDigests = digests
}

// withSealedDigests carries a run's own digests onto a run derived from it, in place of the ones
// WithSealedVars takes over the ciphertext it is handed. A rerun, a relaunch, a shard, and a plan's
// proposed apply execute the answers their source was created with, so they are held to the digests
// that source was bound by: a ciphertext changed in storage after the source ran is caught when the
// derived run executes, where digests taken afresh would have bound the changed one. A source
// created before digests existed has none, and the fresh ones stand.
func withSealedDigests(digests []SealedDigest) SubmitOption {
	return func(r *Run) {
		if len(digests) > 0 {
			r.SealedDigests = slices.Clone(digests)
		}
	}
}

// SealedMatches reports whether sealed is the answer the run's digest for name was fixed over. A run
// created before digests existed has nothing to match and reports true. A run that carries digests
// and none for name reports false, since an answer nothing bound is not one anybody approved.
func (r *Run) SealedMatches(name, sealed string) bool {
	if len(r.SealedDigests) == 0 {
		return true
	}
	for _, d := range r.SealedDigests {
		if d.Var == name {
			return d.SHA256 == sealedDigest(sealed)
		}
	}
	return false
}

// SealedDigestsColumn encodes a run's sealed answer digests for a store column, empty for none.
func SealedDigestsColumn(digests []SealedDigest) string {
	if len(digests) == 0 {
		return ""
	}
	b, err := json.Marshal(digests)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseSealedDigestsColumn decodes stored sealed answer digests. An empty column is a run with no
// sealed answers, or one created before digests existed. A column that does not decode is reported
// rather than read as empty, because the digests are what an approval binds: reading a corrupted list
// as absent would let a run execute sealed answers nothing binds.
func ParseSealedDigestsColumn(s string) ([]SealedDigest, error) {
	if s == "" {
		return nil, nil
	}
	var out []SealedDigest
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse sealed answer digests: %w", err)
	}
	return out, nil
}
