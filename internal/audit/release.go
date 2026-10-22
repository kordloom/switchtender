package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// releaseIDHexLen is how many hex digits of the SHA-256 a release entry's id keeps after its
// prefix: 128 bits, far past any collision a chain will meet.
const releaseIDHexLen = 32

// AdoptedPath is the chain path of an install's adoption record, the one entry that says when the
// rows an earlier binary left were sealed under the install's binding key and by whom. It is
// recorded once per install, under the id the key fixes.
const AdoptedPath = outcomeRunsPrefix + "adopted"

// ReleasePath returns the chain path of the RELEASE entry for one step of a workflow. It names the
// workflow first, so everything that collects a run's entries by its id collects these too, and the
// step record second.
func ReleasePath(parentID, childID string) string {
	return outcomeRunsPrefix + parentID + "/steps/" + childID + "/release"
}

// ParseReleasePath reads a path written by ReleasePath. It reports ok=false for anything that does
// not round-trip exactly, including a step decision's path and either id holding a slash.
func ParseReleasePath(path string) (parentID, childID string, ok bool) {
	rest, found := strings.CutPrefix(path, outcomeRunsPrefix)
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] != "steps" || parts[2] == "" ||
		parts[3] != "release" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// ReleaseID returns the fixed id of the RELEASE entry for one attempt of one workflow step: "aud_"
// and the first 32 hex digits of the SHA-256 over the sorted root ids, the parent id, the step
// index, and the attempt, joined by NUL bytes. Every launcher of the same attempt computes the same
// id from the same decisions, so the chain refuses the second append with ErrDuplicateID and the
// attempt is released once. The order the roots arrive in does not matter, and rootIDs is not
// modified.
func ReleaseID(rootIDs []string, parentID string, index, attempt int) string {
	parts := slices.Sorted(slices.Values(rootIDs))
	parts = append(parts, parentID, strconv.Itoa(index), strconv.Itoa(attempt))
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "aud_" + hex.EncodeToString(sum[:])[:releaseIDHexLen]
}

// StartPath returns the chain path of the START entry for one attempt of a run. It names the run
// first, so everything that collects a run's entries by its id collects this one too.
func StartPath(runID string, attempt int) string {
	return outcomeRunsPrefix + runID + "/start/" + strconv.Itoa(attempt)
}

// StartID returns the fixed id of the START entry for one attempt of a run: "aud_" and the first
// 32 hex digits of the SHA-256 over the run id and the attempt joined by a NUL byte. Every start of
// the same attempt computes the same id, so the chain refuses the second append with
// ErrDuplicateID and the attempt runs once.
func StartID(runID string, attempt int) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + strconv.Itoa(attempt)))
	return "aud_" + hex.EncodeToString(sum[:])[:releaseIDHexLen]
}
