package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/jsonutil"
)

// errorResponse is the JSON body returned for error responses.
type errorResponse struct {
	// Error is a human-readable failure message.
	Error string `json:"error"`
}

// wantsPretty reports whether the request asked for indented JSON via the pretty query parameter.
func wantsPretty(r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("pretty") {
		return false
	}
	return q.Get("pretty") != "false"
}

// respondJSON writes v as JSON with the given status code.
func respondJSON(w http.ResponseWriter, log *zap.Logger, status int, v any, pretty bool) {
	body, err := jsonutil.Marshal(v, pretty)
	if err != nil {
		log.Error("server: marshal response: " + err.Error())
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Error("server: write response: " + err.Error())
	}
}

// respondError writes an errorResponse with the given status code.
func respondError(w http.ResponseWriter, log *zap.Logger, status int, message string) {
	respondJSON(w, log, status, errorResponse{Error: message}, false)
}

// gzipMinSize is the smallest body worth compressing. Below it the gzip header and trailer eat most
// of the saving, and a reply that already fits in one packet gets no faster for being smaller.
const gzipMinSize = 1024

// gzipWriters pools compressors so a compressed reply does not allocate a fresh deflate window,
// which is the largest allocation on the path by a wide margin.
var gzipWriters = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// compress wraps next with content-negotiated gzip for the JSON API and the rendered pages, which
// were the only large responses leaving the process uncompressed: the static assets carry their own
// precomputed gzip, and a run list or an audit page is mostly repeated field names.
//
// Three things are left alone. A client that did not ask for gzip gets exactly what it did before.
// The live stream is never wrapped at all, because compressing a stream means holding events until
// there is enough of them to be worth a block, which is the one thing a stream must not do. The
// prepared assets are skipped because they are already compressed and would otherwise be encoded
// twice.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStream(r) || strings.HasPrefix(r.URL.Path, "/ui/assets/") || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Vary says the body depends on what the client asked for, without which a shared cache
		// hands a compressed reply to a client that cannot read one.
		w.Header().Add("Vary", "Accept-Encoding")
		gz := &gzipResponse{ResponseWriter: w}
		defer gz.finish()
		next.ServeHTTP(gz, r)
	})
}

// acceptsGzip reports whether the client asked for gzip, honoring an explicit refusal by weight.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		weight := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(params), "q="))
		return weight != "0" && weight != "0.0" && weight != "0.00" && weight != "0.000"
	}
	return false
}

// gzipResponse compresses a response once it grows past gzipMinSize and passes a smaller one
// through untouched, which is why the body is held rather than encoded as it arrives: the size is
// not known until the handler is done, and the decision cannot be taken back once the header is on
// the wire. The status code and every header the handler set, the audit receipt above all, are sent
// exactly as they were given.
type gzipResponse struct {
	// ResponseWriter is the writer underneath, which receives the encoded body.
	http.ResponseWriter
	// zw compresses the body, nil until the response is known to be worth compressing.
	zw *gzip.Writer
	// held is the body so far, waiting on the size that decides how it is sent.
	held []byte
	// status is the code the handler set, zero until it sets one.
	status int
	// sent records that the header has gone out, after which the encoding is settled.
	sent bool
}

// WriteHeader records the status without sending it, since the encoding is not decided until the
// body is either big enough to compress or finished.
func (g *gzipResponse) WriteHeader(status int) {
	if g.sent || g.status != 0 {
		return
	}
	g.status = status
}

// Write holds the body until its size decides the encoding, then writes through the chosen path.
func (g *gzipResponse) Write(p []byte) (int, error) {
	if g.zw != nil {
		return g.zw.Write(p)
	}
	if g.sent {
		return g.ResponseWriter.Write(p)
	}
	g.held = append(g.held, p...)
	if len(g.held) < gzipMinSize {
		return len(p), nil
	}
	header := g.Header()
	if header.Get("Content-Encoding") != "" {
		// The handler encoded the body itself, so it is not this middleware's to encode again.
		g.passThrough()
		return len(p), nil
	}
	// The type is sniffed from the body before it is compressed. Left to net/http it would be
	// sniffed from the gzip stream instead, and every page in the product would arrive labeled as
	// an archive.
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", http.DetectContentType(g.held))
	}
	header.Set("Content-Encoding", "gzip")
	// The handler counted the bytes it wrote, which is not the number about to go out.
	header.Del("Content-Length")
	g.send()
	zw, ok := gzipWriters.Get().(*gzip.Writer)
	if !ok {
		zw = gzip.NewWriter(io.Discard)
	}
	zw.Reset(g.ResponseWriter)
	g.zw = zw
	held := g.held
	g.held = nil
	if _, err := zw.Write(held); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush passes a flush through and stops holding anything back, so a handler that flushes keeps
// streaming. Waiting for a kilobyte before releasing what a handler explicitly pushed out is a
// stall, and a stalled stream is worse than an uncompressed one.
func (g *gzipResponse) Flush() {
	if g.zw != nil {
		_ = g.zw.Flush()
	} else {
		g.passThrough()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the writer underneath, so an http.ResponseController reaches the real connection.
func (g *gzipResponse) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// send writes the recorded status through, once.
func (g *gzipResponse) send() {
	if g.sent {
		return
	}
	g.sent = true
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(status)
}

// passThrough gives up on compressing and releases what is held exactly as the handler wrote it.
func (g *gzipResponse) passThrough() {
	g.send()
	if len(g.held) > 0 {
		_, _ = g.ResponseWriter.Write(g.held)
		g.held = nil
	}
}

// finish completes the response after the handler returns: it closes the compressor, or releases a
// body that never grew large enough to be worth compressing.
func (g *gzipResponse) finish() {
	if g.zw != nil {
		_ = g.zw.Close()
		gzipWriters.Put(g.zw)
		g.zw = nil
		return
	}
	g.passThrough()
}
