package witness

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// lapsingTerm is the license term the retention tests enroll under, ending ten days after
// pushEpoch.
func lapsingTerm() time.Time { return pushEpoch.Add(10 * 24 * time.Hour) }

// readOnlyEnds returns the last instant a record whose term ends at term is kept, counted here
// rather than read from the code under test: thirty days of grace, then twelve months.
func readOnlyEnds(term time.Time) time.Time {
	return term.Add(30*24*time.Hour).AddDate(0, 12, 0)
}

// enrollLapsing enrolls pr under licenseID for lapsingTerm and has the witness countersign beats 1
// and 2, the tree at sizes 2 and 4, inside the term. It returns the two answers.
func enrollLapsing(t *testing.T, w *pushWitness, pr *producer, licenseID string) []PushAnswer {
	t.Helper()
	if _, err := w.svc.Enroll(pr.enrollment(t, licenseID, lapsingTerm())); err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	var held []PushAnswer
	for _, p := range []Push{pr.push(t, 1, 2, 0), pr.push(t, 2, 4, 2)} {
		code, answer, raw := w.pushHTTP(t, pr, p)
		if code != http.StatusOK {
			t.Fatalf("push inside the term = %d %s, want 200", code, raw)
		}
		held = append(held, answer)
	}
	return held
}

// enrollAgain posts a newer license for pr to the enrollment route, ending at expires, and returns
// the signed answer.
func enrollAgain(t *testing.T, w *pushWitness, pr *producer, licenseID string,
	expires time.Time) EnrollResponse {
	t.Helper()
	code, raw := w.post(t, "/v1/enroll", pr.enrollment(t, licenseID, expires))
	if code != http.StatusOK {
		t.Fatalf("enrollment with a newer license = %d %s, want 200", code, raw)
	}
	var resp EnrollResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode enrollment answer: %v", err)
	}
	return resp
}

// TestALapsedRecordIsReadableForAYear pins the read-only year: once a lapsed install's grace has
// passed, every push answers 402 and writes nothing, while its statement and findings still answer
// with the memory as it stood, through the last instant of the twelfth month, and the attestations
// the install stored still verify.
func TestALapsedRecordIsReadableForAYear(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	held := enrollLapsing(t, w, pr, "lic_readable")
	graceEnd := lapsingTerm().Add(30 * 24 * time.Hour)
	tests := []struct {
		// At is when the sweep runs and the install pushes and reads.
		At time.Time
	}{{ // Test 0: The day after the grace ends.
		At: graceEnd.Add(24 * time.Hour),
	}, { // Test 1: Six months into the read-only year.
		At: graceEnd.AddDate(0, 6, 0),
	}, { // Test 2: The last instant of the twelfth month.
		At: readOnlyEnds(lapsingTerm()),
	}}
	for testNum, test := range tests {
		w.clock.Set(test.At)
		w.svc.sweepInstalls()
		before := snapshotDir(t, w.dir)
		if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 3, 6, 4)); code != http.StatusPaymentRequired {
			t.Errorf("test %d: push = %d %s, want 402", testNum, code, raw)
		}
		if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
			t.Errorf("test %d: a lapsed push wrote (-before +after):\n%s", testNum, diff)
		}
		st := w.statement(t, pr)
		if st.LastBeat != 2 || st.Size != 4 || st.Root != testRoot(pr.leaves, 4) ||
			st.MemorySince != stamp(pushEpoch) || st.RefusalsTotal != 0 {
			t.Errorf("test %d: statement beat %d size %d root %s since %s refusals %d, want the "+
				"memory as it stood at the lapse", testNum, st.LastBeat, st.Size, st.Root,
				st.MemorySince, st.RefusalsTotal)
		}
		if diff := cmp.Diff([]string{"first_root/note", "silent_install/note"},
			kindsAndClasses(w.findings(t, pr).Findings)); diff != "" {
			t.Errorf("test %d: the record (-want +got):\n%s", testNum, diff)
		}
	}
	for i, answer := range held {
		if err := VerifyClaimAttestation(answer.Head, *answer.ClaimAttestation); err != nil {
			t.Errorf("stored claim attestation %d in the read-only year: %v", i, err)
		}
		if err := VerifyHeadAttestation(answer.Root, answer.Size, *answer.HeadAttestation); err != nil {
			t.Errorf("stored head attestation %d in the read-only year: %v", i, err)
		}
	}
}

