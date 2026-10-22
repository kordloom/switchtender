package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/witnessclient"
	"github.com/kordloom/switchtender/spanbeat"
	"github.com/kordloom/switchtender/witness"
)

// The ways a serve test witness answers a push.
const (
	// serveWitnessUp passes every request to the witness service.
	serveWitnessUp int32 = iota
	// serveWitnessDown answers every push 502, as a tunnel in front of a stopped service does.
	serveWitnessDown
	// serveWitnessHang holds every push until the test releases it, then serves it.
	serveWitnessHang
)

// serveTestWitness is a push witness in open policy whose answers to pushes a test controls.
type serveTestWitness struct {
	// url is the witness's base URL.
	url string
	// mode is how pushes are answered, one of the serveWitness constants.
	mode atomic.Int32
	// held is closed when a held push arrives.
	held chan struct{}
	// heldOnce closes held once.
	heldOnce sync.Once
	// release is closed to let held pushes through.
	release chan struct{}
	// releaseOnce closes release once.
	releaseOnce sync.Once
}

// newServeTestWitness starts a push witness in open policy and serves it.
func newServeTestWitness(t *testing.T) *serveTestWitness {
	t.Helper()
	id, err := audit.LoadWitnessIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessIdentity() error = %v", err)
	}
	svc, err := witness.NewService(id, t.TempDir(), time.Minute, nil, nil, nil,
		witness.WithAcceptEnrollments(witness.DefaultMaxInstalls),
		witness.WithEnrollmentPolicy(witness.OpenEnrollment),
		witness.WithWitnessKeys([]witness.WitnessKey{
			witness.WitnessKeyOf(id, time.Now().Add(-time.Hour))}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w := &serveTestWitness{held: make(chan struct{}), release: make(chan struct{})}
	h := svc.Handler()
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		push := strings.HasSuffix(r.URL.Path, "/beats")
		switch {
		case push && w.mode.Load() == serveWitnessDown:
			rw.WriteHeader(http.StatusBadGateway)
			return
		case push && w.mode.Load() == serveWitnessHang:
			w.heldOnce.Do(func() { close(w.held) })
			<-w.release
		}
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(api.Close)
	t.Cleanup(w.letGo)
	w.url = api.URL
	return w
}

// letGo releases every held push.
func (w *serveTestWitness) letGo() { w.releaseOnce.Do(func() { close(w.release) }) }

// serveTestInstall is an install enrolled with a serve test witness: its identity, its chain, and
// the license path its enrollment sits beside.
type serveTestInstall struct {
	// id signs the enrollment and every push.
	id identity.Identity
	// store is the audit chain, which also keeps anchors and countersignatures.
	store audit.Store
	// licensePath is the license file the enrollment sits beside. No license is written there.
	licensePath string
	// enrollment is the enrollment the witness answered.
	enrollment *witnessclient.Enrollment
}

// newServeTestInstall returns an install with a few chain entries, enrolled with w when w is not
// nil.
func newServeTestInstall(t *testing.T, w *serveTestWitness) *serveTestInstall {
	t.Helper()
	id, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	store := audit.NewMemStore()
	if b, ok := store.(audit.InstallBinder); ok {
		b.BindInstall(id.InstallID)
	}
	in := &serveTestInstall{id: id, store: store,
		licensePath: filepath.Join(t.TempDir(), "switchtender-license.json")}
	for range 3 {
		e := &audit.Entry{ID: audit.NewID(), Actor: "operator", Method: http.MethodPost,
			Path: "/v1/templates"}
		if err := store.Append(context.Background(), e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	if w == nil {
		return in
	}
	enr, err := witnessclient.Enroll(context.Background(), nil, w.url, id, nil, time.Now())
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	if err := enr.Save(witnessclient.PathFor(in.licensePath)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	in.enrollment = enr
	return in
}

// beat appends the next span beat and returns it as the emitter hands it to its hook.
func (in *serveTestInstall) beat(t *testing.T) spanbeat.AppendedBeat {
	t.Helper()
	for range 1000 {
		b, err := (auditBeatStore{store: in.store}).AppendSpanBeatNow(context.Background(), 60)
		if errors.Is(err, audit.ErrClockBehind) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("AppendSpanBeatNow() error = %v", err)
		}
		return b
	}
	t.Fatal("the clock never passed the last beat")
	return spanbeat.AppendedBeat{}
}

// anchored returns the chain positions anchors were recorded at.
func (in *serveTestInstall) anchored(t *testing.T) []int64 {
	t.Helper()
	anchors, ok := in.store.(audit.AnchorStore)
	if !ok {
		t.Fatal("the memory store keeps no anchors")
	}
	saved, err := anchors.Anchors(context.Background(), 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}
	seqs := []int64{}
	for _, a := range saved {
		seqs = append(seqs, a.Seq)
	}
	return seqs
}

// countersigned returns the chain positions claim countersignatures were stored at.
func (in *serveTestInstall) countersigned(t *testing.T) []int64 {
	t.Helper()
	atts, ok := in.store.(audit.AttestationStore)
	if !ok {
		t.Fatal("the memory store keeps no attestations")
	}
	saved, err := atts.Attestations(context.Background(), 0)
	if err != nil {
		t.Fatalf("Attestations() error = %v", err)
	}
	seqs := []int64{}
	for _, a := range saved {
		if a.Kind == audit.AttestationClaim {
			seqs = append(seqs, a.Seq)
		}
	}
	return seqs
}

// recordingAnchor returns a stand-in for the timestamp authority's anchor hook: it records an
// anchor over the beat in store, or returns fail without recording anything when fail is set.
func recordingAnchor(store audit.Store, fail error) spanbeat.AnchorFunc {
	return func(ctx context.Context, b spanbeat.AppendedBeat) error {
		if fail != nil {
			return fail
		}
		anchors, ok := store.(audit.AnchorStore)
		if !ok {
			return errors.New("the store keeps no anchors")
		}
		return anchors.SaveAnchor(ctx, &audit.Anchor{ID: audit.NewAnchorID(),
			Type: audit.AnchorRFC3161, Shape: audit.AnchorShapeLinear, Seq: b.Seq, Link: b.Hash,
			At: time.Now(), Ref: "https://tsa.test/tsr"})
	}
}

// errTestAuthority is the error a failing stand-in timestamp authority returns.
var errTestAuthority = errors.New("timestamp authority down")

// TestWitnessPushNeverBlocksTheAnchor pins the composition serve runs on every span beat: the
// timestamp authority's anchor and the witness push each run on their own, so a witness that hangs
// or is down leaves the anchor recorded, an authority that fails leaves the countersignature
// stored, and the error names every hook that failed.
func TestWitnessPushNeverBlocksTheAnchor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Mode is how the witness answers the push.
		Mode int32
		// AnchorErr is what the stand-in authority fails with, nil when it records the anchor.
		AnchorErr error
		// WantAnchored is whether the anchor must be recorded at the beat.
		WantAnchored bool
		// WantCountersigned is whether the witness's countersignature must be stored at the beat.
		WantCountersigned bool
		// WantErrs are the errors the hook's error must carry, none for a nil error.
		WantErrs []error
	}{{ // Test 0: A witness holding the push leaves the anchor recorded while it holds it.
		Mode: serveWitnessHang, WantAnchored: true, WantCountersigned: true,
	}, { // Test 1: A witness that is down leaves the anchor recorded.
		Mode: serveWitnessDown, WantAnchored: true,
		WantErrs: []error{witnessclient.ErrUnreachable},
	}, { // Test 2: An authority that fails leaves the countersignature stored.
		Mode: serveWitnessUp, AnchorErr: errTestAuthority, WantCountersigned: true,
		WantErrs: []error{errTestAuthority},
	}, { // Test 3: Both run and both record.
		Mode: serveWitnessUp, WantAnchored: true, WantCountersigned: true,
	}, { // Test 4: Both fail and the error names each.
		Mode: serveWitnessDown, AnchorErr: errTestAuthority,
		WantErrs: []error{witnessclient.ErrUnreachable, errTestAuthority},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			w := newServeTestWitness(t)
			in := newServeTestInstall(t, w)
			c, err := serveWitnessClient("", in.licensePath, time.Minute, &in.id, in.store,
				zap.NewNop())
			if err != nil || c == nil {
				t.Fatalf("serveWitnessClient() = %v, %v, want a client", c, err)
			}
			hook := beatHooks(recordingAnchor(in.store, test.AnchorErr), c)
			b := in.beat(t)
			w.mode.Store(test.Mode)

			done := make(chan error, 1)
			go func() { done <- hook(context.Background(), b) }()
			if test.Mode == serveWitnessHang {
				waitForHeldPush(t, w)
				waitForAnchor(t, in, b.Seq, done)
				w.letGo()
			}
			got := <-done

			for _, want := range test.WantErrs {
				if !errors.Is(got, want) {
					t.Errorf("hook() error = %v, want it to carry %v", got, want)
				}
			}
			if len(test.WantErrs) == 0 && got != nil {
				t.Errorf("hook() error = %v, want nil", got)
			}
			wantAnchored, wantCountersigned := []int64{}, []int64{}
			if test.WantAnchored {
				wantAnchored = []int64{b.Seq}
			}
			if test.WantCountersigned {
				wantCountersigned = []int64{b.Seq}
			}
			if diff := cmp.Diff(wantAnchored, in.anchored(t)); diff != "" {
				t.Errorf("anchored positions mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(wantCountersigned, in.countersigned(t)); diff != "" {
				t.Errorf("countersigned positions mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// waitForHeldPush waits until the witness is holding a push.
func waitForHeldPush(t *testing.T, w *serveTestWitness) {
	t.Helper()
	select {
	case <-w.held:
	case <-time.After(20 * time.Second):
		t.Fatal("the push never reached the witness")
	}
}

// waitForAnchor waits until the anchor at seq is recorded while the hook, whose result arrives on
// done, is still running because the witness holds its push.
func waitForAnchor(t *testing.T, in *serveTestInstall, seq int64, done chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			done <- err
			t.Errorf("the hook returned %v while the witness held its push", err)
			return
		default:
		}
		if cmp.Equal(in.anchored(t), []int64{seq}) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("no anchor was recorded at %d while the witness held the push", seq)
}

// chainOnlyStore is an audit store that keeps no witness countersignatures.
type chainOnlyStore struct {
	// Store is the chain it reads and writes.
	audit.Store
}

// TestServeWitnessClientReadsTheEnrollment pins when serve pushes to a witness: with no enrollment
// beside the license and no --witness-enrollment nothing changes, a named enrollment that is
// missing or a broken one is an error, an enrolled install without span beats, a producer identity,
// or a store that keeps countersignatures refuses the start and says why, and an enrolled install
// gets a client and a startup line naming the witness key id.
func TestServeWitnessClientReadsTheEnrollment(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	w := newServeTestWitness(t)
	enrolled := newServeTestInstall(t, w)
	bare := newServeTestInstall(t, nil)
	broken := newServeTestInstall(t, nil)
	if err := os.WriteFile(witnessclient.PathFor(broken.licensePath), []byte("{}"),
		0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "named-enrollment.json")
	if err := enrolled.enrollment.Save(elsewhere); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tests := []struct {
		// Install is the install serve starts over.
		Install *serveTestInstall
		// LicensePath overrides the install's license path when set.
		LicensePath string
		// Flag is the --witness-enrollment value.
		Flag string
		// Cadence is the --span-cadence value.
		Cadence time.Duration
		// NoProducer starts the install without a producer identity.
		NoProducer bool
		// ChainOnly starts over a store that keeps no countersignatures.
		ChainOnly bool
		// WantClient is whether a client must be returned.
		WantClient bool
		// WantErr is a fragment the error must contain, empty for no error.
		WantErr string
		// WantIs are errors the error must carry.
		WantIs []error
	}{{ // Test 0: No enrollment and no flag changes nothing, with or without beats.
		Install: bare, Cadence: time.Minute,
	}, { // Test 1: No enrollment and no beats changes nothing either.
		Install: bare,
	}, { // Test 2: A named enrollment that is missing is an error.
		Install: bare, Flag: filepath.Join(t.TempDir(), "missing.json"), Cadence: time.Minute,
		WantErr: "--witness-enrollment", WantIs: []error{witnessclient.ErrEnrollment,
			fs.ErrNotExist},
	}, { // Test 3: A broken enrollment beside the license is an error.
		Install: broken, Cadence: time.Minute, WantErr: "remove it",
		WantIs: []error{witnessclient.ErrEnrollment},
	}, { // Test 4: An enrolled install without span beats refuses the start.
		Install: enrolled, WantErr: "--span-cadence is not set",
	}, { // Test 5: A named enrollment without span beats refuses the start.
		Install: bare, Flag: elsewhere, WantErr: "--span-cadence is not set",
	}, { // Test 6: An enrolled install without a producer identity refuses the start.
		Install: enrolled, Cadence: time.Minute, NoProducer: true, WantErr: "producer identity",
	}, { // Test 7: A store that keeps no countersignatures refuses the start.
		Install: enrolled, Cadence: time.Minute, ChainOnly: true, WantErr: "countersignatures",
	}, { // Test 8: An enrolled install gets a client.
		Install: enrolled, Cadence: time.Minute, WantClient: true,
	}, { // Test 9: A named enrollment is read wherever the license is.
		Install: enrolled, LicensePath: bare.licensePath, Flag: elsewhere, Cadence: time.Minute,
		WantClient: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zap.InfoLevel)
			producer := &test.Install.id
			if test.NoProducer {
				producer = nil
			}
			store := test.Install.store
			if test.ChainOnly {
				store = chainOnlyStore{Store: store}
			}
			licensePath := test.Install.licensePath
			if test.LicensePath != "" {
				licensePath = test.LicensePath
			}
			c, err := serveWitnessClient(test.Flag, licensePath, test.Cadence, producer, store,
				zap.New(core))
			if test.WantErr == "" && err != nil {
				t.Fatalf("serveWitnessClient() error = %v, want nil", err)
			}
			if test.WantErr != "" && (err == nil || !strings.Contains(err.Error(), test.WantErr)) {
				t.Fatalf("serveWitnessClient() error = %v, want one naming %q", err, test.WantErr)
			}
			for _, want := range test.WantIs {
				if !errors.Is(err, want) {
					t.Errorf("serveWitnessClient() error = %v, want it to carry %v", err, want)
				}
			}
			if diff := cmp.Diff(test.WantClient, c != nil); diff != "" {
				t.Fatalf("client returned mismatch (-want +got):\n%s", diff)
			}
			if c == nil {
				if diff := cmp.Diff(0, logs.Len()); diff != "" {
					t.Errorf("log lines without a client mismatch (-want +got):\n%s", diff)
				}
				return
			}
			st := c.Status()
			want := test.Install.enrollment
			if diff := cmp.Diff([]string{want.Keys.KeyID, want.InstallID},
				[]string{st.WitnessKeyID, st.InstallID}); diff != "" {
				t.Errorf("status enrollment mismatch (-want +got):\n%s", diff)
			}
			lines := logs.FilterMessageSnippet(want.Keys.KeyID).All()
			if diff := cmp.Diff(1, len(lines)); diff != "" {
				t.Fatalf("startup lines naming the witness key mismatch (-want +got):\n%s", diff)
			}
			if !strings.Contains(lines[0].Message, st.Line()) {
				t.Errorf("startup line = %q, want it to carry the status %q", lines[0].Message,
					st.Line())
			}
		})
	}
}

// TestChainAnchorFuncsRunsEveryHook pins the composition itself: nil hooks are left out, a single
// hook is returned unchanged in behavior, every hook runs on every beat whatever the others return,
// and the error joins each failure.
func TestChainAnchorFuncsRunsEveryHook(t *testing.T) {
	t.Parallel()
	errA, errB := errors.New("hook a failed"), errors.New("hook b failed")
	tests := []struct {
		// Fails holds, per hook, what it returns; a nil entry is a nil hook rather than a hook.
		Fails []error
		// Nil marks which hooks are nil.
		Nil []bool
		// WantNil is whether the composition must be nil.
		WantNil bool
		// WantRuns is how many hooks must run.
		WantRuns int64
		// WantErrs are the errors the composition's error must carry.
		WantErrs []error
	}{{ // Test 0: No hooks compose to nil.
		WantNil: true,
	}, { // Test 1: Only nil hooks compose to nil.
		Fails: []error{nil, nil}, Nil: []bool{true, true}, WantNil: true,
	}, { // Test 2: One hook beside a nil one runs alone.
		Fails: []error{errA, nil}, Nil: []bool{false, true}, WantRuns: 1,
		WantErrs: []error{errA},
	}, { // Test 3: A failing first hook does not stop the second.
		Fails: []error{errA, nil}, Nil: []bool{false, false}, WantRuns: 2,
		WantErrs: []error{errA},
	}, { // Test 4: A failing second hook does not stop the first.
		Fails: []error{nil, errB}, Nil: []bool{false, false}, WantRuns: 2,
		WantErrs: []error{errB},
	}, { // Test 5: Both fail and both are named.
		Fails: []error{errA, errB}, Nil: []bool{false, false}, WantRuns: 2,
		WantErrs: []error{errA, errB},
	}, { // Test 6: Both succeed.
		Fails: []error{nil, nil}, Nil: []bool{false, false}, WantRuns: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var runs atomic.Int64
			fns := make([]spanbeat.AnchorFunc, len(test.Fails))
			for i, fail := range test.Fails {
				if test.Nil[i] {
					continue
				}
				fns[i] = func(context.Context, spanbeat.AppendedBeat) error {
					runs.Add(1)
					return fail
				}
			}
			hook := chainAnchorFuncs(fns...)
			if diff := cmp.Diff(test.WantNil, hook == nil); diff != "" {
				t.Fatalf("nil composition mismatch (-want +got):\n%s", diff)
			}
			if hook == nil {
				return
			}
			err := hook(context.Background(), spanbeat.AppendedBeat{Seq: 1, Beat: 1})
			if diff := cmp.Diff(test.WantRuns, runs.Load()); diff != "" {
				t.Errorf("hooks run mismatch (-want +got):\n%s", diff)
			}
			for _, want := range test.WantErrs {
				if !errors.Is(err, want) {
					t.Errorf("hook() error = %v, want it to carry %v", err, want)
				}
			}
			if len(test.WantErrs) == 0 && err != nil {
				t.Errorf("hook() error = %v, want nil", err)
			}
		})
	}
}
