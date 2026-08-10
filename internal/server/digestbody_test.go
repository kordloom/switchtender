package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// TestDigestBodyBoundsAndSkips proves the audit body digest cannot be turned into a memory-exhaustion
// vector and does not double-buffer the large upload paths. An oversized body on an ordinary route is
// refused rather than read whole, an import or hook upload passes through undigested so it is buffered
// once by the handler and not again by the gate, and an ordinary small body is digested and restored.
func TestDigestBodyBoundsAndSkips(t *testing.T) {
	t.Parallel()
	g := &authGate{log: zap.NewNop()}
	big := bytes.Repeat([]byte("a"), maxBodyBytes+4096)

	// An over-limit body on a normal mutating route is refused with 413, not buffered whole.
	del := httptest.NewRequest(http.MethodDelete, "/v1/credentials/cred_1", bytes.NewReader(big))
	dw := httptest.NewRecorder()
	if _, ok := g.digestBody(dw, del); ok {
		t.Error("digestBody accepted an over-limit DELETE body; a stranger could exhaust memory")
	}
	if dw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-limit DELETE status = %d, want 413", dw.Code)
	}

	// An upload path is passed through undigested, so its body still reaches the handler intact.
	up := httptest.NewRequest(http.MethodPost, "/v1/import/awx", bytes.NewReader(big))
	uw := httptest.NewRecorder()
	digest, ok := g.digestBody(uw, up)
	if !ok || digest != "" {
		t.Errorf("upload path: digest=%q ok=%v, want an empty digest and ok", digest, ok)
	}
	if body, _ := io.ReadAll(up.Body); len(body) != len(big) {
		t.Errorf("upload body was consumed by the gate: %d of %d bytes left for the handler", len(body), len(big))
	}

	// An ordinary small body is digested and restored for the handler to read.
	small := []byte(`{"password":"x","name":"web"}`)
	put := httptest.NewRequest(http.MethodPut, "/v1/credentials/cred_1", bytes.NewReader(small))
	pw := httptest.NewRecorder()
	d, ok := g.digestBody(pw, put)
	if !ok || d == "" {
		t.Errorf("small body: digest=%q ok=%v, want a digest and ok", d, ok)
	}
	if restored, _ := io.ReadAll(put.Body); !bytes.Equal(restored, small) {
		t.Error("small body was not restored for the handler after digesting")
	}
}