// TestTheSweepDeletesARecordAfterItsYear pins the deletion: each record is deleted by the first
// sweep after its own read-only year and not an instant sooner, a record with no term never is, the
// sweep removes that record's directory and writes nothing else anywhere in the state directory,
// pull state included, it logs the producer key id once and notifies nothing, the deleted key is
// unknown to every route, and a restart does not bring it back.
func TestTheSweepDeletesARecordAfterItsYear(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	core, logs := observer.New(zap.InfoLevel)
	var notified atomic.Int64
	w := &pushWitness{id: testIdentity(t), dir: t.TempDir(), clock: &testClock{now: pushEpoch}}
	svc, err := NewService(w.id, w.dir, time.Minute, nil, zap.New(core), nil,
		WithServiceClock(w.clock.Now), WithAcceptEnrollments(DefaultMaxInstalls),
		WithEnrollmentPolicy(testPolicy), WithWitnessKeys([]WitnessKey{WitnessKeyOf(w.id, pushEpoch)}),
		WithServiceNotify(func(string, Finding) { notified.Add(1) }))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w.svc, w.api = svc, newTestServer(svc.Handler())
	t.Cleanup(w.api.Close)

	shortTerm, longTerm := lapsingTerm(), pushEpoch.AddDate(2, 0, 0)
	producers := map[string]*producer{"short": newProducer(t, 4), "long": newProducer(t, 4),
		"forever": newProducer(t, 4)}
	for name, term := range map[string]time.Time{"short": shortTerm, "long": longTerm,
		"forever": {}} {
		pr := producers[name]
		if _, err := w.svc.Enroll(pr.enrollment(t, "lic_"+name, term)); err != nil {
			t.Fatalf("Enroll(%s) error = %v", name, err)
		}
		if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
			t.Fatalf("push by %s = %d %s, want 200", name, code, raw)
		}
	}
	pull := filepath.Join(w.dir, StateFileName("https://pull.example.test"))
	if err := os.WriteFile(pull, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// Every install falls silent long before any record is due, so the sweeps below have nothing to
	// note and anything they write is the deletion's.
	w.clock.Set(shortTerm.Add(31 * 24 * time.Hour))
	w.svc.sweepInstalls()
	logs.TakeAll()
	quiet := notified.Load()
	stale := w.svc.lookup(producers["short"].keyID())

	tests := []struct {
		// At is when the sweep runs.
		At time.Time
		// WantKept names the producers whose records remain after it.
		WantKept []string
	}{{ // Test 0: The last instant of the short term's read-only year keeps every record.
		At: readOnlyEnds(shortTerm), WantKept: []string{"forever", "long", "short"},
	}, { // Test 1: One second later the short term's record is deleted.
		At: readOnlyEnds(shortTerm).Add(time.Second), WantKept: []string{"forever", "long"},
	}, { // Test 2: The long term's last instant keeps it.
		At: readOnlyEnds(longTerm), WantKept: []string{"forever", "long"},
	}, { // Test 3: One second later it is deleted.
		At: readOnlyEnds(longTerm).Add(time.Second), WantKept: []string{"forever"},
	}, { // Test 4: A record with no term is never deleted.
		At: readOnlyEnds(longTerm).AddDate(10, 0, 0), WantKept: []string{"forever"},
	}}
	kept := map[string]bool{"short": true, "long": true, "forever": true}
	for testNum, test := range tests {
		w.clock.Set(test.At)
		before := snapshotDir(t, w.dir)
		w.svc.sweepInstalls()
		var deleted []string
		for name, pr := range producers {
			if !kept[name] || slices.Contains(test.WantKept, name) {
				continue
			}
			kept[name] = false
			deleted = append(deleted, pr.keyID())
			prefix := strings.TrimPrefix(w.recordDir(pr), w.dir) + string(filepath.Separator)
			for path := range before {
				if strings.HasPrefix(path, prefix) {
					delete(before, path)
				}
			}
			for _, dir := range []string{w.recordDir(pr), w.recordDir(pr) + retiredSuffix} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("test %d: %s after the sweep: %v, want it gone", testNum, dir, err)
				}
			}
		}
		if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
			t.Errorf("test %d: the sweep wrote more than the deletion (-want +got):\n%s", testNum,
				diff)
		}
		entries := logs.TakeAll()
		if len(entries) != len(deleted) {
			t.Errorf("test %d: the sweep logged %d lines, want one per deleted record, %d", testNum,
				len(entries), len(deleted))
		}
		for _, keyID := range deleted {
			if !slices.ContainsFunc(entries, func(e observer.LoggedEntry) bool {
				return strings.Contains(e.Message, keyID)
			}) {
				t.Errorf("test %d: no log line names the deleted key %s", testNum, keyID)
			}
		}
		for name, pr := range producers {
			want := http.StatusNotFound
			if kept[name] {
				want = http.StatusOK
			}
			for _, route := range []string{"attestation", "findings"} {
				if code, raw := w.get(t, "/v1/installs/"+pr.keyID()+"/"+route); code != want {
					t.Errorf("test %d: GET %s of %s = %d %s, want %d", testNum, route, name, code,
						raw, want)
				}
			}
		}
	}
	if got := notified.Load(); got != quiet {
		t.Errorf("the deletions notified %d times, want none", got-quiet)
	}
	if code, raw := w.post(t, "/v1/installs/"+producers["short"].keyID()+"/beats",
		producers["short"].push(t, 2, 4, 2)); code != http.StatusNotFound {
		t.Errorf("push after the deletion = %d %s, want 404", code, raw)
	}
	// A push that found the record before the sweep deleted it answers as if nothing were enrolled.
	stale.mu.Lock()
	_, _, err = w.svc.hold(stale, producers["short"].push(t, 2, 4, 2))
	stale.mu.Unlock()
	if !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("push holding the deleted record = %v, want ErrNotEnrolled", err)
	}
	w.restart(t)
	for name, pr := range producers {
		want := http.StatusNotFound
		if kept[name] {
			want = http.StatusOK
		}
		if code, raw := w.get(t, "/v1/installs/"+pr.keyID()+"/attestation"); code != want {
			t.Errorf("after a restart GET statement of %s = %d %s, want %d", name, code, raw, want)
		}
	}
}

