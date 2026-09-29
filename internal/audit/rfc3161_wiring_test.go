package audit

import (
	"context"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestTimestampRefusesAForgedTokenSoNothingIsStored proves the token check is wired into the path
// that talks to a real authority, not only reachable by calling it directly.
//
// The check itself was tested, but nothing exercised its call site, so removing the call left the
// suite green while a forged token was returned as proof and written to the anchor store. An
// operator then read an anchor with a proof attached and believed the chain was fixed in time
// somewhere this install cannot rewrite, and the token attested to a value nobody here ever sent.
//
// Two forgeries are served, both of which a granted status and a non-empty body accept. The first
// commits to a digest of something else. The second commits to the right digest but echoes the
// nonce of some other request, which is what a reply recorded off the wire looks like.
func TestTimestampRefusesAForgedTokenSoNothingIsStored(t *testing.T) {
	t.Parallel()
	chain := buildChain(t, 3)
	head := chain[len(chain)-1]
	raw, err := hex.DecodeString(head.Hash)
	if err != nil {
		t.Fatalf("chain head is not hex: %v", err)
	}
	ours := sha256.Sum256(raw)
	theirs := sha256.Sum256([]byte("a link this install never asked about"))
	// A nonce the request cannot have sent: Timestamp draws 128 random bits every call.
	replayed := big.NewInt(0xBEEF)

	tests := []struct {
		Name    string
		Reply   []byte
		WantErr string
	}{{ // Test 0: A token about a different value, which says nothing about this chain.
		Name:    "commits to another value",
		Reply:   tsaReply(t, tokenOver(t, theirs[:])),
		WantErr: "attests to a different value",
	}, { // Test 1: The right value, answering some other request.
		Name:    "echoes another nonce",
		Reply:   tsaReply(t, tokenOverTail(t, ours[:], derInt(t, replayed))),
		WantErr: "echoes a different nonce",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			srv := tsaServing(t, test.Reply)

			proof, err := Timestamp(ctx, srv.Client(), srv.URL, head.Hash)
			switch {
			case err == nil:
				t.Errorf("a forged token was accepted and handed back as proof: %q", proof)
			case !strings.Contains(err.Error(), test.WantErr):
				t.Errorf("error = %v, want it to mention %q", err, test.WantErr)
			}
			if proof != "" {
				t.Errorf("proof = %q, want empty so a refused token cannot reach an anchor", proof)
			}

			// The caller's path: an anchor is saved only when NewAnchor succeeds, so a refused
			// token must leave the store with nothing in it.
			var store AnchorStore = &memStore{}
			a, err := NewAnchor(ctx, srv.Client(), AnchorRFC3161, srv.URL, AnchorShapeLinear,
				head.Seq, head.Hash, genTime())
			if err == nil {
				if err := store.SaveAnchor(ctx, a); err != nil {
					t.Fatalf("SaveAnchor() error = %v", err)
				}
				t.Errorf("NewAnchor() stored an anchor over a forged token, proof %q", a.Proof)
			}
			stored, err := store.Anchors(ctx, 0)
			if err != nil {
				t.Fatalf("Anchors() error = %v", err)
			}
			if diff := cmp.Diff([]*Anchor(nil), stored, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored anchors (-want +got):\n%s", diff)
			}
			_, results := CheckAnchors(chain, stored, "")
			verified := 0
			for _, r := range results {
				if r.Reached {
					verified++
				}
			}
			if verified != 0 {
				t.Errorf("CheckAnchors reports %d verified anchors over a chain no authority "+
					"vouched for", verified)
			}
		})
	}
}

// tsaReply wraps token in a TimeStampResp with a granted status, which is the shape of an
// authority's answer and everything the reply used to be checked for.
func tsaReply(t *testing.T, token []byte) []byte {
	t.Helper()
	out, err := asn1.Marshal(tsaResponse{
		Status: tsaStatus{Status: 0},
		Token:  asn1.RawValue{FullBytes: token},
	})
	if err != nil {
		t.Fatalf("marshal TimeStampResp: %v", err)
	}
	return out
}

// tsaServing returns a timestamp authority that answers every request with reply.
func tsaServing(t *testing.T, reply []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write(reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}
