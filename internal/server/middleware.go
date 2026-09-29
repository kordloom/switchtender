package server

import (
	"net/http"
	"strings"
)

// maxBodyBytes caps the request body every mutating endpoint accepts, so an oversized POST fails
// with 413 instead of allocating without bound. Import and hook uploads carry whole exports, so
// they get the larger cap that matches their handlers' own readers.
const (
	maxBodyBytes       = 1 << 20
	maxUploadBodyBytes = 26 << 20
)

// bodyLimit wraps mutating requests in a MaxBytesReader before any handler decodes them. The
// login endpoint is reached unauthenticated, so this cap is the only thing between a crafted
// multi-gigabyte body and the process allocating it.
func bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			limit := int64(maxBodyBytes)
			if strings.HasPrefix(r.URL.Path, "/v1/import/") || strings.HasPrefix(r.URL.Path, "/hooks/") {
				limit = maxUploadBodyBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders stamps every response with the browser hardening headers: no MIME sniffing, no
// framing, a same-origin content security policy, and a conservative referrer policy. The web
// interface loads one first-party script and stylesheet, so the policy costs it nothing.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// base-uri and form-action do not fall back to default-src, so they are stated. Without
		// base-uri an injected <base> element repoints every relative script and fetch at another
		// origin while still satisfying script-src 'self'; without form-action a form can post
		// wherever it likes.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; "+
				"object-src 'none'")
		next.ServeHTTP(w, r)
	})
}
