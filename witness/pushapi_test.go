package witness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/identity"
)

// pushEpoch is when every push witness test starts its clock.
var pushEpoch = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

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

// Set moves the clock to t.
func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Add moves the clock forward by d.
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testPolicy entitles a request by the id and term its license document names, without checking a
// license signature, which is the product's licensed policy's work. A license naming no id is
// refused.
func testPolicy(r EnrollRequest) (Entitlement, error) {
	var doc struct {
		// Claims holds the members the policy reads.
		Claims struct {
			// ID names the license.
			ID string `json:"id"`
			// Expires ends the term, empty for none.
			Expires string `json:"expires"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(r.License, &doc); err != nil || doc.Claims.ID == "" {
		return Entitlement{}, fmt.Errorf("the license names no id")
	}
	var expires time.Time
	if doc.Claims.Expires != "" {
		var err error
		if expires, err = time.Parse(time.RFC3339, doc.Claims.Expires); err != nil {
			return Entitlement{}, err
		}
	}
	return Entitlement{LicenseID: doc.Claims.ID, Expires: expires}, nil
}

// pushWitness is a running push witness under test.
type pushWitness struct {
	// svc is the service.
	svc *Service
	// id is the witness identity.
	id identity.Identity
	// dir is the state directory.
	dir string
	// clock is the service's clock.
	clock *testClock
	// api serves the service's handler.
	api *httptest.Server
}

// newPushWitness starts a push witness accepting enrollments under testPolicy on a hand-moved
// clock, with its API served. Options given override the defaults.
func newPushWitness(t *testing.T, opts ...ServiceOption) *pushWitness {
	t.Helper()
	w := &pushWitness{id: testIdentity(t), dir: t.TempDir(), clock: &testClock{now: pushEpoch}}
	w.start(t, opts...)
	return w
}

// start runs a service over the witness's directory, as a fresh process would.
func (w *pushWitness) start(t *testing.T, opts ...ServiceOption) {
	t.Helper()
	base := []ServiceOption{WithServiceClock(w.clock.Now), WithAcceptEnrollments(DefaultMaxInstalls),
		WithEnrollmentPolicy(testPolicy), WithWitnessKeys([]WitnessKey{WitnessKeyOf(w.id, pushEpoch)})}
	svc, err := NewService(w.id, w.dir, time.Minute, nil, nil, nil, append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := svc.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(svc.Close)
	w.svc = svc
	w.api = newTestServer(svc.Handler())
	t.Cleanup(w.api.Close)
}

// restart stops the service and starts another over the same directory.
func (w *pushWitness) restart(t *testing.T, opts ...ServiceOption) {
	t.Helper()
	w.api.Close()
	w.svc.Close()
	w.start(t, opts...)
}

// producer is an install under test: its signing identity, the label its key was born to, and the
// chain it pushes.
type producer struct {
	// id signs the install's enrollment and pushes.
	id identity.Identity
	// label is the install id the key was born to.
	label string
	// leaves are the chain's tree leaves.
	leaves [][]byte
}

// newProducer returns an install with a chain of n entries.
func newProducer(t *testing.T, n int64) *producer {
	t.Helper()
	id := testIdentity(t)
	return &producer{id: id, label: id.InstallID, leaves: testLeaves(t, n)}
}

// keyID returns the producer key id, the name of the install's record.
func (pr *producer) keyID() string { return pr.id.KeyID() }

// enrollment returns a signed enrollment request carrying a license with the given id and term.
func (pr *producer) enrollment(t *testing.T, licenseID string, expires time.Time) EnrollRequest {
	t.Helper()
	term := ""
	if !expires.IsZero() {
		term = expires.UTC().Format(time.RFC3339)
	}
	license, err := json.Marshal(map[string]any{"claims": map[string]any{"v": 1, "id": licenseID,
		"tier": "team", "addons": []string{"hosted-witness"}, "expires": term}, "sig": "AA=="})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	r := EnrollRequest{InstallID: pr.label, License: license, At: "2026-10-21T12:00:00Z"}
	if err := SignEnroll(&r, pr.id); err != nil {
		t.Fatalf("SignEnroll() error = %v", err)
	}
	return r
}

// push returns the signed push the producer makes for beat at size, proving from from.
func (pr *producer) push(t *testing.T, beat, size, from int64) Push {
	t.Helper()
	return pr.pushOver(t, pr.leaves, beat, size, from)
}

// pushOver returns the signed push over leaves for beat at size, proving from from, so a test can
// push a rewritten chain under the producer's key.
func (pr *producer) pushOver(t *testing.T, leaves [][]byte, beat, size, from int64) Push {
	t.Helper()
	p := testPush(t, leaves, beat, size, from)
	p.InstallID = pr.label
	if err := SignPush(&p, pr.id); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	return p
}

// enroll enrolls the producer with the witness under licenseID and fails the test otherwise.
func (w *pushWitness) enroll(t *testing.T, pr *producer, licenseID string) {
	t.Helper()
	if _, err := w.svc.Enroll(pr.enrollment(t, licenseID, time.Time{})); err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
}

// post sends v as JSON to path on the witness API and returns the status and the raw answer.
func (w *pushWitness) post(t *testing.T, path string, v any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return w.send(t, http.MethodPost, path, raw)
}

// get fetches path from the witness API and returns the status and the raw answer.
func (w *pushWitness) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	return w.send(t, http.MethodGet, path, nil)
}

// send makes one request to the witness API and returns the status and the raw answer.
func (w *pushWitness) send(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, w.api.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	res, err := testClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatalf("read %s answer: %v", path, err)
	}
	return res.StatusCode, buf.Bytes()
}

// pushHTTP posts a push for pr and decodes the answer.
func (w *pushWitness) pushHTTP(t *testing.T, pr *producer, p Push) (int, PushAnswer, []byte) {
	t.Helper()
	code, raw := w.post(t, "/v1/installs/"+pr.keyID()+"/beats", p)
	var answer PushAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("decode push answer %s: %v", raw, err)
	}
	return code, answer, raw
}

// statement fetches and verifies pr's statement over the API.
func (w *pushWitness) statement(t *testing.T, pr *producer) Statement {
	t.Helper()
	code, raw := w.get(t, "/v1/installs/"+pr.keyID()+"/attestation")
	if code != http.StatusOK {
		t.Fatalf("GET statement = %d %s, want 200", code, raw)
	}
	var st Statement
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode statement: %v", err)
	}
	if signer, err := VerifyStatement(&st); err != nil || signer != w.id.KeyID() {
		t.Fatalf("VerifyStatement() = %s, %v, want signed by this witness", signer, err)
	}
	return st
}

// installFindingsAnswer is the findings route's answer.
type installFindingsAnswer struct {
	// Findings is the tail of the record.
	Findings []RecordedFinding `json:"findings"`
	// RefusalsTotal and NotesTotal are the totals the statement carries.
	RefusalsTotal int64 `json:"refusals_total"`
	NotesTotal    int64 `json:"notes_total"`
}

// findings fetches pr's recorded findings over the API.
func (w *pushWitness) findings(t *testing.T, pr *producer) installFindingsAnswer {
	t.Helper()
	code, raw := w.get(t, "/v1/installs/"+pr.keyID()+"/findings")
	if code != http.StatusOK {
		t.Fatalf("GET findings = %d %s, want 200", code, raw)
	}
	var out installFindingsAnswer
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	return out
}

// recordDir returns the directory of pr's record.
func (w *pushWitness) recordDir(pr *producer) string {
	return filepath.Join(w.dir, installsDir, strings.TrimPrefix(pr.keyID(), "sha256:"))
}

// snapshotDir returns every file under dir with its bytes, so a test can show a request wrote
// nothing.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(path, dir)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// lineCount returns how many lines the file at path holds.
func lineCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return strings.Count(string(raw), "\n")
}

// kindsAndClasses names each finding as kind/class, in order.
func kindsAndClasses(recs []RecordedFinding) []string {
	out := make([]string, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.Kind+"/"+rec.Class)
	}
	return out
}

// TestRefusalsAndNotesAreRecordedApart is invariant 20: the witness records what it refused and
// what it noted apart, and countersigns only what it adopted. One install walks through every
// answer a push can get. A first push is countersigned with a first_root note, a push after two
// lost beats with a missing_beat note, a stale push and a malformed push are answered and recorded
// nowhere, and a push from a chain rewritten inside the witnessed tree is refused with no
// attestation. Pushes on that rewritten chain stay one refusal however often they come, and a gap
// riding on one of them is a note beside it.
func TestRefusalsAndNotesAreRecordedApart(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 20)
	w.enroll(t, pr, "lic_apart")
	rewritten := testLeaves(t, 20, 3)

	// A stale push proves from the held size while claiming a smaller tree, which no proof can
	// show, so it carries none.
	stale := testPush(t, pr.leaves, 5, 6, 0)
	stale.InstallID, stale.FromSize = pr.label, 8
	if err := SignPush(&stale, pr.id); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	malformed := pr.push(t, 5, 10, 8)
	malformed.Head = "not a link"
	if err := SignPush(&malformed, pr.id); err != nil {
		t.Fatalf("SignPush() error = %v", err)
	}
	refused := pr.pushOver(t, rewritten, 5, 10, 8)

	tests := []struct {
		// Push is what the install sends.
		Push Push
		// WantCode is the answer's status.
		WantCode int
		// WantAttested is whether the answer carries both attestations.
		WantAttested bool
		// WantRaised names the findings the answer carries, as kind/class.
		WantRaised []string
		// WantHeld is the size the answer says the witness holds, -1 for none.
		WantHeld int64
		// WantRefusals and WantNotes are the statement's totals after the push.
		WantRefusals, WantNotes int64
		// WantBeat is the statement's newest countersigned beat after the push.
		WantBeat int64
	}{{ // Test 0: The first push is countersigned and says where memory starts.
		Push: pr.push(t, 1, 2, 0), WantCode: http.StatusOK, WantAttested: true,
		WantRaised: []string{"first_root/note"}, WantHeld: -1, WantNotes: 1, WantBeat: 1,
	}, { // Test 1: Beats 2 and 3 never arrived, and the proof shows the chain intact: a note.
		Push: pr.push(t, 4, 8, 2), WantCode: http.StatusOK, WantAttested: true,
		WantRaised: []string{"missing_beat/note"}, WantHeld: -1, WantNotes: 2, WantBeat: 4,
	}, { // Test 2: A push behind the held tree is stale, answered with the size held and
		// recorded nowhere.
		Push: stale, WantCode: http.StatusConflict, WantHeld: 8, WantNotes: 2, WantBeat: 4,
	}, { // Test 3: A push out of form is an error, recorded nowhere.
		Push: malformed, WantCode: http.StatusBadRequest, WantHeld: -1, WantNotes: 2, WantBeat: 4,
	}, { // Test 4: A chain rewritten at entry 3, inside the witnessed tree, is refused.
		Push: refused, WantCode: http.StatusConflict,
		WantRaised: []string{"rewritten_history/refusal"}, WantHeld: -1, WantRefusals: 1,
		WantNotes: 2, WantBeat: 4,
	}, { // Test 5: The rewritten chain pushed again is the same refusal, not a second one.
		Push: refused, WantCode: http.StatusConflict,
		WantRaised: []string{"rewritten_history/refusal"}, WantHeld: -1, WantRefusals: 1,
		WantNotes: 2, WantBeat: 4,
	}, { // Test 6: And again.
		Push: refused, WantCode: http.StatusConflict,
		WantRaised: []string{"rewritten_history/refusal"}, WantHeld: -1, WantRefusals: 1,
		WantNotes: 2, WantBeat: 4,
	}, { // Test 7: And a third time.
		Push: refused, WantCode: http.StatusConflict,
		WantRaised: []string{"rewritten_history/refusal"}, WantHeld: -1, WantRefusals: 1,
		WantNotes: 2, WantBeat: 4,
	}, { // Test 8: The rewritten chain's next beat skips the refused one. The gap is a note beside
		// the standing refusal, which is still one.
		Push: pr.pushOver(t, rewritten, 6, 12, 8), WantCode: http.StatusConflict,
		WantRaised: []string{"missing_beat/note", "rewritten_history/refusal"}, WantHeld: -1,
		WantRefusals: 1, WantNotes: 3, WantBeat: 4,
	}}
	for testNum, test := range tests {
		code, answer, raw := w.pushHTTP(t, pr, test.Push)
		if code != test.WantCode {
			t.Fatalf("test %d: status = %d %s, want %d", testNum, code, raw, test.WantCode)
		}
		attested := answer.ClaimAttestation != nil && answer.HeadAttestation != nil
		if attested != test.WantAttested || (!attested && (answer.ClaimAttestation != nil ||
			answer.HeadAttestation != nil)) {
			t.Errorf("test %d: attested = %v, want %v: %s", testNum, attested, test.WantAttested, raw)
		}
		if diff := cmp.Diff(test.WantRaised, kindsAndClasses(answer.Findings),
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: raised (-want +got):\n%s", testNum, diff)
		}
		held := int64(-1)
		if answer.HeldSize != nil {
			held = *answer.HeldSize
		}
		if held != test.WantHeld {
			t.Errorf("test %d: held_size = %d, want %d", testNum, held, test.WantHeld)
		}
		st := w.statement(t, pr)
		if st.RefusalsTotal != test.WantRefusals || st.NotesTotal != test.WantNotes ||
			st.LastBeat != test.WantBeat {
			t.Errorf("test %d: statement refusals %d notes %d beat %d, want %d %d %d", testNum,
				st.RefusalsTotal, st.NotesTotal, st.LastBeat, test.WantRefusals, test.WantNotes,
				test.WantBeat)
		}
	}
	got := w.findings(t, pr)
	want := []string{"first_root/note", "missing_beat/note", "rewritten_history/refusal",
		"missing_beat/note"}
	if diff := cmp.Diff(want, kindsAndClasses(got.Findings)); diff != "" {
		t.Errorf("the record (-want +got):\n%s", diff)
	}
	if got.RefusalsTotal != 1 || got.NotesTotal != 3 {
		t.Errorf("findings route totals = %d and %d, want 1 and 3", got.RefusalsTotal, got.NotesTotal)
	}
	st := w.statement(t, pr)
	if st.Size != 8 || st.Root != testRoot(pr.leaves, 8) || st.LastHead != testHead(pr.leaves, 8) {
		t.Errorf("statement coordinate = size %d root %s, want the honest tree at size 8", st.Size,
			st.Root)
	}
}

// TestPushClassNamesEveryNoteKind pins which kinds are notes. Everything else is a refusal,
// including a kind this build has never heard of, so an unknown finding can never pass a verify.
func TestPushClassNamesEveryNoteKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Kind is the finding kind.
		Kind string
		// WantClass is the class it is recorded under.
		WantClass string
	}{{ // Test 0: Where memory starts.
		Kind: "first_root", WantClass: ClassNote,
	}, { // Test 1: Beats the witness never received.
		Kind: "missing_beat", WantClass: ClassNote,
	}, { // Test 2: The witness's own memory rewound.
		Kind: "memory_restored", WantClass: ClassNote,
	}, { // Test 3: The install went quiet.
		Kind: "silent_install", WantClass: ClassNote,
	}, { // Test 4: A tree that does not extend the held one.
		Kind: "rewritten_history", WantClass: ClassRefusal,
	}, { // Test 5: A beat that came back with another link.
		Kind: "rewritten_beat", WantClass: ClassRefusal,
	}, { // Test 6: A chain position behind the witnessed one.
		Kind: "seq_regression", WantClass: ClassRefusal,
	}, { // Test 7: A beat behind the witnessed one.
		Kind: "head_regression", WantClass: ClassRefusal,
	}, { // Test 8: One beat over two trees.
		Kind: "duplicate_beat", WantClass: ClassRefusal,
	}, { // Test 9: A kind nobody has named yet.
		Kind: "something_new", WantClass: ClassRefusal,
	}, { // Test 10: No kind at all.
		Kind: "", WantClass: ClassRefusal,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := pushClass(test.Kind); got != test.WantClass {
				t.Errorf("pushClass(%q) = %q, want %q", test.Kind, got, test.WantClass)
			}
		})
	}
}

// TestAnEnrolledFindingReachesTheNotifierWithItsClass pins that a finding about an enrolled install
// reaches the service's notifier carrying the class it was recorded under, so a webhook can route a
// refusal apart from a note. One install pushes a first beat, a beat after a gap, and a chain
// rewritten inside the witnessed tree, which raises two notes and a refusal.
func TestAnEnrolledFindingReachesTheNotifierWithItsClass(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var notified []string
	var servers []string
	w := newPushWitness(t, WithServiceNotify(func(server string, f Finding) {
		mu.Lock()
		defer mu.Unlock()
		servers = append(servers, server)
		notified = append(notified, f.Kind+"/"+f.Class)
	}))
	pr := newProducer(t, 20)
	w.enroll(t, pr, "lic_notify")
	rewritten := testLeaves(t, 20, 3)
	pushes := []struct {
		// Push is what the install sends.
		Push Push
		// WantCode is the answer's status.
		WantCode int
		// WantNotified names the findings the notifier heard from it, as kind/class.
		WantNotified []string
	}{{ // Test 0: The first push says where memory starts, a note.
		Push: pr.push(t, 1, 2, 0), WantCode: http.StatusOK,
		WantNotified: []string{"first_root/note"},
	}, { // Test 1: Beats 2 and 3 never arrived and the proof shows the chain intact, a note.
		Push: pr.push(t, 4, 8, 2), WantCode: http.StatusOK,
		WantNotified: []string{"missing_beat/note"},
	}, { // Test 2: A chain rewritten at entry 3, inside the witnessed tree, a refusal.
		Push: pr.pushOver(t, rewritten, 5, 10, 8), WantCode: http.StatusConflict,
		WantNotified: []string{"rewritten_history/refusal"},
	}}
	for testNum, test := range pushes {
		mu.Lock()
		before := len(notified)
		mu.Unlock()
		if code, _, raw := w.pushHTTP(t, pr, test.Push); code != test.WantCode {
			t.Fatalf("test %d: status = %d %s, want %d", testNum, code, raw, test.WantCode)
		}
		mu.Lock()
		heard := append([]string(nil), notified[before:]...)
		mu.Unlock()
		if diff := cmp.Diff(test.WantNotified, heard); diff != "" {
			t.Errorf("test %d: notified (-want +got):\n%s", testNum, diff)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, server := range servers {
		if server != pr.keyID() {
			t.Errorf("notifier heard server %q, want the install's key id %q", server, pr.keyID())
		}
	}
}

// TestARecordIsItsKey is invariant 18: a record is its key, and nothing but its key can write it.
// Another install cannot enroll under this install's id, cannot change this install's directory or
// statement by anything it does under its own record, and this install's next push is countersigned
// as if the other were not there.
func TestARecordIsItsKey(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	a, b := newProducer(t, 8), newProducer(t, 8)
	w.enroll(t, a, "lic_a")
	w.enroll(t, b, "lic_b")
	if code, _, raw := w.pushHTTP(t, a, a.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("A's first push = %d %s, want 200", code, raw)
	}
	before := snapshotDir(t, w.recordDir(a))
	_, statementBefore := w.get(t, "/v1/installs/"+a.keyID()+"/attestation")

	// B signs an enrollment naming A's id. B's key was not born to it.
	lift := EnrollRequest{InstallID: a.label, License: b.enrollment(t, "lic_b", time.Time{}).License,
		At: "2026-10-21T12:00:00Z"}
	if err := SignEnroll(&lift, b.id); err != nil {
		t.Fatalf("SignEnroll() error = %v", err)
	}
	if code, raw := w.post(t, "/v1/enroll", lift); code != http.StatusBadRequest {
		t.Errorf("B enrolling under A's id = %d %s, want 400", code, raw)
	}
	// B pushes a chain of its own, a rewrite included, under its own record.
	for i, p := range []Push{b.push(t, 1, 2, 0), b.push(t, 2, 4, 2),
		b.pushOver(t, testLeaves(t, 8, 1), 3, 6, 4)} {
		if code, _, raw := w.pushHTTP(t, b, p); code == http.StatusInternalServerError {
			t.Fatalf("B's push %d = %d %s", i, code, raw)
		}
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.recordDir(a))); diff != "" {
		t.Errorf("B changed A's record (-before +after):\n%s", diff)
	}
	if _, after := w.get(t, "/v1/installs/"+a.keyID()+"/attestation"); !bytes.Equal(statementBefore,
		after) {
		t.Errorf("A's statement changed:\n%s\n%s", statementBefore, after)
	}
	if code, answer, raw := w.pushHTTP(t, a, a.push(t, 2, 4, 2)); code != http.StatusOK ||
		answer.ClaimAttestation == nil {
		t.Errorf("A's next push = %d %s, want it countersigned", code, raw)
	}
	if st := w.statement(t, b); st.RefusalsTotal != 1 || st.KeyID != b.keyID() {
		t.Errorf("B's statement = key %s refusals %d, want B's own record with its one refusal",
			st.KeyID, st.RefusalsTotal)
	}
}

// TestAPushNamingAnotherLabelWritesNothing pins that the key's record answers only for the label
// the key enrolled under. A push signed by the right key naming the key's other born form, or
// another install's id, is refused and the state directory is untouched.
func TestAPushNamingAnotherLabelWritesNothing(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	a, b := newProducer(t, 8), newProducer(t, 8)
	w.enroll(t, a, "lic_label")
	w.enroll(t, b, "lic_label")
	if code, _, raw := w.pushHTTP(t, a, a.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	tests := []struct {
		// Label is the install id the push names.
		Label string
	}{{ // Test 0: The narrow id the same key was also born to.
		Label: identity.LegacyInstallIDFromKey(a.id.Public()),
	}, { // Test 1: Another enrolled install's id.
		Label: b.label,
	}, { // Test 2: No id at all.
		Label: "",
	}}
	for testNum, test := range tests {
		before := snapshotDir(t, w.dir)
		p := testPush(t, a.leaves, 2, 4, 2)
		p.InstallID = test.Label
		if err := SignPush(&p, a.id); err != nil {
			t.Fatalf("SignPush() error = %v", err)
		}
		if code, _, raw := w.pushHTTP(t, a, p); code != http.StatusBadRequest {
			t.Errorf("test %d: status = %d %s, want 400", testNum, code, raw)
		}
		if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
			t.Errorf("test %d: the push wrote (-before +after):\n%s", testNum, diff)
		}
	}
}

// TestAPushSignedByAnotherKeyWritesNothing is invariant 3 at the service: a push to an install's
// record that its enrolled key did not sign is refused before it reaches memory, so nobody can
// poison an install's memory by knowing its key id, and the state directory is byte-identical
// after.
func TestAPushSignedByAnotherKeyWritesNothing(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	a, intruder := newProducer(t, 8), newProducer(t, 8)
	w.enroll(t, a, "lic_sig")
	if code, _, raw := w.pushHTTP(t, a, a.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	resign := func(p Push, id identity.Identity) Push {
		if err := SignPush(&p, id); err != nil {
			t.Fatalf("SignPush() error = %v", err)
		}
		return p
	}
	honestNext := testPush(t, a.leaves, 2, 4, 2)
	honestNext.InstallID = a.label
	rewrite := testPush(t, testLeaves(t, 8, 1), 2, 4, 2)
	rewrite.InstallID = a.label
	tampered := resign(honestNext, a.id)
	tampered.Root = testRoot(testLeaves(t, 8, 4), 4)
	unsigned := honestNext
	unsigned.Switchtender = PushVersion
	tests := []struct {
		// Push is what arrives at A's record.
		Push Push
		// WantCode is the answer.
		WantCode int
	}{{ // Test 0: A's honest next beat, signed by another key.
		Push: resign(honestNext, intruder.id), WantCode: http.StatusUnauthorized,
	}, { // Test 1: A rewrite of A's chain, signed by another key.
		Push: resign(rewrite, intruder.id), WantCode: http.StatusUnauthorized,
	}, { // Test 2: A's own signature over a push edited after signing.
		Push: tampered, WantCode: http.StatusUnauthorized,
	}, { // Test 3: No signature at all.
		Push: unsigned, WantCode: http.StatusUnauthorized,
	}}
	before := snapshotDir(t, w.dir)
	_, statementBefore := w.get(t, "/v1/installs/"+a.keyID()+"/attestation")
	for testNum, test := range tests {
		code, answer, raw := w.pushHTTP(t, a, test.Push)
		if code != test.WantCode || answer.ClaimAttestation != nil {
			t.Errorf("test %d: status = %d %s, want %d and no attestation", testNum, code, raw,
				test.WantCode)
		}
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
		t.Errorf("a push not signed by the enrolled key wrote (-before +after):\n%s", diff)
	}
	if _, after := w.get(t, "/v1/installs/"+a.keyID()+"/attestation"); !bytes.Equal(statementBefore,
		after) {
		t.Errorf("the statement changed:\n%s\n%s", statementBefore, after)
	}
}

// TestARetriedPushAnswersTheSameBytes is invariant 12 at the service: a push sent again, an hour
// later, gets the byte-identical answer and the witness records nothing new, so a client that lost
// an answer is never told something new about a push it already made.
func TestARetriedPushAnswersTheSameBytes(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_retry")
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	w.clock.Add(time.Minute)
	p := pr.push(t, 2, 4, 2)
	code, _, first := w.pushHTTP(t, pr, p)
	if code != http.StatusOK {
		t.Fatalf("second push = %d %s", code, first)
	}
	record := snapshotDir(t, w.recordDir(pr))
	w.clock.Add(time.Hour)
	code, _, again := w.pushHTTP(t, pr, p)
	if code != http.StatusOK {
		t.Fatalf("retry = %d %s", code, again)
	}
	if diff := cmp.Diff(string(first), string(again)); diff != "" {
		t.Errorf("the retry answered other bytes (-first +retry):\n%s", diff)
	}
	if diff := cmp.Diff(record, snapshotDir(t, w.recordDir(pr))); diff != "" {
		t.Errorf("the retry wrote (-before +after):\n%s", diff)
	}
	if n := lineCount(t, filepath.Join(w.recordDir(pr), headsFile)); n != 2 {
		t.Errorf("head log holds %d lines, want 2", n)
	}
}

// TestLapseKeepsEveryStoredAttestation is invariant 13: pushes are accepted through the thirtieth
// day after the license term, refused as lapsed on the thirty-first with nothing written, and every
// attestation the install already holds still verifies, as does the statement.
func TestLapseKeepsEveryStoredAttestation(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	term := pushEpoch.Add(10 * 24 * time.Hour)
	if _, err := w.svc.Enroll(pr.enrollment(t, "lic_lapse", term)); err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	var held []PushAnswer
	tests := []struct {
		// At is when the push arrives.
		At time.Time
		// Push is the beat pushed.
		Push Push
		// WantCode is the answer.
		WantCode int
	}{{ // Test 0: Inside the term.
		At: pushEpoch, Push: pr.push(t, 1, 2, 0), WantCode: http.StatusOK,
	}, { // Test 1: The twenty-ninth day of grace.
		At: term.Add(29 * 24 * time.Hour), Push: pr.push(t, 2, 4, 2), WantCode: http.StatusOK,
	}, { // Test 2: The last instant of the thirtieth day.
		At: term.Add(LapseGrace), Push: pr.push(t, 3, 6, 4), WantCode: http.StatusOK,
	}, { // Test 3: The thirty-first day.
		At: term.Add(LapseGrace + 24*time.Hour), Push: pr.push(t, 4, 8, 6),
		WantCode: http.StatusPaymentRequired,
	}}
	for testNum, test := range tests {
		w.clock.Set(test.At)
		before := snapshotDir(t, w.dir)
		code, answer, raw := w.pushHTTP(t, pr, test.Push)
		if code != test.WantCode {
			t.Fatalf("test %d: status = %d %s, want %d", testNum, code, raw, test.WantCode)
		}
		if code == http.StatusOK {
			held = append(held, answer)
			continue
		}
		if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
			t.Errorf("test %d: a lapsed push wrote (-before +after):\n%s", testNum, diff)
		}
	}
	for i, answer := range held {
		if err := VerifyClaimAttestation(answer.Head, *answer.ClaimAttestation); err != nil {
			t.Errorf("stored claim attestation %d after lapse: %v", i, err)
		}
		if err := VerifyHeadAttestation(answer.Root, answer.Size, *answer.HeadAttestation); err != nil {
			t.Errorf("stored head attestation %d after lapse: %v", i, err)
		}
	}
	if st := w.statement(t, pr); st.LastBeat != 3 {
		t.Errorf("statement after lapse holds beat %d, want 3", st.LastBeat)
	}
}

// TestPushFloodIs429AndWritesNothing is invariant 14: the sixty-first push inside the burst is
// refused with a 429 and a Retry-After, writes nothing, and the same push is countersigned once the
// budget has refilled by one.
func TestPushFloodIs429AndWritesNothing(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_flood")
	first := pr.push(t, 1, 2, 0)
	for i := 0; i < pushBurst; i++ {
		if _, err := w.svc.Accept(pr.keyID(), first); err != nil {
			t.Fatalf("push %d of the burst: %v", i+1, err)
		}
	}
	before := snapshotDir(t, w.dir)
	next := pr.push(t, 2, 4, 2)
	req, err := http.NewRequest(http.MethodPost, w.api.URL+"/v1/installs/"+pr.keyID()+"/beats",
		bytes.NewReader(mustJSON(t, next)))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	res, err := testClient().Do(req)
	if err != nil {
		t.Fatalf("push past the burst: %v", err)
	}
	drain(res)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("push past the burst = %d, Retry-After %q, want 429 with one", res.StatusCode,
			res.Header.Get("Retry-After"))
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
		t.Errorf("a refused push wrote (-before +after):\n%s", diff)
	}
	w.clock.Add(pushEvery)
	if code, _, raw := w.pushHTTP(t, pr, next); code != http.StatusOK {
		t.Errorf("push after the refill = %d %s, want 200", code, raw)
	}
}

// mustJSON encodes v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return raw
}

// TestASilentInstallIsAFinding is invariant 15: an install heard from nothing for StaleAfter raises
// one silent_install note, none on the next sweep, the statement says silent and when it was last
// seen, a replay of a held push does not end the silence, and a new push does, so the next silence
// is noted anew.
func TestASilentInstallIsAFinding(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_silent")
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	tests := []struct {
		// After is how far past the first push the sweep runs.
		After time.Duration
		// WantNotes is the notes total after the sweep.
		WantNotes int64
		// WantSilent is whether the statement says silent.
		WantSilent bool
	}{{ // Test 0: Exactly StaleAfter is not yet silence.
		After: StaleAfter, WantNotes: 1,
	}, { // Test 1: Past it, one note.
		After: StaleAfter + time.Second, WantNotes: 2, WantSilent: true,
	}, { // Test 2: The next sweep notes nothing more.
		After: StaleAfter + time.Minute, WantNotes: 2, WantSilent: true,
	}, { // Test 3: Nor does one a day later.
		After: 2 * StaleAfter, WantNotes: 2, WantSilent: true,
	}}
	for testNum, test := range tests {
		w.clock.Set(pushEpoch.Add(test.After))
		w.svc.sweepInstalls()
		st := w.statement(t, pr)
		if st.NotesTotal != test.WantNotes || st.Silent != test.WantSilent ||
			st.LastSeen != stamp(pushEpoch) {
			t.Errorf("test %d: statement notes %d silent %v last_seen %s, want %d %v %s", testNum,
				st.NotesTotal, st.Silent, st.LastSeen, test.WantNotes, test.WantSilent,
				stamp(pushEpoch))
		}
	}
	got := w.findings(t, pr)
	if diff := cmp.Diff([]string{"first_root/note", "silent_install/note"},
		kindsAndClasses(got.Findings)); diff != "" {
		t.Errorf("the record (-want +got):\n%s", diff)
	}
	// A replay of the push the witness already holds is answered as before and does not end the
	// silence, or a chain cut back could stay unsilent by replaying its last held push.
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("replayed push = %d %s, want 200", code, raw)
	}
	w.svc.sweepInstalls()
	if st := w.statement(t, pr); !st.Silent || st.NotesTotal != 2 || st.LastSeen != stamp(pushEpoch) {
		t.Errorf("after a replay silent %v notes %d last_seen %s, want true, 2, and %s", st.Silent,
			st.NotesTotal, st.LastSeen, stamp(pushEpoch))
	}
	// A push ends the silence, and the silence after it is a new one.
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 2, 4, 2)); code != http.StatusOK {
		t.Fatalf("push after the silence = %d %s", code, raw)
	}
	if st := w.statement(t, pr); st.Silent {
		t.Error("the statement still says silent after a push")
	}
	w.clock.Add(StaleAfter + time.Second)
	w.svc.sweepInstalls()
	if st := w.statement(t, pr); st.NotesTotal != 3 || !st.Silent || st.RefusalsTotal != 0 {
		t.Errorf("after the second silence notes %d silent %v refusals %d, want 3 true 0",
			st.NotesTotal, st.Silent, st.RefusalsTotal)
	}
}

// TestASilentInstallIsNotTheWitnessDowntime pins that silence is measured from the witness's own
// start when that is later than the last push: a witness down for days must not record its own
// outage as every install's silence the moment it comes back.
func TestASilentInstallIsNotTheWitnessDowntime(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_outage")
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	w.clock.Add(3 * StaleAfter)
	w.restart(t)
	w.svc.sweepInstalls()
	if st := w.statement(t, pr); st.NotesTotal != 1 || !st.Silent {
		t.Errorf("after the witness's outage notes %d silent %v, want 1 and true: silent, but not "+
			"noted as the install's doing", st.NotesTotal, st.Silent)
	}
	w.clock.Add(StaleAfter + time.Second)
	w.svc.sweepInstalls()
	if st := w.statement(t, pr); st.NotesTotal != 2 {
		t.Errorf("a full StaleAfter after the restart notes %d, want 2", st.NotesTotal)
	}
}

// TestRestoredMemoryRenegotiatesTheProofBase is invariant 11 at the witness: the state directory is
// rolled back three beats from a snapshot with the restore marker beside it. The install's next
// push, proving from the size it last had acknowledged, is told the size the witness now holds, the
// push proved again from there is countersigned, memory_restored is recorded once as a note, and no
// refusal is recorded at any point.
func TestRestoredMemoryRenegotiatesTheProofBase(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 16)
	w.enroll(t, pr, "lic_restore")
	for beat := int64(1); beat <= 5; beat++ {
		if beat == 3 {
			w.svc.Close()
			copyTree(t, w.dir, w.dir+".snapshot")
			w.restart(t)
		}
		w.clock.Add(time.Minute)
		if code, _, raw := w.pushHTTP(t, pr, pr.push(t, beat, 2*beat, 2*(beat-1))); code !=
			http.StatusOK {
			t.Fatalf("beat %d = %d %s", beat, code, raw)
		}
	}
	w.api.Close()
	w.svc.Close()
	if err := os.RemoveAll(w.dir); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	copyTree(t, w.dir+".snapshot", w.dir)
	marker := filepath.Join(w.dir, RestoreMarker)
	if err := os.WriteFile(marker, []byte("snapshot\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	w.start(t)

	code, answer, raw := w.pushHTTP(t, pr, pr.push(t, 6, 12, 10))
	if code != http.StatusConflict || answer.HeldSize == nil || *answer.HeldSize != 4 ||
		answer.HeldBeat == nil || *answer.HeldBeat != 2 || answer.ClaimAttestation != nil {
		t.Fatalf("push from the lost base = %d %s, want 409 holding size 4 at beat 2", code, raw)
	}
	code, answer, raw = w.pushHTTP(t, pr, pr.push(t, 6, 12, *answer.HeldSize))
	if code != http.StatusOK || answer.ClaimAttestation == nil {
		t.Fatalf("push proved again = %d %s, want it countersigned", code, raw)
	}
	w.restart(t)
	got := w.findings(t, pr)
	want := []string{"first_root/note", "memory_restored/note", "missing_beat/note"}
	if diff := cmp.Diff(want, kindsAndClasses(got.Findings)); diff != "" {
		t.Errorf("the record (-want +got):\n%s", diff)
	}
	if st := w.statement(t, pr); st.RefusalsTotal != 0 || st.NotesTotal != 3 || st.Size != 12 {
		t.Errorf("statement refusals %d notes %d size %d, want 0, 3, and 12", st.RefusalsTotal,
			st.NotesTotal, st.Size)
	}
	if _, err := os.Stat(filepath.Join(w.dir, RestoreMarker)); !os.IsNotExist(err) {
		t.Errorf("the restore marker is still there: %v", err)
	}
}

// copyTree copies the directory tree at from to to.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(to, strings.TrimPrefix(path, from))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o750)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, raw, 0o600)
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
}

// TestAPushTheWitnessCannotWriteIsNotCountersigned pins memory before testimony: when the head log
// or the checkpoint cannot be written the push is answered 503 with no attestation and the memory
// unchanged, and the retry once the disk recovers is countersigned with one head log line.
func TestAPushTheWitnessCannotWriteIsNotCountersigned(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_disk")
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	dir, heads := w.recordDir(pr), filepath.Join(w.recordDir(pr), headsFile)
	tests := []struct {
		// Break makes one write fail and returns how to mend it.
		Break func() func()
	}{{ // Test 0: The head log cannot be appended to.
		Break: func() func() {
			if err := os.Chmod(heads, 0o400); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
			return func() { _ = os.Chmod(heads, 0o600) }
		},
	}, { // Test 1: The checkpoint cannot be replaced.
		Break: func() func() {
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
			return func() { _ = os.Chmod(dir, 0o750) }
		},
	}}
	p := pr.push(t, 2, 4, 2)
	for testNum, test := range tests {
		mend := test.Break()
		t.Cleanup(mend)
		code, answer, raw := w.pushHTTP(t, pr, p)
		mend()
		if code != http.StatusServiceUnavailable || answer.ClaimAttestation != nil {
			t.Errorf("test %d: status = %d %s, want 503 with nothing countersigned", testNum, code, raw)
		}
		if st := w.statement(t, pr); st.LastBeat != 1 {
			t.Errorf("test %d: statement holds beat %d, want 1", testNum, st.LastBeat)
		}
	}
	if code, answer, raw := w.pushHTTP(t, pr, p); code != http.StatusOK ||
		answer.ClaimAttestation == nil {
		t.Fatalf("retry on a mended disk = %d %s, want it countersigned", code, raw)
	}
	if n := lineCount(t, heads); n != 2 {
		t.Errorf("head log holds %d lines, want 2", n)
	}
}

// TestPushBodiesAreBoundedAndStrict pins how the push routes answer what is not a push: a body past
// the cap is 413, unknown members and trailing objects are 400, and a key that names no record is
// 404, all without reading any state.
func TestPushBodiesAreBoundedAndStrict(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_body")
	good := string(mustJSON(t, pr.push(t, 1, 2, 0)))
	beats := "/v1/installs/" + pr.keyID() + "/beats"
	tests := []struct {
		// Path is where the body goes.
		Path string
		// Body is what is sent.
		Body string
		// WantCode is the answer.
		WantCode int
	}{{ // Test 0: A body past the cap.
		Path: beats, Body: `{"proof":["` + strings.Repeat("a", maxPushBody) + `"]}`,
		WantCode: http.StatusRequestEntityTooLarge,
	}, { // Test 1: A member a push does not have.
		Path: beats, Body: strings.TrimSuffix(good, "}") + `,"org":"Acme"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: Two objects.
		Path: beats, Body: good + good, WantCode: http.StatusBadRequest,
	}, { // Test 3: Not JSON.
		Path: beats, Body: "beat", WantCode: http.StatusBadRequest,
	}, { // Test 4: A key id with no record.
		Path: "/v1/installs/sha256:" + strings.Repeat("0", 64) + "/beats", Body: good,
		WantCode: http.StatusNotFound,
	}, { // Test 5: Not a key id.
		Path: "/v1/installs/nope/beats", Body: good, WantCode: http.StatusNotFound,
	}, { // Test 6: An enrollment past the cap.
		Path: "/v1/enroll", Body: `{"license":"` + strings.Repeat("a", maxPushBody) + `"}`,
		WantCode: http.StatusRequestEntityTooLarge,
	}, { // Test 7: An enrollment with a member it does not have.
		Path: "/v1/enroll", Body: `{"contact":"ops@acme.example"}`, WantCode: http.StatusBadRequest,
	}}
	before := snapshotDir(t, w.dir)
	for testNum, test := range tests {
		code, raw := w.send(t, http.MethodPost, test.Path, []byte(test.Body))
		if code != test.WantCode {
			t.Errorf("test %d: status = %d %s, want %d", testNum, code, raw, test.WantCode)
		}
	}
	if diff := cmp.Diff(before, snapshotDir(t, w.dir)); diff != "" {
		t.Errorf("a refused body wrote (-before +after):\n%s", diff)
	}
	if code, raw := w.send(t, http.MethodPost, beats, []byte(good)); code != http.StatusOK {
		t.Errorf("the good push = %d %s, want 200", code, raw)
	}
}