// TestReenrollingInsideTheYearKeepsTheMemory pins that a newer license arriving while a lapsed
// record is still held reopens that record and its memory: the answer keeps the first enrolled_at,
// the sweep at the moment the lapsed record was due keeps the renewed one, a rewrite of the history
// witnessed before the lapse is still refused, and the next honest push extends the old root with
// no new first_root.
func TestReenrollingInsideTheYearKeepsTheMemory(t *testing.T) {
	t.Parallel()
	graceEnd := lapsingTerm().Add(30 * 24 * time.Hour)
	tests := []struct {
		// Renew is when the newer license enrolls.
		Renew time.Time
	}{{ // Test 0: The day after the grace ends.
		Renew: graceEnd.Add(24 * time.Hour),
	}, { // Test 1: Eleven months into the read-only year.
		Renew: graceEnd.AddDate(0, 11, 0),
	}, { // Test 2: The last instant of the read-only year.
		Renew: readOnlyEnds(lapsingTerm()),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			w := newPushWitness(t)
			pr := newProducer(t, 8)
			enrollLapsing(t, w, pr, "lic_lapsed")
			w.clock.Set(test.Renew)
			w.svc.sweepInstalls()
			if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 3, 6, 4)); code != http.StatusPaymentRequired {
				t.Fatalf("push before the renewal = %d %s, want 402", code, raw)
			}
			resp := enrollAgain(t, w, pr, "lic_renewed", test.Renew.AddDate(1, 0, 0))
			if resp.EnrolledAt != stamp(pushEpoch) {
				t.Errorf("renewal enrolled_at = %s, want the first %s", resp.EnrolledAt,
					stamp(pushEpoch))
			}
			w.clock.Set(readOnlyEnds(lapsingTerm()).Add(time.Second))
			w.svc.sweepInstalls()

			code, answer, raw := w.pushHTTP(t, pr, pr.pushOver(t, testLeaves(t, 8, 3), 3, 6, 4))
			if code != http.StatusConflict || answer.HeldSize != nil || answer.ClaimAttestation != nil {
				t.Errorf("rewrite after the renewal = %d %s, want refused against the memory held "+
					"since before the lapse", code, raw)
			}
			if diff := cmp.Diff([]string{"rewritten_history/refusal"},
				kindsAndClasses(answer.Findings)); diff != "" {
				t.Errorf("rewrite after the renewal raised (-want +got):\n%s", diff)
			}
			code, answer, raw = w.pushHTTP(t, pr, pr.push(t, 4, 8, 4))
			if code != http.StatusOK || answer.ClaimAttestation == nil ||
				slices.Contains(kindsAndClasses(answer.Findings), "first_root/note") {
				t.Errorf("honest push after the renewal = %d %s, want countersigned as an "+
					"extension of the old root", code, raw)
			}
			st := w.statement(t, pr)
			if st.MemorySince != stamp(pushEpoch) || st.LastBeat != 4 || st.Size != 8 ||
				st.Root != testRoot(pr.leaves, 8) || st.RefusalsTotal != 1 {
				t.Errorf("statement since %s beat %d size %d refusals %d, want since %s, beat 4, "+
					"size 8, one refusal", st.MemorySince, st.LastBeat, st.Size, st.RefusalsTotal,
					stamp(pushEpoch))
			}
		})
	}
}

