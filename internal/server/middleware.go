package server

import (
	"net/http"
	"path"
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
		case http.MethodGet, http.MethodHead:
			// A read carries no body to cap.
		default:
			// Every mutating method is capped, not only POST, PUT, and PATCH. A DELETE or any other
			// method left uncapped is a body the audit gate then buffers whole to digest it, which
			// an anonymous caller could turn into an out-of-memory crash.
			limit := int64(maxBodyBytes)
			if uploadPath(r.URL.Path) {
				limit = maxUploadBodyBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// uploadPath reports whether p is one of the routes allowed a body past the ordinary cap: the import
// endpoints and the inbound webhook hooks. It matches on the cleaned, lowercased path so a "../" or a
// case trick cannot slip an ordinary mutation into the larger cap or past the body digest.
func uploadPath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(strings.ToLower(p), "/"))
	return strings.HasPrefix(clean, "/v1/import/") || strings.HasPrefix(clean, "/hooks/")
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
		// Pin the browser to HTTPS once it has arrived over it. An operator who reaches an install on
		// a hostile network over plain HTTP once hands the sign-in POST, and with it a bearer token
		// good for thirty days, to anyone on the path; with this, every visit after the first is
		// immune. It is set only on a TLS request, so an install behind a plaintext loopback proxy or
		// one still being set up is not locked out of a scheme it does not serve.
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
