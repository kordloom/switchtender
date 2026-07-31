package relay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kordloom/switchtender/internal/relay"
)

// TestDownRelayIsNotAskedPerWrite pins that a relay refusing posts is retried on a backoff rather
// than asked once for every chunk of output a run produces.
//
// A batch that reaches the size threshold posts immediately, and a failed post puts its bytes back,
// which leaves the buffer over the threshold. Every subsequent append was therefore a size-triggered
// flush: with the relay down, a hundred small writes became a hundred synchronous failing requests,
// each paying a connection attempt on the path the run's own output travels.
func TestDownRelayIsNotAskedPerWrite(t *testing.T) {
	t.Parallel()
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			posts.Add(1)
			// The relay is up but refusing, which is what a restarting control node looks like.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	transport := relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client())
	ctx := context.Background()

	// Fill the batch past its size threshold so every following write is a size-triggered flush.
	big := make([]byte, 64<<10)
	if err := transport.AppendLog(ctx, "run_1", big); err != nil {
		t.Fatalf("AppendLog(big) error = %v", err)
	}
	afterFill := posts.Load()

	// Now write a hundred small chunks, the shape a chatty tool produces.
	const writes = 100
	for i := 0; i < writes; i++ {
		if err := transport.AppendLog(ctx, "run_1", []byte("a line of output\n")); err != nil {
			t.Fatalf("AppendLog(%d) error = %v", i, err)
		}
	}
	extra := posts.Load() - afterFill

	// Without a backoff this is one post per write. With one, the writes land inside a single
	// backoff window and cost nothing. A small allowance covers a timer firing mid-loop.
	if extra > 5 {
		t.Errorf("%d posts for %d writes against a down relay, want the writes to coalesce behind "+
			"a retry backoff instead of each paying its own failing request", extra, writes)
	}
	t.Logf("posts: %d to fill, %d more across %d writes", afterFill, extra, writes)
}

// TestRelayRecoversAfterBackoff pins that output buffered while the relay was down is delivered once
// it comes back, so the backoff delays posts rather than dropping them.
func TestRelayRecoversAfterBackoff(t *testing.T) {
	t.Parallel()
	var down atomic.Bool
	var delivered atomic.Int64
	down.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") && down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			delivered.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	transport := relay.NewHTTPTransport(srv.URL, "worker-token", srv.Client())
	ctx := context.Background()
	if err := transport.AppendLog(ctx, "run_1", make([]byte, 64<<10)); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	down.Store(false)

	// A terminal save flushes whatever is buffered, which is the path that must not be held off by
	// a backoff opened while the relay was down.
	if err := transport.AppendLog(ctx, "run_1", []byte("tail\n")); err != nil {
		t.Fatalf("AppendLog(tail) error = %v", err)
	}
	flusher, ok := transport.(interface {
		FlushLogForTest(context.Context, string) error
	})
	if !ok {
		t.Fatal("the HTTP transport does not expose its flush to tests")
	}
	if err := flusher.FlushLogForTest(ctx, "run_1"); err != nil {
		t.Fatalf("flush after recovery error = %v", err)
	}
	if delivered.Load() == 0 {
		t.Error("nothing was delivered after the relay came back, so the buffered output was lost")
	}
}
