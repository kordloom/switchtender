package server

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
)

// withheldMarker stands in for a decision's free text in the body the gate digests. It is fixed, so
// the request entry still commits that a reason was sent, and it is not a value anybody types.
const withheldMarker = "«withheld: committed by the decision entry»"

// reasonFields are the body fields that carry a person's free text on a decision route: the reason
// as typed, the masked form being confirmed, and a correction's text.
var reasonFields = []string{"reason", "masked_reason", "text", "masked_text"}

// decisionTextRoute reports whether a request is one whose body carries an approver's free text: an
// approval, a rejection, or a correction.
func decisionTextRoute(p string) bool {
	clean := strings.TrimPrefix(path.Clean("/"+p), "/v1")
	if !strings.HasPrefix(clean, "/runs/") {
		return false
	}
	return strings.HasSuffix(clean, "/approve") || strings.HasSuffix(clean, "/reject") ||
		strings.HasSuffix(clean, "/corrections")
}

// withholdReasonText returns the bytes the gate digests for a decision route's body: the body with
// each free-text field replaced by a fixed marker.
//
// The request-body fingerprint is a keyed commitment whose key is stored beside the entry, so
// anyone holding the database can confirm a guess of what it committed. That is the right shape for
// a change's configuration and the wrong one for a sentence a person typed, which may hold personal
// data and must be removable. The text is committed once, masked, by the decision entry, under a
// random value that is deleted when the reason is redacted. The original, unmasked text is never
// committed anywhere. A body that is not a single JSON object is refused by the handler, and its
// bytes are digested as the marker alone rather than committed raw.
func withholdReasonText(body []byte) []byte {
	var doc map[string]any
	if !json.Valid(body) || strictDecode(bytes.NewReader(body), &doc) != nil {
		return []byte(withheldMarker)
	}
	for _, field := range reasonFields {
		if v, ok := doc[field]; ok {
			if s, isString := v.(string); isString && s != "" {
				doc[field] = withheldMarker
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return []byte(withheldMarker)
	}
	return out
}