// TestTheStatementSignatureBindsEveryField flips each member of a signed statement in turn and
// expects verification to fail, so no member can quietly become forgeable as the type grows.
func TestTheStatementSignatureBindsEveryField(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 8)
	w.enroll(t, pr, "lic_bind")
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("first push = %d %s", code, raw)
	}
	st := w.statement(t, pr)
	var fields map[string]any
	if err := json.Unmarshal(mustJSON(t, st), &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	other := testIdentity(t)
	for name, value := range fields {
		if name == "sig" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tampered := map[string]any{}
			for k, v := range fields {
				tampered[k] = v
			}
			switch v := value.(type) {
			case bool:
				tampered[name] = !v
			case float64:
				tampered[name] = v + 1
			case string:
				switch name {
				case "public_key", "witness_public_key":
					tampered[name] = other.PublicKeyBase64()
				case "key_id", "witness_key_id":
					tampered[name] = other.KeyID()
				default:
					tampered[name] = v + "0"
				}
			default:
				t.Fatalf("member %s has unhandled type %T", name, value)
			}
			var altered Statement
			if err := json.Unmarshal(mustJSON(t, tampered), &altered); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if _, err := VerifyStatement(&altered); err == nil {
				t.Errorf("VerifyStatement() accepted a change to %s", name)
			}
		})
	}
}