// TestReenrollingAfterDeletionStartsAFreshMemory pins that a key whose record was deleted enrolls
// into a fresh memory: a new enrolled_at, nothing held, so a push extending the old tree is told
// the witness holds none, the first push is countersigned with a first_root note, and none of the
// old record's refusals or notes come back. A deletion a crash or the disk cut short after its
// rename is finished at the next start and reads the same.
func TestReenrollingAfterDeletionStartsAFreshMemory(t *testing.T) {
	t.Parallel()
	due := readOnlyEnds(lapsingTerm()).Add(time.Second)
	tests := []struct {
		// Delete removes the lapsed record.
		Delete func(t *testing.T, w *pushWitness, pr *producer)
	}{{ // Test 0: The sweep deletes it.
		Delete: func(_ *testing.T, w *pushWitness, _ *producer) {
			w.clock.Set(due)
			w.svc.sweepInstalls()
		},
	}, { // Test 1: The sweep deletes it and the witness restarts before the key enrolls again.
		Delete: func(t *testing.T, w *pushWitness, _ *producer) {
			w.clock.Set(due)
			w.svc.sweepInstalls()
			w.restart(t)
		},
	}, { // Test 2: A crash stops the deletion after the rename that decides it.
		Delete: func(t *testing.T, w *pushWitness, pr *producer) {
			w.clock.Set(due)
			w.api.Close()
			w.svc.Close()
			if err := os.Rename(w.recordDir(pr), w.recordDir(pr)+retiredSuffix); err != nil {
				t.Fatalf("Rename() error = %v", err)
			}
			w.start(t)
		},
	}, { // Test 3: The disk refuses to remove the files once the record is renamed. The deletion
		// stands from the rename, and the next start finishes it.
		Delete: func(t *testing.T, w *pushWitness, pr *producer) {
			if err := os.Chmod(w.recordDir(pr), 0o500); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
			w.clock.Set(due)
			w.svc.sweepInstalls()
			for _, dir := range []string{w.recordDir(pr), w.recordDir(pr) + retiredSuffix} {
				_ = os.Chmod(dir, 0o700)
			}
			if code, raw := w.get(t, "/v1/installs/"+pr.keyID()+"/attestation"); code !=
				http.StatusNotFound {
				t.Errorf("statement when the files could not be removed = %d %s, want 404", code, raw)
			}
			w.restart(t)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			w := newPushWitness(t)
			pr := newProducer(t, 8)
			enrollLapsing(t, w, pr, "lic_old")
			if code, _, raw := w.pushHTTP(t, pr, pr.pushOver(t, testLeaves(t, 8, 1), 3, 6,
				4)); code != http.StatusConflict {
				t.Fatalf("rewrite inside the term = %d %s, want 409", code, raw)
			}
			test.Delete(t, w, pr)
			for _, dir := range []string{w.recordDir(pr), w.recordDir(pr) + retiredSuffix} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("%s after the deletion: %v, want it gone", dir, err)
				}
			}
			if code, raw := w.get(t, "/v1/installs/"+pr.keyID()+"/attestation"); code !=
				http.StatusNotFound {
				t.Errorf("statement after the deletion = %d %s, want 404", code, raw)
			}

			now := w.clock.Now()
			if resp := enrollAgain(t, w, pr, "lic_new", now.AddDate(1, 0, 0)); resp.EnrolledAt !=
				stamp(now) {
				t.Errorf("enrolled_at = %s, want the new enrollment's %s", resp.EnrolledAt, stamp(now))
			}
			code, answer, raw := w.pushHTTP(t, pr, pr.push(t, 3, 6, 4))
			if code != http.StatusConflict || answer.HeldSize == nil || *answer.HeldSize != 0 {
				t.Errorf("push extending the deleted memory = %d %s, want 409 holding size 0", code,
					raw)
			}
			code, answer, raw = w.pushHTTP(t, pr, pr.push(t, 3, 6, 0))
			if code != http.StatusOK || answer.ClaimAttestation == nil || answer.HeadAttestation == nil {
				t.Fatalf("first push into the fresh memory = %d %s, want countersigned", code, raw)
			}
			if diff := cmp.Diff([]string{"first_root/note"}, kindsAndClasses(answer.Findings)); diff != "" {
				t.Errorf("first push raised (-want +got):\n%s", diff)
			}
			st := w.statement(t, pr)
			if st.MemorySince != stamp(now) || st.RefusalsTotal != 0 || st.NotesTotal != 1 ||
				st.LastBeat != 3 || st.Size != 6 {
				t.Errorf("statement since %s refusals %d notes %d beat %d size %d, want since %s, "+
					"0, 1, 3, 6", st.MemorySince, st.RefusalsTotal, st.NotesTotal, st.LastBeat,
					st.Size, stamp(now))
			}
			if diff := cmp.Diff([]string{"first_root/note"},
				kindsAndClasses(w.findings(t, pr).Findings)); diff != "" {
				t.Errorf("the fresh record (-want +got):\n%s", diff)
			}
		})
	}
}
