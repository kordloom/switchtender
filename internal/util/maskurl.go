package util

import (
	"net/url"
	"sort"
	"strings"
)

// MaskMarker is the ellipsis a masked value carries in place of the part that was redacted. A field
// holding it is a redaction coming back, not an address a caller chose.
const MaskMarker = "…"

// MaskURL redacts a URL that is itself a bearer secret, keeping only the scheme and host. A Slack,
// Discord, Teams, Mattermost, or plain webhook address is the credential: anyone holding it can post
// to the channel. A Twilio endpoint carries the account SID in its path and a Splunk HEC endpoint
// often ends in the token, so the path and query go too. A value that does not parse or names no
// host is redacted whole, since nothing about it is known to be safe to show.
func MaskURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return MaskMarker
	}
	return u.Scheme + "://" + u.Host + "/" + MaskMarker
}

// MaskURLError returns err with every secret-bearing URL redacted out of its message, and nil for a
// nil error. Masking the field a URL is logged in is not enough: net/http wraps a transport failure
// in a *url.Error that re-embeds the whole address in its own message, so the error text printed
// beside a masked field still carries the credential. Every *url.Error in the chain contributes its
// address, and extra addresses the caller already knows about are masked too, covering a wrapper
// that quoted the URL without being a *url.Error. The original error stays in the returned error's
// chain, so errors.Is and errors.As still match what actually failed.
func MaskURLError(err error, urls ...string) error {
	if err == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var targets []string
	add := func(raw string) {
		if raw == "" {
			return
		}
		if _, ok := seen[raw]; ok {
			return
		}
		seen[raw] = struct{}{}
		targets = append(targets, raw)
	}
	for _, raw := range urls {
		add(raw)
	}
	collectURLs(err, add)
	// Longest first, so a URL that is a prefix of another is not masked out from under it, leaving
	// the longer one's tail in the message.
	sort.Slice(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })

	original := err.Error()
	msg := original
	for _, raw := range targets {
		msg = strings.ReplaceAll(msg, raw, MaskURL(raw))
	}
	if msg == original {
		return err
	}
	return &maskedError{msg: msg, err: err}
}

// collectURLs walks err's chain, following a joined error into every branch, and hands add the
// address each *url.Error carries.
func collectURLs(err error, add func(string)) {
	for err != nil {
		if ue, ok := err.(*url.Error); ok {
			add(ue.URL)
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, branch := range u.Unwrap() {
				collectURLs(branch, add)
			}
			return
		default:
			return
		}
	}
}

// maskedError is an error reporting a redacted message while the error it stands for remains in the
// chain, so errors.Is and errors.As still match the failure that happened.
type maskedError struct {
	// msg is the redacted message.
	msg string
	// err is the original error, reachable only by unwrapping.
	err error
}

// Error returns the redacted message.
func (e *maskedError) Error() string { return e.msg }

// Unwrap returns the error whose message was redacted.
func (e *maskedError) Unwrap() error { return e.err }