// TestPushRoutesAreOpenWhileTheReadTokenGatesTheRest pins the access split: with a read token set,
// the per-install routes and the key list still answer without it, by design, while every /witness
// route still refuses.
func TestPushRoutesAreOpenWhileTheReadTokenGatesTheRest(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t, WithServiceReadToken("read-token"))
	pr := newProducer(t, 8)
	if code, raw := w.post(t, "/v1/enroll", pr.enrollment(t, "lic_open", time.Time{})); code !=
		http.StatusOK {
		t.Fatalf("enroll without a token = %d %s, want 200", code, raw)
	}
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 1, 2, 0)); code != http.StatusOK {
		t.Fatalf("push without a token = %d %s, want 200", code, raw)
	}
	tests := []struct {
		// Path is the route read without a token.
		Path string
		// WantCode is the answer.
		WantCode int
	}{{ // Test 0: The install's statement is open.
		Path: "/v1/installs/" + pr.keyID() + "/attestation", WantCode: http.StatusOK,
	}, { // Test 1: Its findings are open.
		Path: "/v1/installs/" + pr.keyID() + "/findings", WantCode: http.StatusOK,
	}, { // Test 2: The key list is open.
		Path: "/.well-known/loomseal.json", WantCode: http.StatusOK,
	}, { // Test 3: The cross-server findings stay gated.
		Path: "/witness/findings", WantCode: http.StatusUnauthorized,
	}, { // Test 4: So does the server list.
		Path: "/witness/servers", WantCode: http.StatusUnauthorized,
	}}
	for testNum, test := range tests {
		if code, raw := w.get(t, test.Path); code != test.WantCode {
			t.Errorf("test %d: GET %s = %d %s, want %d", testNum, test.Path, code, raw,
				test.WantCode)
		}
	}
}

