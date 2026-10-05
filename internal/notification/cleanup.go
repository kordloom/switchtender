package notification

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// Cleanup summarizes the attachments a delete removed together with the object they were attached
// to: how many, and which targets they named. The targets themselves are never removed by it.
type Cleanup struct {
	// Removed is how many attachments were removed.
	Removed int `json:"removed"`
	// Targets are the distinct targets those attachments named, sorted.
	Targets []string `json:"targets,omitempty"`
}

// Summarize builds the summary of removing attachments that named the given targets, one id per
// attachment removed.
func Summarize(targetIDs []string) Cleanup {
	targets := slices.Clone(targetIDs)
	slices.Sort(targets)
	return Cleanup{Removed: len(targetIDs), Targets: slices.Compact(targets)}
}

// Equal reports whether two summaries describe the same removal.
func (c Cleanup) Equal(o Cleanup) bool {
	return c.Removed == o.Removed && slices.Equal(c.Targets, o.Targets)
}

// cleanupKeys are the query keys a delete's chain entry carries its cleanup under, in order.
const (
	// cleanupCountKey carries how many attachments the delete removed.
	cleanupCountKey = "notification_attachments"
	// cleanupTargetsKey carries the targets they named.
	cleanupTargetsKey = "notification_targets"
)

// PathSuffix renders the summary the way a delete's own chain entry carries it, appended to the
// path it records: "?notification_attachments=2&notification_targets=ntf_a,ntf_b". It is empty when
// the delete removes nothing, so a delete with no attachments records exactly the path it always
// did. The chain link commits to the path, so the summary is committed with the delete itself
// rather than written as separate events. The form is fixed, a count and a list of ids that need no
// escaping, so ParsePathSuffix can read it back exactly.
func (c Cleanup) PathSuffix() string {
	if c.Removed == 0 {
		return ""
	}
	return "?" + cleanupCountKey + "=" + strconv.Itoa(c.Removed) + "&" + cleanupTargetsKey + "=" +
		strings.Join(c.Targets, ",")
}

// ParsePathSuffix reads the cleanup a delete's chain entry path carries, reporting false for a path
// that carries none or one that does not round-trip through PathSuffix exactly.
func ParsePathSuffix(path string) (Cleanup, bool) {
	_, query, found := strings.Cut(path, "?")
	if !found {
		return Cleanup{}, false
	}
	count, rest, ok := strings.Cut(query, "&")
	if !ok {
		return Cleanup{}, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(count, cleanupCountKey+"="))
	if err != nil || n <= 0 || !strings.HasPrefix(count, cleanupCountKey+"=") {
		return Cleanup{}, false
	}
	list, ok := strings.CutPrefix(rest, cleanupTargetsKey+"=")
	if !ok || list == "" {
		return Cleanup{}, false
	}
	c := Cleanup{Removed: n, Targets: strings.Split(list, ",")}
	if "?"+query != c.PathSuffix() {
		return Cleanup{}, false
	}
	return c, true
}

// RecordedDeleter deletes an attachable object together with every attachment on it in one
// transaction, the way a store's ordinary Delete does, and refuses with ErrCleanupChanged, deleting
// nothing, when the attachments it would remove are not the ones recorded. A delete's chain entry
// is written before the delete runs and states what the delete removes, so an attachment made or
// removed in between would otherwise leave the entry saying something that did not happen.
type RecordedDeleter interface {
	// DeleteRecorded removes the object with the given id and its attachments, provided they match
	// recorded.
	DeleteRecorded(ctx context.Context, id string, recorded Cleanup) error
}
