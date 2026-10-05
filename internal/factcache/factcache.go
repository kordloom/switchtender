// Package factcache keeps the facts Ansible gathers for each host of a stored inventory, so a later
// run of a template with the fact cache on can read them without gathering again. It follows the
// AWX model: before a run the cached facts are written into a private directory that Ansible's
// jsonfile cache plugin reads, and after the run the files Ansible rewrote are read back and
// stored.
//
// A fact document routinely carries the remote user's environment and other values nobody meant to
// publish, so it is held apart from everything evidential. It never enters the audit chain, a
// receipt, a dossier, or a backup, and it is bounded per host by MaxFactsBytes.
package factcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxFactsBytes is the largest fact document kept for one host. A full gather on an ordinary
// server is tens to a few hundred kilobytes, so this leaves room for a large one while stopping a
// play that sets an enormous fact from filling the database one host at a time.
const MaxFactsBytes = 1 << 20

// maxHostName bounds a host name, which becomes a file name in the cache directory.
const maxHostName = 255

// Entry is one host's cached facts in one stored inventory.
type Entry struct {
	// InventoryID is the stored inventory the host belongs to.
	InventoryID string `json:"inventory_id"`
	// Host is the inventory host name, the name Ansible keys its cache by.
	Host string `json:"host"`
	// Facts is the fact document as Ansible wrote it, a JSON object. It is empty in a listing that
	// asked for the summary only.
	Facts json.RawMessage `json:"facts,omitempty"`
	// Bytes is the size of the fact document.
	Bytes int `json:"bytes"`
	// RunID is the run that gathered the facts. Who may read the facts follows who may read it.
	RunID string `json:"run_id,omitempty"`
	// ModifiedAt is when the facts were gathered, which the fact cache timeout is measured from.
	ModifiedAt time.Time `json:"modified_at"`
}

// Store persists cached facts. Implementations must be safe for concurrent use.
type Store interface {
	// SaveFacts writes each entry, keyed by inventory and host, unless the facts stored for the host
	// were gathered after the entry's, so a run that gathered earlier and finished later cannot write
	// its older facts over newer ones. An entry stamped the same instant as the stored one replaces
	// it. A store that keeps the inventories beside the facts writes nothing for an inventory that is
	// no longer stored, and that is not an error: deleting the inventory took its facts with it, and
	// a run that outlived it has nothing to add. Every entry is checked with Validate first and
	// nothing is written when one fails.
	SaveFacts(ctx context.Context, entries []Entry) error
	// Facts returns one host's entry with its fact document, or ErrNotFound.
	Facts(ctx context.Context, inventoryID, host string) (*Entry, error)
	// List returns an inventory's entries ordered by host. The fact documents are included only
	// when withFacts is set, so a listing of a large inventory stays small.
	List(ctx context.Context, inventoryID string, withFacts bool) ([]Entry, error)
	// Clear removes the named hosts' facts from an inventory and reports how many were removed.
	Clear(ctx context.Context, inventoryID string, hosts ...string) (int, error)
	// ClearInventory removes every host's facts from an inventory and reports how many were
	// removed. It runs when the inventory itself is deleted.
	ClearInventory(ctx context.Context, inventoryID string) (int, error)
}

// ValidHost reports whether name can be a cache file name: not empty, not too long, valid UTF-8,
// and free of anything that would let it name a path outside the cache directory. Ansible writes
// each host to a file named after it, so a host name is a path component here.
func ValidHost(name string) bool {
	if name == "" || len(name) > maxHostName || !utf8.ValidString(name) {
		return false
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

// Validate reports why an entry cannot be stored, or nil when it can.
func Validate(e Entry) error {
	if e.InventoryID == "" {
		return fmt.Errorf("%w: no inventory", ErrInvalid)
	}
	if !ValidHost(e.Host) {
		return fmt.Errorf("%w: host %q cannot be a cache file name", ErrInvalid, e.Host)
	}
	if len(e.Facts) > MaxFactsBytes {
		return fmt.Errorf("%w: %s has %d bytes of facts, more than %d", ErrTooLarge, e.Host,
			len(e.Facts), MaxFactsBytes)
	}
	// Both database backends store the document as text, and PostgreSQL refuses invalid UTF-8.
	if !utf8.Valid(e.Facts) || !isObject(e.Facts) {
		return fmt.Errorf("%w: facts for %s are not a JSON object", ErrInvalid, e.Host)
	}
	return nil
}

// isObject reports whether raw is a single valid JSON object.
func isObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	return json.Valid(trimmed)
}

// Fresh reports whether facts modified at modified are still fresh enough to serve at now under a
// timeout. A timeout of zero or less never expires them, which is the AWX default.
func Fresh(modified, now time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		return true
	}
	return !modified.Before(now.Add(-timeout))
}
