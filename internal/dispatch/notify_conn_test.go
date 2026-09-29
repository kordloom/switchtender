package dispatch

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// connCounter records the distinct client connections a notification receiver was asked to serve,
// which is what says whether the controller reused one socket or opened a new one per delivery.
type connCounter struct {
	// mu guards seen.
	mu sync.Mutex
	// seen holds every distinct client address the receiver was reached from.
	seen map[string]struct{}
}

// record notes one request's client address.
func (c *connCounter) record(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]struct{}{}
	}
	c.seen[addr] = struct{}{}
}

// count returns how many distinct connections were used.
func (c *connCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

// TestNotificationDeliveriesReuseOneConnection pins that a run's notifications ride an existing
// connection rather than dialing a new one each time.
//
// A notification receiver answers with a body, and net/http only returns a connection to the pool
// once that body has been read out. Closing it unread makes the transport tear the connection down,
// so a controller notifying on every finished run paid a fresh TCP and TLS handshake per run, per
// channel, forever. This asks a receiver how many connections it was actually served over: one
// delivery may legitimately need one, but twenty must not need twenty.
func TestNotificationDeliveriesReuseOneConnection(t *testing.T) {
	t.Parallel()
	conns := &connCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns.record(req.RemoteAddr)
		w.WriteHeader(http.StatusOK)
		// A real receiver answers with something. An empty response would let an undrained body
		// through, which is the bug this test exists to catch.
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	t.Cleanup(srv.Close)

	d := notifyDispatcher(srv.Client())
	const deliveries = 20
	for i := 0; i < deliveries; i++ {
		d.deliver(srv.URL, "run_reuse", []byte(`{}`))
	}

	if got := conns.count(); got != 1 {
		t.Errorf("connections used = %d, want 1: %d deliveries must share one socket", got, deliveries)
	}
}

// TestNotifyClientIsSharedAcrossDeliveries pins that the guarded default client is built once.
//
// It used to be built per delivery, which gave every notification its own http.Transport. A
// transport dropped without being closed keeps the connection it dialed idle for its idle timeout,
// along with the read and write goroutines serving it, so a hundred deliveries left three hundred
// goroutines and a hundred sockets alive. A controller that notifies on every finished run
// accumulates those for as long as runs keep finishing, which is what kills it in a week.
func TestNotifyClientIsSharedAcrossDeliveries(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{log: zap.NewNop()}
	first := d.notifyClient()
	if first == nil {
		t.Fatal("notifyClient() = nil, want the guarded default")
	}
	for i := 0; i < 10; i++ {
		if got := d.notifyClient(); got != first {
			t.Fatalf("notifyClient() call %d returned a new client, want the shared one", i)
		}
	}
}

// TestNotifyClientKeepsAnInjectedClient pins that a caller-supplied client is not replaced by the
// guarded default, which is how a test reaches a receiver on loopback.
func TestNotifyClientKeepsAnInjectedClient(t *testing.T) {
	t.Parallel()
	injected := &http.Client{Timeout: time.Second}
	d := notifyDispatcher(injected)
	if got := d.notifyClient(); got != injected {
		t.Errorf("notifyClient() = %p, want the injected client %p", got, injected)
	}
}

// BenchmarkNotifyClient pins the cost of reaching the delivery client. Building the guarded default
// per delivery cost 366 ns and five allocations totalling 1104 bytes, on top of the transport it
// then threw away; sharing it makes the lookup a field read.
func BenchmarkNotifyClient(b *testing.B) {
	d := &Dispatcher{log: zap.NewNop()}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = d.notifyClient()
	}
}
