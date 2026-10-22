package witnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/spanbeat"
	"github.com/kordloom/switchtender/witness"
)

// testEpoch is when every test witness's key begins signing, well before any test runs.
var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testTransport carries every test request. It keeps connections alive and holds enough idle per
// server that the suite reuses a few connections rather than opening one per request, so -count=300
// does not run the host out of ephemeral ports.
var testTransport = &http.Transport{MaxIdleConnsPerHost: 64}

// testHTTPClient returns the client the tests' witness clients send through.
func testHTTPClient() *http.Client {
	return &http.Client{Transport: testTransport, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// The ways a test witness can be down.
const (
	// witnessUp serves every request.
	witnessUp int32 = iota
	// witnessBadGateway answers every request 502, as a tunnel in front of a stopped service does.
	witnessBadGateway
	// witnessReset drops every connection without an answer.
	witnessReset
)

// testClock is a clock a test moves by hand.
type testClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the time the clock reads.
	now time.Time
}

// Now reads the clock.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Add moves the clock forward by d.
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// handlerBox holds the handler a test witness serves, so a restart can swap it under a running
// server whose address the enrollment names.
type handlerBox struct {
	// h is the service's handler.
	h http.Handler
}

// testWitness is a push witness from the witness package, served over HTTP at an address that
// survives restarts of the service behind it.
type testWitness struct {
	// id is the witness identity when the witness has no keyring.
	id identity.Identity
	// keyDir is the witness's key directory when it runs from a keyring, empty otherwise.
	keyDir string
	// dir is the state directory.
	dir string
	// opts are the service options given on top of the defaults.
	opts []witness.ServiceOption
	// svc is the running service.
	svc *witness.Service
	// handler is what the server serves.
	handler atomic.Pointer[handlerBox]
	// down says whether and how the witness is down.
	down atomic.Int32
	// requests counts every request that reached the server.
	requests atomic.Int64
	// pushes counts every push that reached the server.
	pushes atomic.Int64
	// url is the server's base URL.
	url string
}

// newTestWitness starts a push witness in open policy and serves it.
func newTestWitness(t *testing.T, opts ...witness.ServiceOption) *testWitness {
	t.Helper()
	id, err := identity.LoadWitnessFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessFile() error = %v", err)
	}
	return serveTestWitness(t, &testWitness{id: id, dir: t.TempDir(), opts: opts})
}

// newKeyringWitness starts a push witness in open policy that signs from a keyring, so a test can
// rotate its key.
func newKeyringWitness(t *testing.T) *testWitness {
	t.Helper()
	return serveTestWitness(t, &testWitness{keyDir: t.TempDir(), dir: t.TempDir()})
}

