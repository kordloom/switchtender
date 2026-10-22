package witnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// TestOutageQueuesAndResumes is invariant 10: while the witness is down the install keeps beating
// and anchoring, every push queues in order, a full queue drops its oldest push with the counter
// rising, and once the witness answers again the queue drains in order, each push proved from the
// size the witness last acknowledged, so the witness adopts every queued beat and notes only the
// dropped ones as beats it never received.
func TestOutageQueuesAndResumes(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	tests := []struct {
		// Down is how the witness is down during the outage.
		Down int32
	}{{ // Test 0: A tunnel answering 502 in front of a stopped service.
		Down: witnessBadGateway,
	}, { // Test 1: Connections dropped without an answer.
		Down: witnessReset,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newTestWitness(t)
			in := newTestInstall(t)
			in.entries(t, 3)
			in.enroll(t, w)
			// A beat every six hours makes one day of beats, the queue's span, four pushes.
			c := in.client(t, 6*time.Hour, nil)
			// anchored stands in for the timestamp anchor composed beside the push on every beat.
			var anchored []int64
			beat := func() error {
				b := in.beat(t)
				anchored = append(anchored, b.Beat)
				err := c.Beat(ctx, b)
				in.entries(t, 2)
				return err
			}
			for range 2 {
				if err := beat(); err != nil {
					t.Fatalf("Beat() with the witness up error = %v", err)
				}
			}
			w.down.Store(test.Down)
			var queued []int
			var dropped []int64
			for range 6 {
				if err := beat(); !errors.Is(err, ErrUnreachable) {
					t.Fatalf("Beat() with the witness down = %v, want ErrUnreachable", err)
				}
				st := c.Status()
				queued, dropped = append(queued, st.Queue), append(dropped, st.Dropped)
			}
			if diff := cmp.Diff([]int{1, 2, 3, 4, 4, 4}, queued); diff != "" {
				t.Errorf("queue length through the outage mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]int64{0, 0, 0, 0, 1, 2}, dropped); diff != "" {
				t.Errorf("dropped count through the outage mismatch (-want +got):\n%s", diff)
			}
			if st := c.Status(); st.LastBeat != 2 || !strings.Contains(st.LastError, "unreachable") {
				t.Errorf("Status() during the outage = beat %d, error %q; want beat 2 and the "+
					"outage named", st.LastBeat, st.LastError)
			}

			w.down.Store(witnessUp)
			if err := beat(); err != nil {
				t.Fatalf("Beat() once the witness answers again error = %v", err)
			}
			st := c.Status()
			if st.Queue != 0 || st.Dropped != 2 || st.LastBeat != 9 || st.LastSeq != in.beats[8].Seq {
				t.Errorf("Status() after the outage = queue %d, dropped %d, beat %d at seq %d; want "+
					"0, 2, 9 at %d", st.Queue, st.Dropped, st.LastBeat, st.LastSeq, in.beats[8].Seq)
			}
			if diff := cmp.Diff(in.seqsOf(1, 2, 5, 6, 7, 8, 9), in.claims(t)); diff != "" {
				t.Errorf("countersigned beats mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(in.seqsOf(1, 2, 5, 6, 7, 8, 9), in.heads(t)); diff != "" {
				t.Errorf("countersigned tree heads mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9}, anchored); diff != "" {
				t.Errorf("the anchor did not run on every beat (-want +got):\n%s", diff)
			}
			stmt := w.statement(t, in.id.KeyID())
			if stmt.LastBeat != 9 || stmt.Size != in.beats[8].Seq || stmt.RefusalsTotal != 0 {
				t.Errorf("statement = beat %d, size %d, refusals %d; want beat 9, size %d, none",
					stmt.LastBeat, stmt.Size, stmt.RefusalsTotal, in.beats[8].Seq)
			}
			wantKinds := map[string]int{"first_root": 1, "missing_beat": 1}
			if diff := cmp.Diff(wantKinds, w.findings(t, in.id.KeyID()).kinds()); diff != "" {
				t.Errorf("recorded findings mismatch (-want +got):\n%s", diff)
			}
			if st.LastNote == nil || st.LastNote.Beat != 5 || len(st.LastNote.Findings) != 1 ||
				st.LastNote.Findings[0].Kind != "missing_beat" {
				t.Errorf("Status().LastNote = %+v, want the missing_beat note on beat 5", st.LastNote)
			}
		})
	}
}

// TestRestoredMemoryRenegotiatesTheProofBase is invariant 11: a witness whose memory is rolled back
// three beats answers the next push with the size it holds, the client proves again from that size,
// the witness adopts it, memory_restored is recorded once as a note, nothing is refused, and the
// countersignatures issued in the lost interval are still held and still verify.
func TestRestoredMemoryRenegotiatesTheProofBase(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	in := newTestInstall(t)
	in.entries(t, 2)
	in.enroll(t, w)
	c := in.client(t, time.Minute, nil)
	push := func(n int) {
		t.Helper()
		for range n {
			if err := c.Beat(ctx, in.beat(t)); err != nil {
				t.Fatalf("Beat() error = %v", err)
			}
			in.entries(t, 3)
		}
	}
	push(3)
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	w.restart(t, func() {
		if err := os.CopyFS(snapshot, os.DirFS(w.dir)); err != nil {
			t.Fatalf("CopyFS() error = %v", err)
		}
	})
	push(3)
	// The restore runbook puts the snapshot back and leaves the marker the service turns into a
	// memory_restored note on start.
	w.restart(t, func() {
		if err := os.RemoveAll(w.dir); err != nil {
			t.Fatalf("RemoveAll() error = %v", err)
		}
		if err := os.CopyFS(w.dir, os.DirFS(snapshot)); err != nil {
			t.Fatalf("CopyFS() error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(w.dir, witness.RestoreMarker), nil, 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	})
	keyID := in.id.KeyID()
	if got := w.statement(t, keyID).LastBeat; got != 3 {
		t.Fatalf("the restored witness holds beat %d, want 3", got)
	}

	before := w.pushes.Load()
	push(1)
	if got := w.pushes.Load() - before; got != 2 {
		t.Errorf("beat 7 took %d pushes, want 2: one answered with the held size and one proved "+
			"from it", got)
	}
	stmt := w.statement(t, keyID)
	if stmt.LastBeat != 7 || stmt.Size != in.beats[6].Seq || stmt.RefusalsTotal != 0 {
		t.Errorf("statement = beat %d, size %d, refusals %d; want beat 7, size %d, none",
			stmt.LastBeat, stmt.Size, stmt.RefusalsTotal, in.beats[6].Seq)
	}
	found := w.findings(t, keyID)
	if got := found.kinds()["memory_restored"]; got != 1 || found.RefusalsTotal != 0 {
		t.Errorf("findings = %d memory_restored, %d refusals; want 1 and 0", got, found.RefusalsTotal)
	}
	for _, f := range found.Findings {
		if f.Kind == "memory_restored" && f.Class != witness.ClassNote {
			t.Errorf("memory_restored is recorded as %q, want a note", f.Class)
		}
	}
	if st := c.Status(); st.LastBeat != 7 || st.Queue != 0 {
		t.Errorf("Status() = beat %d, queue %d; want beat 7, queue 0", st.LastBeat, st.Queue)
	}
	if diff := cmp.Diff(in.seqsOf(1, 2, 3, 4, 5, 6, 7), in.claims(t)); diff != "" {
		t.Errorf("countersigned beats mismatch (-want +got):\n%s", diff)
	}

	before = w.pushes.Load()
	push(1)
	if got := w.pushes.Load() - before; got != 1 {
		t.Errorf("beat 8 took %d pushes, want 1 now that the base is settled", got)
	}
	if got := w.findings(t, keyID).kinds()["memory_restored"]; got != 1 {
		t.Errorf("memory_restored is recorded %d times, want once", got)
	}
}

// TestWitnessClientDropsAStaleBeat pins the stale rule: a beat the witness already holds a tree at
// or past is dropped, counted, logged at warn, and never sent again, and the size the witness named
// is where the next beat proves from, so it is countersigned on its first push.
func TestWitnessClientDropsAStaleBeat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	in := newTestInstall(t)
	in.entries(t, 2)
	in.enroll(t, w)
	first := in.client(t, time.Minute, nil)
	for range 3 {
		if err := first.Beat(ctx, in.beat(t)); err != nil {
			t.Fatalf("Beat() error = %v", err)
		}
		in.entries(t, 1)
	}
	// A second replica of the install, started after the first moved the witness on, pushes a beat
	// the first already passed.
	log, logs := observed()
	replica := in.client(t, time.Minute, log)
	before := w.pushes.Load()
	if err := replica.Beat(ctx, in.beats[1]); err != nil {
		t.Fatalf("Beat() of a superseded beat = %v, want nil: a stale beat is not an error", err)
	}
	if got := w.pushes.Load() - before; got != 1 {
		t.Errorf("the stale beat was pushed %d times, want once", got)
	}
	if st := replica.Status(); st.Stale != 1 || st.Queue != 0 {
		t.Errorf("Status() = stale %d, queue %d; want 1 and 0", st.Stale, st.Queue)
	}
	warned := logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("not be sent again")
	if n := warned.Len(); n != 1 {
		t.Errorf("the stale beat was logged at warn %d times, want once", n)
	}
	before = w.pushes.Load()
	b := in.beat(t)
	if err := replica.Beat(ctx, b); err != nil {
		t.Fatalf("Beat() after the stale one error = %v", err)
	}
	if got := w.pushes.Load() - before; got != 1 {
		t.Errorf("the next beat took %d pushes, want 1: the stale answer named the base", got)
	}
	if st := replica.Status(); st.LastBeat != b.Beat || st.Stale != 1 {
		t.Errorf("Status() = beat %d, stale %d; want beat %d, stale 1", st.LastBeat, st.Stale, b.Beat)
	}
	if got := w.statement(t, in.id.KeyID()).RefusalsTotal; got != 0 {
		t.Errorf("the witness refused %d pushes, want none: a stale beat accuses nothing", got)
	}
}

// TestWitnessClientKeepsTheLastRefusal pins what a refusal does: a push under the install's key
// over a chain rewritten below the witnessed size is answered 409 with findings, the client logs
// the refusal at error with its findings, keeps the answer as the last refusal, stores nothing,
// and goes on pushing later beats, each refused in turn.
func TestWitnessClientKeepsTheLastRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	honest := newTestInstall(t)
	honest.entries(t, 2)
	honest.enroll(t, w)
	hc := honest.client(t, time.Minute, nil)
	for range 2 {
		if err := hc.Beat(ctx, honest.beat(t)); err != nil {
			t.Fatalf("Beat() error = %v", err)
		}
		honest.entries(t, 1)
	}
	// The same key over a chain rewritten from its first entry, grown past the witnessed size.
	forged := newInstallFor(t, honest.id)
	forged.path = honest.path
	forged.entries(t, 12)
	log, logs := observed()
	fc := forged.client(t, time.Minute, log)
	for i := range 2 {
		b := forged.beat(t)
		if err := fc.Beat(ctx, b); !errors.Is(err, ErrRefused) {
			t.Fatalf("Beat() %d over the rewritten chain = %v, want ErrRefused", i, err)
		}
		st := fc.Status()
		if st.LastRefusal == nil || st.LastRefusal.Beat != b.Beat || st.LastRefusal.Seq != b.Seq {
			t.Fatalf("Status().LastRefusal = %+v, want the refusal of beat %d", st.LastRefusal, b.Beat)
		}
		rewritten := false
		for _, f := range st.LastRefusal.Findings {
			rewritten = rewritten || (f.Kind == "rewritten_history" && f.Class == witness.ClassRefusal)
		}
		if !rewritten {
			t.Errorf("the refusal of beat %d carries %+v, want rewritten_history as a refusal",
				b.Beat, st.LastRefusal.Findings)
		}
		if st.Queue != 0 {
			t.Errorf("a refused push stayed queued: queue %d", st.Queue)
		}
		forged.entries(t, 1)
	}
	if got := forged.claims(t); len(got) != 0 {
		t.Errorf("the rewritten chain stored countersignatures at %v, want none", got)
	}
	if logs.FilterLevelExact(zap.ErrorLevel).FilterMessageSnippet("rewritten_history").Len() == 0 {
		t.Error("the refusal was not logged at error with its findings")
	}
	if stmt := w.statement(t, honest.id.KeyID()); stmt.LastBeat != 2 || stmt.RefusalsTotal < 1 {
		t.Errorf("statement = beat %d, refusals %d; want beat 2 held and the refusal counted",
			stmt.LastBeat, stmt.RefusalsTotal)
	}
}

// TestWitnessClientStopsPushingWhenLapsed pins the lapse: once the witness answers 402 the client
// is lapsed, empties its queue, and sends nothing on later beats.
func TestWitnessClientStopsPushingWhenLapsed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: time.Now().UTC()}
	term := clock.Now().Add(time.Hour)
	w := newTestWitness(t, witness.WithServiceClock(clock.Now),
		witness.WithEnrollmentPolicy(func(witness.EnrollRequest) (witness.Entitlement, error) {
			return witness.Entitlement{LicenseID: "lic_term", Expires: term}, nil
		}))
	in := newTestInstall(t)
	in.entries(t, 1)
	in.enroll(t, w)
	c := in.client(t, time.Minute, nil)
	if err := c.Beat(ctx, in.beat(t)); err != nil {
		t.Fatalf("Beat() within the term error = %v", err)
	}
	clock.Add(time.Hour + witness.LapseGrace + time.Minute)
	if err := c.Beat(ctx, in.beat(t)); !errors.Is(err, ErrLapsed) {
		t.Fatalf("Beat() past the grace = %v, want ErrLapsed", err)
	}
	if st := c.Status(); !st.Lapsed || st.Queue != 0 || st.LastBeat != 1 {
		t.Errorf("Status() = lapsed %v, queue %d, beat %d; want lapsed, 0, 1", st.Lapsed, st.Queue,
			st.LastBeat)
	}
	before := w.pushes.Load()
	if err := c.Beat(ctx, in.beat(t)); err != nil {
		t.Errorf("Beat() once lapsed = %v, want nil", err)
	}
	if got := w.pushes.Load() - before; got != 0 {
		t.Errorf("a lapsed client pushed %d more times, want none", got)
	}
}

// TestWitnessClientDropsAPushTheWitnessCannotCheck pins the 400 rule: a push the witness reads as
// out of form is logged at error and dropped, never sent again, and the next beat is pushed on its
// own.
func TestWitnessClientDropsAPushTheWitnessCannotCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	in := newTestInstall(t)
	in.entries(t, 1)
	in.enroll(t, w)
	log, logs := observed()
	c := in.client(t, time.Minute, log)
	bad := in.beat(t)
	bad.Beat = 0
	before := w.pushes.Load()
	if err := c.Beat(ctx, bad); !errors.Is(err, ErrRejected) {
		t.Fatalf("Beat() of beat 0 = %v, want ErrRejected", err)
	}
	if st := c.Status(); st.Queue != 0 {
		t.Errorf("the rejected push stayed queued: queue %d", st.Queue)
	}
	if logs.FilterLevelExact(zap.ErrorLevel).FilterMessageSnippet("not be sent again").Len() != 1 {
		t.Error("the rejected push was not logged at error")
	}
	good := in.beat(t)
	if err := c.Beat(ctx, good); err != nil {
		t.Fatalf("Beat() after the rejected one error = %v", err)
	}
	if got := w.pushes.Load() - before; got != 2 {
		t.Errorf("the two beats took %d pushes, want 2: the rejected one is never sent again", got)
	}
	if diff := cmp.Diff([]int64{good.Seq}, in.claims(t)); diff != "" {
		t.Errorf("countersigned beats mismatch (-want +got):\n%s", diff)
	}
}

// TestWitnessClientStoresOnlyAHeldWitnessKey pins the key rule: an answer countersigned by a key
// the enrollment does not hold is not stored and its beat stays queued, and a key list written over
// the enrollment file is taken only when a held key signed it, so a list edited on disk never is.
func TestWitnessClientStoresOnlyAHeldWitnessKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	held, stranger := newTestWitness(t), newTestWitness(t)
	in := newTestInstall(t)
	in.entries(t, 1)
	in.enroll(t, held)
	// The stranger enrolls the install too, so it countersigns its pushes, with its own key.
	theirs, err := Enroll(ctx, testHTTPClient(), stranger.url, in.id, nil, time.Now())
	if err != nil {
		t.Fatalf("Enroll() with the stranger error = %v", err)
	}
	mine, err := Load(in.path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	mine.Witness = stranger.url
	if err := mine.Save(in.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	c := in.client(t, time.Minute, nil)
	err = c.Beat(ctx, in.beat(t))
	if !errors.Is(err, ErrWitnessKey) || !strings.Contains(c.Status().LastError, "--refresh-keys") {
		t.Fatalf("Beat() answered by an unheld key = %v, last error %q; want ErrWitnessKey naming "+
			"--refresh-keys", err, c.Status().LastError)
	}
	if err := theirs.Save(in.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := c.Beat(ctx, in.beat(t)); !errors.Is(err, ErrWitnessKey) {
		t.Fatalf("Beat() with an unendorsed list on disk = %v, want ErrWitnessKey", err)
	}
	if got := in.claims(t); len(got) != 0 {
		t.Errorf("countersignatures by an unheld key were stored at %v", got)
	}
	if st := c.Status(); st.Queue != 2 || st.WitnessKeyID != held.id.KeyID() {
		t.Errorf("Status() = queue %d, witness key %s; want 2 queued under %s", st.Queue,
			st.WitnessKeyID, held.id.KeyID())
	}
}

// TestWitnessClientTakesARefreshedKeyList pins a rotation end to end: after the witness rotates its
// key, its countersignatures by the new key wait in the queue until --refresh-keys keeps the new
// list, which the old key endorsed, and then every queued beat is stored under the key that signed
// it.
func TestWitnessClientTakesARefreshedKeyList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newKeyringWitness(t)
	in := newTestInstall(t)
	in.entries(t, 1)
	first := in.enroll(t, w)
	c := in.client(t, time.Minute, nil)
	if err := c.Beat(ctx, in.beat(t)); err != nil {
		t.Fatalf("Beat() before the rotation error = %v", err)
	}
	w.restart(t, func() {
		if _, err := witness.RotateKey(w.keyDir, w.dir, time.Now()); err != nil {
			t.Fatalf("RotateKey() error = %v", err)
		}
	})
	if err := c.Beat(ctx, in.beat(t)); !errors.Is(err, ErrWitnessKey) {
		t.Fatalf("Beat() after the rotation = %v, want ErrWitnessKey until the list is refreshed", err)
	}
	enr, changed, err := RefreshKeys(ctx, testHTTPClient(), in.path, "")
	if err != nil || !changed || enr.Keys.KeyID == first.Keys.KeyID {
		t.Fatalf("RefreshKeys() = %v, %v; want the rotated list kept", changed, err)
	}
	if err := enr.Save(in.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := c.Beat(ctx, in.beat(t)); err != nil {
		t.Fatalf("Beat() after the refresh error = %v", err)
	}
	if diff := cmp.Diff(in.seqsOf(1, 2, 3), in.claims(t)); diff != "" {
		t.Errorf("countersigned beats mismatch (-want +got):\n%s", diff)
	}
	atts, err := in.attestations(t).Attestations(ctx, 0)
	if err != nil {
		t.Fatalf("Attestations() error = %v", err)
	}
	type signer struct {
		// Seq is the countersigned coordinate.
		Seq int64
		// KeyID is the key that signed.
		KeyID string
		// Witness is the key the enrollment named as current.
		Witness string
	}
	var got []signer
	for _, a := range atts {
		if a.Kind == audit.AttestationClaim {
			got = append(got, signer{Seq: a.Seq, KeyID: a.KeyID, Witness: a.Witness})
		}
	}
	old, rotated := first.Keys.KeyID, enr.Keys.KeyID
	want := []signer{{in.beats[0].Seq, old, old}, {in.beats[1].Seq, rotated, rotated},
		{in.beats[2].Seq, rotated, rotated}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("signers mismatch (-want +got):\n%s", diff)
	}
	if st := c.Status(); st.WitnessKeyID != rotated || st.Queue != 0 {
		t.Errorf("Status() = witness key %s, queue %d; want %s and 0", st.WitnessKeyID, st.Queue,
			rotated)
	}
	if _, changed, err := RefreshKeys(ctx, testHTTPClient(), in.path, ""); err != nil || changed {
		t.Errorf("RefreshKeys() again = %v, %v; want unchanged", changed, err)
	}
}

// TestWitnessClientRefusesAnotherIdentitysEnrollment pins that a client never pushes under an
// enrollment that is not its identity's, or one edited out of form.
func TestWitnessClientRefusesAnotherIdentitysEnrollment(t *testing.T) {
	t.Parallel()
	w := newTestWitness(t)
	in := newTestInstall(t)
	in.enroll(t, w)
	other := newTestInstall(t)
	tests := []struct {
		// Edit changes the enrollment on disk, nil for none.
		Edit func(e *Enrollment)
		// Other uses another identity's client over the file.
		Other bool
		// WantErr is a fragment the refusal must contain.
		WantErr string
	}{{ // Test 0: Another producer key loads the file.
		Other: true, WantErr: "producer key",
	}, { // Test 1: The push path names another key.
		Edit:    func(e *Enrollment) { e.PushPath = pushPath(other.id.KeyID()) },
		WantErr: "push path",
	}, { // Test 2: The producer key id is not the key's digest.
		Edit:    func(e *Enrollment) { e.KeyID = other.id.KeyID() },
		WantErr: "digest",
	}, { // Test 3: The label was not born to the key.
		Edit:    func(e *Enrollment) { e.InstallID = other.id.InstallID },
		WantErr: "not born",
	}, { // Test 4: The witness key list was edited after it was signed.
		Edit:    func(e *Enrollment) { e.Keys.Keys[0].ValidFrom = "2020-01-01T00:00:00Z" },
		WantErr: "key list",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(in.path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			path := filepath.Join(t.TempDir(), EnrollmentFile)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if test.Edit != nil {
				e, err := Load(path)
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				test.Edit(e)
				data, err := json.MarshalIndent(e, "", "  ")
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			id := in.id
			if test.Other {
				id = other.id
			}
			_, err = New(path, id, in.store, in.attestations(t), time.Minute, nil)
			if err == nil || (!errors.Is(err, ErrEnrollment) && !errors.Is(err, ErrWitnessKey)) ||
				!strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("New() = %v, want a refusal naming %q", err, test.WantErr)
			}
		})
	}
	if _, err := New(filepath.Join(t.TempDir(), EnrollmentFile), in.id, in.store,
		in.attestations(t), time.Minute, nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("New() with no enrollment = %v, want a missing file", err)
	}
}

// TestWitnessClientStatusIsACopy pins that Status hands out a copy, so a caller holding one cannot
// change the next.
func TestWitnessClientStatusIsACopy(t *testing.T) {
	t.Parallel()
	w := newTestWitness(t)
	in := newTestInstall(t)
	in.enroll(t, w)
	c := in.client(t, time.Minute, nil)
	c.mu.Lock()
	c.status.LastRefusal = &Testimony{Beat: 1, Findings: []witness.RecordedFinding{{Kind: "k"}}}
	c.mu.Unlock()
	got := c.Status()
	got.LastRefusal.Findings[0].Kind = "changed"
	want := Status{Witness: w.url, WitnessKeyID: w.id.KeyID(), InstallID: in.id.InstallID,
		KeyID: in.id.KeyID(), QueueCap: QueueCap(time.Minute),
		LastRefusal: &Testimony{Beat: 1, Findings: []witness.RecordedFinding{{Kind: "k"}}}}
	if diff := cmp.Diff(want, c.Status(), cmpopts.IgnoreFields(Status{}, "EnrolledAt")); diff != "" {
		t.Errorf("Status() mismatch (-want +got):\n%s", diff)
	}
	if line := c.Status().Line(); !strings.Contains(line, w.id.KeyID()) ||
		!strings.Contains(line, "nothing countersigned") {
		t.Errorf("Line() = %q, want the witness key and the empty record named", line)
	}
}

// TestWitnessClientStatusIsSafeDuringBeats reads the status while beats are pushed, which is how
// the status route reads it, so the race detector sees every field the two share. A beat behind the
// acknowledged size is dropped as stale and a beat at no chain position is refused, both before
// anything is sent, and the next beat goes through.
func TestWitnessClientStatusIsSafeDuringBeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	in := newTestInstall(t)
	in.entries(t, 1)
	in.enroll(t, w)
	c := in.client(t, time.Minute, nil)
	done := make(chan struct{})
	read := make(chan int)
	go func() {
		reads := 0
		for {
			select {
			case <-done:
				read <- reads
				return
			default:
				_ = c.Status().Line()
				reads++
			}
		}
	}()
	for range 5 {
		if err := c.Beat(ctx, in.beat(t)); err != nil {
			t.Errorf("Beat() error = %v", err)
		}
		in.entries(t, 1)
	}
	close(done)
	if reads := <-read; reads == 0 {
		t.Error("the status was never read while beats were pushed")
	}
	if st := c.Status(); st.LastBeat != 5 {
		t.Errorf("Status().LastBeat = %d, want 5", st.LastBeat)
	}
	// A beat behind the size the witness acknowledged cannot be proved and is stale without
	// asking, so it neither reaches the witness nor stalls the queue.
	before := w.pushes.Load()
	if err := c.Beat(ctx, in.beats[0]); err != nil {
		t.Errorf("Beat() of an already countersigned beat = %v, want it dropped as stale", err)
	}
	if st := c.Status(); st.Stale != 1 || st.Queue != 0 {
		t.Errorf("Status() = stale %d, queue %d; want 1 and 0", st.Stale, st.Queue)
	}
	bad := in.beats[0]
	bad.Seq = 0
	if err := c.Beat(ctx, bad); !errors.Is(err, ErrChain) {
		t.Errorf("Beat() at chain position 0 = %v, want ErrChain", err)
	}
	if got := w.pushes.Load() - before; got != 0 {
		t.Errorf("the two beats took %d pushes, want none", got)
	}
	if err := c.Beat(ctx, in.beat(t)); err != nil {
		t.Errorf("Beat() after them error = %v", err)
	}
	if st := c.Status(); st.LastBeat != 6 {
		t.Errorf("Status().LastBeat = %d, want 6", st.LastBeat)
	}
}
