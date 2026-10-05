package server

import (
	"net/http"
	"strconv"

	"go.uber.org/zap"
)

// maxReferenceBytes bounds an id a request names for another object, such as the account a
// membership adds or the object a grant covers. These are stored under an index, and PostgreSQL
// refuses an index entry past about 2.7 kilobytes, so a longer value that did not compress was
// answered 500 instead of being refused. An id this server mints is a prefix and a few dozen hex
// digits, and a queue grant's object is bounded by the queue name's own limit, so the bound refuses
// only a value no object of this install can carry.
const maxReferenceBytes = 512

// refuseLongReference answers 400, naming field and the limit, when value is longer than
// maxReferenceBytes, and reports whether it did.
func refuseLongReference(w http.ResponseWriter, log *zap.Logger, field, value string) bool {
	if len(value) <= maxReferenceBytes {
		return false
	}
	respondError(w, log, http.StatusBadRequest, field+" may be at most "+
		strconv.Itoa(maxReferenceBytes)+" bytes, and this one is "+strconv.Itoa(len(value)))
	return true
}