// serveTestWitness starts w's service and serves it.
func serveTestWitness(t *testing.T, w *testWitness) *testWitness {
	t.Helper()
	w.start(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(w.serve))
	// The server closes with SO_LINGER 0, so its shutdown leaves no TIME_WAIT behind.
	srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if tcp, ok := c.(*net.TCPConn); ok && state == http.StateNew {
			_ = tcp.SetLinger(0)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	w.url = srv.URL
	return w
}

// start runs a service over the witness's state directory, as a fresh process would.
func (w *testWitness) start(t *testing.T) {
	t.Helper()
	id := w.id
	base := []witness.ServiceOption{witness.WithAcceptEnrollments(witness.DefaultMaxInstalls),
		witness.WithEnrollmentPolicy(witness.OpenEnrollment)}
	if w.keyDir != "" {
		k, err := witness.LoadKeyring(w.keyDir, w.dir)
		if err != nil {
			t.Fatalf("LoadKeyring() error = %v", err)
		}
		id = k.Current()
		base = append(base, witness.WithKeyring(k))
	} else {
		base = append(base, witness.WithWitnessKeys([]witness.WitnessKey{
			witness.WitnessKeyOf(id, testEpoch)}))
	}
	svc, err := witness.NewService(id, w.dir, time.Minute, nil, nil, nil, append(base, w.opts...)...)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w.svc = svc
	w.handler.Store(&handlerBox{h: svc.Handler()})
}

// restart stops the service, runs between with it stopped, and starts another over the same state
// directory behind the same address.
func (w *testWitness) restart(t *testing.T, between func()) {
	t.Helper()
	w.svc.Close()
	if between != nil {
		between()
	}
	w.start(t)
}

// serve answers one request as the witness, or as a witness that is down.
func (w *testWitness) serve(rw http.ResponseWriter, r *http.Request) {
	w.requests.Add(1)
	if strings.HasSuffix(r.URL.Path, "/beats") {
		w.pushes.Add(1)
	}
	switch w.down.Load() {
	case witnessBadGateway:
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadGateway)
		_, _ = rw.Write([]byte(`{"error":"bad gateway"}`))
		return
	case witnessReset:
		if conn, _, err := http.NewResponseController(rw).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	w.handler.Load().h.ServeHTTP(rw, r)
}

// statement returns the witness's signed statement about the install whose producer key is keyID.
func (w *testWitness) statement(t *testing.T, keyID string) *witness.Statement {
	t.Helper()
	st, err := w.svc.AttestInstall(keyID)
	if err != nil || st == nil {
		t.Fatalf("AttestInstall() = %v, %v, want a statement", st, err)
	}
	if _, err := witness.VerifyStatement(st); err != nil {
		t.Fatalf("VerifyStatement() error = %v", err)
	}
	return st
}

// installFindings is the witness's findings route for one install.
type installFindings struct {
	// Findings are the recorded findings, both classes.
	Findings []witness.RecordedFinding `json:"findings"`
	// RefusalsTotal counts refused pushes.
	RefusalsTotal int64 `json:"refusals_total"`
	// NotesTotal counts notes.
	NotesTotal int64 `json:"notes_total"`
}

// findings reads the witness's recorded findings about the install whose producer key is keyID.
func (w *testWitness) findings(t *testing.T, keyID string) installFindings {
	t.Helper()
	res, err := testHTTPClient().Get(w.url + "/v1/installs/" + keyID + "/findings")
	if err != nil {
		t.Fatalf("GET findings error = %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var out installFindings
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	return out
}

// kinds returns how many recorded findings have each kind.
func (f installFindings) kinds() map[string]int {
	out := map[string]int{}
	for _, rec := range f.Findings {
		out[rec.Kind]++
	}
	return out
}

// testInstall is a producer: its signing identity, its audit chain, and where its enrollment lives.
type testInstall struct {
	// id signs the enrollment and every push.
	id identity.Identity
	// store is the audit chain, which also keeps the attestations.
	store audit.Store
	// path is the enrollment file.
	path string
	// beats are every beat appended, in order.
	beats []spanbeat.AppendedBeat
}

// newTestInstall returns an install with an empty chain bound to a fresh identity.
func newTestInstall(t *testing.T) *testInstall {
	t.Helper()
	id, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	return newInstallFor(t, id)
}

// newInstallFor returns an install with an empty chain bound to id, such as a second replica of
// one identity or the same key over a rewritten chain.
func newInstallFor(t *testing.T, id identity.Identity) *testInstall {
	t.Helper()
	store := audit.NewMemStore()
	if b, ok := store.(audit.InstallBinder); ok {
		b.BindInstall(id.InstallID)
	}
	return &testInstall{id: id, store: store,
		path: filepath.Join(t.TempDir(), EnrollmentFile)}
}

// attestations returns the install's attestation store.
func (in *testInstall) attestations(t *testing.T) audit.AttestationStore {
	t.Helper()
	atts, ok := in.store.(audit.AttestationStore)
	if !ok {
		t.Fatal("the memory store keeps no attestations")
	}
	return atts
}

// entries appends n ordinary entries to the chain.
func (in *testInstall) entries(t *testing.T, n int) {
	t.Helper()
	for range n {
		e := &audit.Entry{ID: audit.NewID(), Actor: "operator", Method: http.MethodPost,
			Path: "/v1/templates"}
		if err := in.store.Append(context.Background(), e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
}

// beat appends the next span beat and returns it as the emitter hands it to an anchor.
func (in *testInstall) beat(t *testing.T) spanbeat.AppendedBeat {
	t.Helper()
	for range 1000 {
		e, err := in.store.AppendSpanBeat(context.Background(), time.Time{}, 60)
		if errors.Is(err, audit.ErrClockBehind) {
			// The clock has not passed the last beat at its resolution yet.
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("AppendSpanBeat() error = %v", err)
		}
		n, _, _, ok := audit.ParseSpanPath(e.Path)
		if !ok {
			t.Fatalf("the appended beat's path %q does not parse", e.Path)
		}
		b := spanbeat.AppendedBeat{At: e.At, Seq: e.Seq, Hash: e.Hash, Beat: n}
		in.beats = append(in.beats, b)
		return b
	}
	t.Fatal("the clock never passed the last beat")
	return spanbeat.AppendedBeat{}
}

// enroll enrolls the install with w and writes its enrollment file.
func (in *testInstall) enroll(t *testing.T, w *testWitness) *Enrollment {
	t.Helper()
	e, err := Enroll(context.Background(), testHTTPClient(), w.url, in.id, nil, time.Now())
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	if err := e.Save(in.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return e
}

// client returns a client pushing under the install's enrollment at the given cadence, logging to
// log, or nowhere when log is nil.
func (in *testInstall) client(t *testing.T, cadence time.Duration, log *zap.Logger) *Client {
	t.Helper()
	c, err := New(in.path, in.id, in.store, in.attestations(t), cadence, log,
		WithHTTPClient(testHTTPClient()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

// claims returns the sequence of every stored claim attestation, oldest first, after checking each
// verifies over the link it names.
func (in *testInstall) claims(t *testing.T) []int64 {
	t.Helper()
	atts, err := in.attestations(t).Attestations(context.Background(), 0)
	if err != nil {
		t.Fatalf("Attestations() error = %v", err)
	}
	var seqs []int64
	for _, a := range atts {
		if a.Kind != audit.AttestationClaim {
			continue
		}
		cs := witness.Countersignature{KeyID: a.KeyID, PublicKey: a.PublicKey, Alg: "ed25519",
			Role: a.Role, At: a.At, Sig: a.Sig}
		if err := witness.VerifyClaimAttestation(a.Link, cs); err != nil {
			t.Errorf("the stored claim attestation at seq %d does not verify: %v", a.Seq, err)
		}
		seqs = append(seqs, a.Seq)
	}
	return seqs
}

// heads returns the tree size of every stored head attestation, oldest first, after checking each
// verifies over the root and size it names.
func (in *testInstall) heads(t *testing.T) []int64 {
	t.Helper()
	atts, err := in.attestations(t).Attestations(context.Background(), 0)
	if err != nil {
		t.Fatalf("Attestations() error = %v", err)
	}
	var sizes []int64
	for _, a := range atts {
		if a.Kind != audit.AttestationHead {
			continue
		}
		cs := witness.Countersignature{KeyID: a.KeyID, PublicKey: a.PublicKey, Alg: "ed25519",
			Role: a.Role, At: a.At, Sig: a.Sig}
		if err := witness.VerifyHeadAttestation(a.Root, a.Size, cs); err != nil || a.Seq != a.Size {
			t.Errorf("the stored head attestation at size %d does not verify: %v", a.Size, err)
		}
		sizes = append(sizes, a.Size)
	}
	return sizes
}

// seqsOf returns the chain positions of the given beats, numbered from one.
func (in *testInstall) seqsOf(beats ...int) []int64 {
	out := make([]int64, 0, len(beats))
	for _, n := range beats {
		out = append(out, in.beats[n-1].Seq)
	}
	return out
}

// observed returns a logger that records every entry, and the record.
func observed() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}
