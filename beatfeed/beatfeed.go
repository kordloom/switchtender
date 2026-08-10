// Package beatfeed is the span beat feed contract: the wire shape of one beat, the path the feed is
// served at, and the query parameter that bounds a request. A SwitchTender server produces this
// feed and an outside witness consumes it, so the two must agree on it exactly. Holding the
// contract in one dependency-free package is what keeps the producer, the consumer, and the auth
// carve-out that leaves the route open from drifting apart, and it lets an out-of-tree witness
// build against the same definition the server serves.
package beatfeed

// FeedPath is the feed's path below the versioned API prefix. It is what the server's auth carve-out
// matches after the prefix is trimmed, so a stranger with no account can still read the feed.
const FeedPath = "/audit/beats"

// APIPath is the full request path the feed is served at, prefix included. An outside watcher
// fetches <base>+APIPath.
const APIPath = "/v1" + FeedPath

// LimitParam is the query parameter that bounds how many beats one request returns.
const LimitParam = "limit"

// Beat is one span beat as the feed serves it. A witness reads the stream to tell a quiet chain
// from one whose tail was removed: a missing, duplicated, or rewritten beat is the signal.
type Beat struct {
	// Beat is the beat number, starting at one and rising by exactly one per beat.
	Beat int64 `json:"beat"`
	// At is when the beat was appended, RFC 3339.
	At string `json:"at"`
	// Seq is the beat entry's position in the audit chain.
	Seq int64 `json:"seq"`
	// Head is the beat entry's own chain hash, the head the beat attests.
	Head string `json:"head"`
}
