package cmd

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kordloom/switchtender/internal/imageref"
)

// TestTheServeResolverRefusesThisServerItself covers the client the server asks registries through.
// A run's image is its submitter's choice, so a reference that points at the loopback interface must
// open no connection there, and the submitter must learn nothing from the refusal: the error is the
// one a registry that is down gets, and the reason stays with the server's log.
func TestTheServeResolverRefusesThisServerItself(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var dialed atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dialed.Add(1)
			_ = conn.Close()
		}
	}()

	const wantText = "image tag not resolved to a digest: the registry did not answer"
	ref := listener.Addr().String() + "/acme/ee:1"
	_, err = newImageResolver().Digest(context.Background(), ref, imageref.Credentials{})
	if !errors.Is(err, imageref.ErrResolve) {
		t.Fatalf("Digest() error = %v, want %v", err, imageref.ErrResolve)
	}
	if err.Error() != wantText {
		t.Errorf("Digest() error text = %q, want the fixed %q", err.Error(), wantText)
	}
	if detail := imageref.NoAnswerDetail(err); !strings.Contains(detail, "this server itself") {
		t.Errorf("NoAnswerDetail() = %q, want the refused dial named for the log", detail)
	}
	if got := dialed.Load(); got != 0 {
		t.Errorf("the lookup opened %d connections to the loopback interface, want none", got)
	}
}
