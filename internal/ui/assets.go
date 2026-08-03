package ui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
)

// asset is one embedded static file prepared for serving, with its content type, a content
// derived ETag, and a precomputed gzip body so neither is recomputed per request.
type asset struct {
	// body is the raw file bytes.
	body []byte
	// gzipped is the gzip encoded body, nil when compression did not shrink the file.
	gzipped []byte
	// contentType is the MIME type sent with the file.
	contentType string
	// etag is the quoted content hash, used for conditional revalidation.
	etag string
}

// assetHandler serves the embedded UI assets with content based ETags and precomputed gzip. The
// bytes never change for a given binary, so a browser revalidates with If-None-Match and gets a
// 304 instead of re-downloading the whole bundle on every page. A new build changes the content
// and therefore the ETag, so an upgrade is never served stale.
type assetHandler struct {
	// assets maps a request path, such as app.js, to its prepared file.
	assets map[string]asset
}

// newAssetHandler prepares every file in the embedded assets subtree for serving. It panics on a
// read failure, which is a build time error since the tree is embedded.
func newAssetHandler(assets fs.FS) *assetHandler {
	h := &assetHandler{assets: make(map[string]asset)}
	err := fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			// mime consults OS tables at runtime, and minimal images ship without a woff2 entry.
			switch path.Ext(p) {
			case ".woff2":
				ct = "font/woff2"
			default:
				ct = "application/octet-stream"
			}
		}
		sum := sha256.Sum256(body)
		h.assets[p] = asset{
			body:        body,
			gzipped:     gzipBody(body),
			contentType: ct,
			etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		}
		return nil
	})
	if err != nil {
		panic("ui: prepare assets: " + err.Error())
	}
	h.assembleAppJS()
	return h
}

// assembleAppJS builds the served app.js by concatenating the source files under js/ in name
// order. The application script is written as many files so a change touches one focused file,
// but it ships as the single script the templates already load. Order is part of the program:
// constants must initialize before the code below them runs, so the numeric prefixes on the
// source files are load-bearing and concatenation preserves them.
//
// Each part is separated by a newline rather than butted directly against the next. Concatenating
// raw bytes meant a part whose last line had no terminator glued two statements into one, which is
// a syntax error for most pairs and a silently different program for the rest. Nothing enforced
// that terminator, the failure would have landed in the shipped bundle rather than in a source
// file, and the node test loader joins the same parts with a newline of its own, so the suite would
// have gone on passing while the served script did not parse.
//
// Nothing under js/ is served on its own. Removing only the .js parts left any other file in that
// directory reachable at its own URL while never appearing in app.js, so a note, a backup copy, or
// a snippet parked beside the sources shipped to anyone who asked for it by name.
func (h *assetHandler) assembleAppJS() {
	var names, extras []string
	for p := range h.assets {
		switch {
		case !strings.HasPrefix(p, "js/"):
		case strings.HasSuffix(p, ".js"):
			names = append(names, p)
		default:
			extras = append(extras, p)
		}
	}
	if len(names) == 0 {
		panic("ui: no js/ source parts embedded for app.js")
	}
	sort.Strings(names)
	var body []byte
	for _, p := range names {
		body = append(body, h.assets[p].body...)
		if len(body) > 0 && body[len(body)-1] != '\n' {
			body = append(body, '\n')
		}
		delete(h.assets, p)
	}
	for _, p := range extras {
		delete(h.assets, p)
	}
	sum := sha256.Sum256(body)
	h.assets["app.js"] = asset{
		body:        body,
		gzipped:     gzipBody(body),
		contentType: mime.TypeByExtension(".js"),
		etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
	}
}

// gzipBody returns the gzip encoded form of body, or nil when compression does not shrink it or
// the file is too small to be worth compressing.
func gzipBody(body []byte) []byte {
	if len(body) < 1024 {
		return nil
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(body); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(body) {
		return nil
	}
	return bytes.Clone(buf.Bytes())
}

// ServeHTTP serves the prepared asset for the request path, honoring If-None-Match for a 304 and
// serving the gzip body when the client accepts it.
func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, ok := h.assets[strings.TrimPrefix(r.URL.Path, "/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Type", a.contentType)
	header.Set("Cache-Control", "no-cache")
	header.Set("ETag", a.etag)
	header.Add("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if a.gzipped != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		header.Set("Content-Encoding", "gzip")
		_, _ = w.Write(a.gzipped)
		return
	}
	_, _ = w.Write(a.body)
}
