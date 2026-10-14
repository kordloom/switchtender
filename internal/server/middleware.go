package server

import (
	"fmt"
	"net/http"
	"path"
	"strings"
)

// maxBodyBytes caps the request body every mutating endpoint accepts, so an oversized POST fails
// with 413 instead of allocating without bound. Import and hook uploads carry whole exports, so
// they get the larger cap that matches their handlers' own readers, and so does the write of an
// inventory, which can hold what an import of one brought in.
const (
	maxBodyBytes       = 1 << 20
	maxUploadBodyBytes = 26 << 20
)

// requestBodyLimit returns the most body bytes r may carry. Both the cap on reading the body and the
// audit gate's digest of it use it, so the two cannot disagree about what a route accepts.
func requestBodyLimit(r *http.Request) int64 {
	if uploadPath(r.URL.Path) || planFilePath(r.URL.Path) || inventoryWrite(r) {
		return maxUploadBodyBytes
	}
	return maxBodyBytes
}

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
			r.Body = http.MaxBytesReader(w, r.Body, requestBodyLimit(r))
		}
		next.ServeHTTP(w, r)
	})
}

// tooLargeMessage says which limit a refused body exceeded, so the answer to an oversized write
// names the number to stay under instead of only saying the body could not be read.
func tooLargeMessage(limit int64) string {
	return fmt.Sprintf("the request body is larger than the %d MiB this route accepts, so the change was "+
		"not recorded or made", limit>>20)
}

// inventoryWrite reports whether r creates or replaces a stored inventory. An imported inventory can
// be larger than the ordinary cap, and it has to stay editable, so these two writes take the same
// cap as an import. Preview, facts and delete keep the ordinary one.
func inventoryWrite(r *http.Request) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return false
	}
	clean := path.Clean("/" + strings.TrimPrefix(strings.ToLower(r.URL.Path), "/"))
	rest, ok := strings.CutPrefix(clean, "/v1/inventories")
	if !ok {
		return false
	}
	if rest == "" {
		return r.Method == http.MethodPost
	}
	id, ok := strings.CutPrefix(rest, "/")
	return ok && r.Method == http.MethodPut && id != "" && !strings.Contains(id, "/")
}

// uploadPath reports whether p is one of the routes allowed a body past the ordinary cap: the import
// endpoints and the inbound webhook hooks. It matches on the cleaned, lowercased path so a "../" or a
// case trick cannot slip an ordinary mutation into the larger cap or past the body digest.
func uploadPath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(strings.ToLower(p), "/"))
	return strings.HasPrefix(clean, "/v1/import/") || strings.HasPrefix(clean, "/hooks/")
}

// planFilePath reports whether p is a relay route a worker hands a saved plan file to, the plan
// gate's proposal or a drift check's plan, which a plan of an ordinary configuration outgrows the
// ordinary cap with. It matches the cleaned, lowercased path exactly, as uploadPath does.
func planFilePath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(strings.ToLower(p), "/"))
	rest, ok := strings.CutPrefix(clean, "/relay/v1/runs/")
	if !ok {
		return false
	}
	id, tail, ok := strings.Cut(rest, "/")
	return ok && id != "" && (tail == "propose-apply" || tail == "drift-plan")
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
