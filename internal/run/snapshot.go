package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// InventorySnapshot describes the stored inventory a run executes against, materialized once when
// the run was submitted. The run executes this snapshot and never what the store holds when it is
// claimed, so an inventory edited after an approval cannot widen or redirect the approved run.
//
// The content itself is sealed on the run, because a host list can carry an ansible_password or an
// API token. What this record holds is never secret: the digest of the sealed content as stored, a
// digest of the content with its secret values masked, and the hosts it names. The first is what
// binds the run to its snapshot, the way a sealed survey answer is bound by its ciphertext, so no
// guess at a secret in the inventory can be checked against anything disclosed. The second lets a
// reader holding the inventory confirm which one it was without disclosing anything the inventory
// API does not already show.
type InventorySnapshot struct {
	// SealedSHA256 is the hex SHA-256 of the sealed content exactly as stored on the run.
	SealedSHA256 string `json:"sealed_sha256"`
	// ContentSHA256 is the hex SHA-256 of the content with every value the inventory redactor treats
	// as secret replaced by a fixed marker.
	ContentSHA256 string `json:"content_sha256"`
	// Hosts are the hosts the content names, sorted, when it resolves without Ansible. Empty for a
	// dynamic source, whose hosts exist only once Ansible resolves it.
	Hosts []string `json:"hosts,omitempty"`
	// Dynamic reports that the content is a definition Ansible resolves when the run executes, such as
	// an inventory plugin configuration, so the hosts it reaches are known only then.
	Dynamic bool `json:"dynamic,omitempty"`
	// DynamicReason says why the hosts resolve at execution, empty for a static inventory.
	DynamicReason string `json:"dynamic_reason,omitempty"`
	// CredentialIDs are the credentials the inventory attached when the run was submitted. Every run
	// against an inventory receives them, so they are part of what an approval covers, and a
	// credential attached to the inventory afterward does not reach a run approved before it.
	CredentialIDs []string `json:"credential_ids,omitempty"`
}

// Clone returns a deep copy, or nil for nil.
func (s *InventorySnapshot) Clone() *InventorySnapshot {
	if s == nil {
		return nil
	}
	out := *s
	out.Hosts = slices.Clone(s.Hosts)
	out.CredentialIDs = slices.Clone(s.CredentialIDs)
	return &out
}

// SnapshotColumn encodes an inventory snapshot record for a store column, empty for none.
func SnapshotColumn(s *InventorySnapshot) string {
	if s == nil {
		return ""
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseSnapshotColumn decodes a stored inventory snapshot record. An empty column is a run that
// targets no stored inventory, or one created before snapshots existed. A column
// that does not decode is reported rather than read as empty, because the record is what an approval
// binds: reading a corrupted one as absent would let a run execute a snapshot nothing binds.
func ParseSnapshotColumn(s string) (*InventorySnapshot, error) {
	if s == "" {
		return nil, nil
	}
	var out InventorySnapshot
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse inventory snapshot: %w", err)
	}
	return &out, nil
}

// SealedBlobSHA256 returns the hex SHA-256 of a sealed value exactly as stored. It is the digest an
// approval binds a sealed inventory snapshot or a sealed plan file by: it says which sealed value a
// run carries and nothing about what the value holds, because the value is sealed under a key only
// the server holds, with a fresh nonce each time.
func SealedBlobSHA256(sealed string) string {
	sum := sha256.Sum256([]byte(sealed))
	return hex.EncodeToString(sum[:])
}

// SnapshotMatches reports whether sealed is the snapshot the run was bound to when it was submitted.
// A run with no snapshot record has nothing to match and reports false, since a snapshot nothing
// bound is not one anybody approved.
func (r *Run) SnapshotMatches(sealed string) bool {
	if r.InventorySnapshot == nil || sealed == "" {
		return false
	}
	return r.InventorySnapshot.SealedSHA256 == SealedBlobSHA256(sealed)
}

// PlanMatches reports whether sealed is the plan file the run's approval was bound to. A run that
// names no plan digest has nothing to match and reports false.
func (r *Run) PlanMatches(sealed string) bool {
	if r.PlanSHA256 == "" || sealed == "" {
		return false
	}
	return r.PlanSHA256 == SealedBlobSHA256(sealed)
}

// WithInventorySnapshot carries a source run's inventory snapshot onto a run derived from it, the
// apply a plan proposes, so the derived run executes the inventory its source was submitted with
// rather than taking a snapshot of its own. A composed inventory's snapshot travels with the
// resolution it was taken from, res, which is nil for any other inventory. Both the record and the
// sealed content must be present, or the derived run takes its own snapshot when it is submitted.
func WithInventorySnapshot(snap *InventorySnapshot, sealed string, res *InventoryResolution) SubmitOption {
	return func(r *Run) {
		if snap != nil && sealed != "" {
			r.InventorySnapshot, r.InventorySealed = snap.Clone(), sealed
			r.InventoryResolution = res.Clone()
		}
	}
}

// WithPlanFile binds a proposed apply to the sealed plan file it carries out, recording the sealed
// file and the digest an approval binds.
func WithPlanFile(sealed string) SubmitOption {
	return func(r *Run) {
		if sealed != "" {
			r.PlanSealed, r.PlanSHA256 = sealed, SealedBlobSHA256(sealed)
		}
	}
}
