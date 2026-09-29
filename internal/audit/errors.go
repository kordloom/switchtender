package audit

import "errors"

// ErrReservedSpan is returned by Append when an entry carries the span beat marker. The marker is
// how every reader of the chain recognizes a beat, so only AppendSpanBeat may write it; an entry
// that merely arrived wearing it is a forgery attempt, not a beat.
var ErrReservedSpan = errors.New("span marker reserved for AppendSpanBeat")
