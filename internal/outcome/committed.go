package outcome

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/audit"
)

// MaxDisclosedBytes is the largest redacted outcome record an entry commits and a receipt discloses
// whole. A larger one is committed and disclosed as its summary, so a receipt stays a document a
// small install can build and a reader can open, whatever the size of the fleet behind it.
const MaxDisclosedBytes = 1 << 20

// summaryMembers are the record's members a summary keeps beside the full record's size and digest:
// what ran, how it ended, and what was approved, so a receipt for an oversized run still answers
// those questions and still binds the spec it discloses.
var summaryMembers = []string{"run_id", "status", "exit_code", "spec_digest"}

// Oversize names the full record a summary stands in for.
type Oversize struct {
	// Limit is the size, in bytes, above which a record is summarized, recorded with the summary so
	// the rule it was made under travels with it.
	Limit int `json:"limit"`
	// Size is the length of the full redacted record in bytes.
	Size int `json:"size"`
	// SHA256 is the hex SHA-256 of the full redacted record.
	SHA256 string `json:"sha256"`
}

// Committed returns the bytes an outcome entry commits under the exact digest form and a receipt
// discloses: the record redacted and canonicalized once, before its bytes are fixed, so the bytes
// committed are the bytes shown and checking them needs none of this product's redaction rules.
//
// A record whose redacted form exceeds MaxDisclosedBytes is represented by its summary: the run,
// its status, exit code, and spec digest, and the full redacted record's size and SHA-256, under an
// oversize member that also records the limit. The summary is what is committed and disclosed, so
// the rule is the same for every size: what the chain fixed is what a reader holds.
//
// The older form digested the record as it was handed over and reduced it again at every check, and
// skipped the reduction entirely above 1 MiB, so its input could only be rebuilt by this product
// and the receipt disclosed a record the redaction had never read.
func Committed(body []byte) ([]byte, error) {
	reduced, err := audit.CanonicalRedacted(body)
	if err != nil {
		return nil, err
	}
	if len(reduced) <= MaxDisclosedBytes {
		return reduced, nil
	}
	sum := sha256.Sum256(reduced)
	summary := map[string]any{"oversize": map[string]any{
		"limit": int64(MaxDisclosedBytes), "size": int64(len(reduced)),
		"sha256": hex.EncodeToString(sum[:]),
	}}
	if tree, perr := jcs.Parse(reduced); perr == nil {
		if obj, ok := tree.(map[string]any); ok {
			for _, key := range summaryMembers {
				if v, ok := obj[key]; ok {
					summary[key] = v
				}
			}
		}
	}
	return jcs.Serialize(summary)
}

// Disclosed returns the bytes a receipt discloses for an outcome entry whose digest is digest,
// rebuilt from body, the record as Body assembles it today. An entry under the exact form committed
// Committed(body), so those are the bytes. An entry under an older form committed the record as it
// was handed over, which its digest reduces at every check, so the record itself is disclosed.
func Disclosed(digest string, body []byte) ([]byte, error) {
	if strings.HasPrefix(digest, audit.ExactDigestPrefix) {
		return Committed(body)
	}
	return body, nil
}

// VerifyBody reports whether body, the record as Body rebuilds it, is the outcome an entry with
// digest and nonce committed, under whichever form the entry used.
func VerifyBody(digest, nonce string, body []byte) bool {
	disclosed, err := Disclosed(digest, body)
	return err == nil && audit.VerifyContentDigest(digest, nonce, disclosed)
}