// TestAWitnessRestartKeepsEveryInstall pins that a restarted witness holds every install where the
// last process left it: the same statement byte for byte, the same totals, the same enrollment
// answer, and the next honest push countersigned.
func TestAWitnessRestartKeepsEveryInstall(t *testing.T) {
	t.Parallel()
	w := newPushWitness(t)
	pr := newProducer(t, 12)
	w.enroll(t, pr, "lic_restart")
	for i, p := range []Push{pr.push(t, 1, 2, 0), pr.push(t, 2, 4, 2),
		pr.pushOver(t, testLeaves(t, 12, 2), 3, 6, 4)} {
		w.clock.Add(time.Minute)
		if code, _, raw := w.pushHTTP(t, pr, p); code == http.StatusInternalServerError {
			t.Fatalf("push %d = %d %s", i, code, raw)
		}
	}
	_, statement := w.get(t, "/v1/installs/"+pr.keyID()+"/attestation")
	_, enrolled := w.post(t, "/v1/enroll", pr.enrollment(t, "lic_restart", time.Time{}))
	w.restart(t)
	if _, again := w.get(t, "/v1/installs/"+pr.keyID()+"/attestation"); !bytes.Equal(statement,
		again) {
		t.Errorf("the statement changed across a restart:\n%s\n%s", statement, again)
	}
	if _, again := w.post(t, "/v1/enroll", pr.enrollment(t, "lic_restart",
		time.Time{})); !bytes.Equal(enrolled, again) {
		t.Errorf("the enrollment answer changed across a restart:\n%s\n%s", enrolled, again)
	}
	if code, _, raw := w.pushHTTP(t, pr, pr.push(t, 3, 6, 4)); code != http.StatusOK {
		t.Errorf("the honest push after the restart = %d %s, want 200", code, raw)
	}
}
