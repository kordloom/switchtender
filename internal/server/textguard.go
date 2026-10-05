package server

import (
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/util"
)

// textGuardNameCap bounds how much of a query parameter's name a refusal echoes back.
const textGuardNameCap = 80

// requestTextGuard refuses a request whose path or query holds a NUL byte or bytes that are not
// valid UTF-8, before anything routes it.
//
// A percent escape lets a caller put any byte into a path segment or a query value, and both reach
// the store as text. SQLite keeps the byte and answers not found. PostgreSQL refuses the value with
// SQLSTATE 22021, so the same request was a 404 on one backend and a 500 on the other, and the 500
// read as a fault in the server. No identifier, name, or filter this API reads can hold either, so
// the request is malformed, and it is answered that way before a handler, the relay routes, or a
// store sees it.
func requestTextGuard(log *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if where := unstorableAddress(r); where != "" {
			respondError(w, log, http.StatusBadRequest,
				where+" holds a NUL byte or text that is not valid UTF-8")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// unstorableAddress names the part of r's path or query that holds text no store keeps, and
// returns the empty string when every part is storable. The query is read the way a handler reads
// it, so a pair the parser drops is not held against the request.
func unstorableAddress(r *http.Request) string {
	if !util.IsSafeText(r.URL.Path) {
		return "the request path"
	}
	for name, values := range r.URL.Query() {
		if !util.IsSafeText(name) {
			return "a query parameter name"
		}
		for _, v := range values {
			if !util.IsSafeText(v) {
				return "the query parameter " + strconv.Quote(util.Clip(name, textGuardNameCap))
			}
		}
	}
	return ""
}
