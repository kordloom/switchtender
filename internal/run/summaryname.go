package run

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/util"
)

// MaxSummaryNameBytes is the longest host or task name the fleet tables store as it was given.
//
// Every fleet table indexes the name it is keyed by: the host and task summaries by name and time,
// gathered facts by host. PostgreSQL refuses an index entry longer than about 2700 bytes, and the
// names are somebody else's, from an imported or dynamic inventory or from a playbook, so one
// pathological name failed the whole summary write on PostgreSQL while SQLite kept it. The run
// finished and was missing from fleet health, drift, host history, and task trends on one backend
// only. A host name stops at 253 bytes and a task name is a line of text, so this sits far above
// any name in use and far enough below the index limit that the other key columns fit beside it.
const MaxSummaryNameBytes = 1024

// summaryDigestMark separates the kept start of a cut name from the digest of the whole name.
const summaryDigestMark = "... sha256:"

// SummaryName returns the name the fleet tables store a host or task under: the name made safe for
// a text column, and when that is longer than MaxSummaryNameBytes, the longest start of it that
// leaves room for the SHA-256 digest of the whole name, followed by that digest.
//
// A name at or under the limit comes back unchanged, so no host or task already in history is
// renamed. A longer one keeps a readable start, and two long names that share it are still stored
// apart, because their digests differ. Passing a stored name back through it changes nothing, so a
// lookup by a name the fleet views displayed finds the same rows.
func SummaryName(name string) string {
	name = util.SafeText(name)
	if len(name) <= MaxSummaryNameBytes {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := summaryDigestMark + hex.EncodeToString(sum[:])
	cut := MaxSummaryNameBytes - len(suffix)
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + suffix
}

// SummaryNameKeys returns the two values a lookup by a host or task name matches in the fleet
// tables. The first is the name made safe for a text column, which is how a row written before long
// names were cut holds it, since SQLite kept such names whole and is not rewritten. The second is
// SummaryName's form, which is how every row written since holds it. They differ only for a name
// longer than MaxSummaryNameBytes.
func SummaryNameKeys(name string) (whole, stored string) {
	whole = util.SafeText(name)
	return whole, SummaryName(whole)
}
