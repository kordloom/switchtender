package witness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// fixedClock is the time every service test stamps its findings and attestations with, so a
// comparison against a recorded value is exact rather than approximate.
var fixedClock = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// rawBody starts a feed answering exactly what body returns, with the given status.
func rawBody(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// getStatus fetches url without following redirects and returns the status and the decoded error
// message, so a test can prove a refusal names nothing an anonymous caller should not see.
func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Error
}

// TestNewServiceRefusesAndPanicsOnTheRightThings pins the constructor's two classes of refusal. A
// configuration mistake an operator can make is an error they can read; a programming error that
// would turn the watch into a busy loop or write state nowhere is a panic, because there is no
// sensible way to carry on.
func TestNewServiceRefusesAndPanicsOnTheRightThings(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	tests := []struct {
		// Dir, Interval, and Servers are the constructor arguments under test.
		Dir      string
		Interval time.Duration
		Servers  []string
		// WantPanic is whether the call must panic, WantErr whether it must return an error.
		WantPanic bool
		WantErr   bool
	}{ // Test 0: A sound configuration.
		{Dir: "state", Interval: 10 * time.Second, Servers: []string{"https://a.example"}},
		// Test 1: No state directory is a programming error; a witness with nowhere to remember is
		// not a witness.
		{Dir: "", Interval: time.Minute, Servers: []string{"https://a.example"}, WantPanic: true},
		// Test 2: One second below the floor turns the watch into a busy loop against the servers it
		// is meant to observe.
		{Dir: "state", Interval: 9 * time.Second, Servers: []string{"https://a.example"},
			WantPanic: true},
		// Test 3: Exactly the floor is allowed, so the bound is a floor and not a fence.
		{Dir: "state", Interval: 10 * time.Second, Servers: []string{"https://a.example"}},
		// Test 4: A zero interval.
		{Dir: "state", Interval: 0, Servers: []string{"https://a.example"}, WantPanic: true},
		// Test 5: A negative interval.
		{Dir: "state", Interval: -time.Hour, Servers: []string{"https://a.example"}, WantPanic: true},
		// Test 6: Nothing to watch is an operator's mistake, not a crash.
		{Dir: "state", Interval: time.Minute, Servers: nil, WantErr: true},
		// Test 7: An empty list is the same mistake.
		{Dir: "state", Interval: time.Minute, Servers: []string{}, WantErr: true},
		// Test 8: The same server twice, however spelled: two watchers over one state file take
		// turns overwriting the memory that catches a rewrite.
		{Dir: "state", Interval: time.Minute,
			Servers: []string{"https://a.example", "https://a.example/"}, WantErr: true},
		// Test 9: Three servers where two collide only after normalizing.
		{Dir: "state", Interval: time.Minute,
			Servers: []string{"https://a.example", "https://b.example", "HTTPS://A.EXAMPLE//"},
			WantErr: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := test.Dir
			if dir != "" {
				dir = filepath.Join(t.TempDir(), dir)
			}
			var panicked any
			var s *Service
			var err error
			func() {
				defer func() { panicked = recover() }()
				s, err = NewService(id, dir, test.Interval, test.Servers, nil, nil)
			}()
			if got := panicked != nil; got != test.WantPanic {
				t.Fatalf("panic = %v, want a panic = %v", panicked, test.WantPanic)
			}
			if test.WantPanic {
				return
			}
			if got := err != nil; got != test.WantErr {
				t.Fatalf("NewService() error = %v, want an error = %v", err, test.WantErr)
			}
			if err != nil {
				if s != nil {
					t.Error("NewService() returned a service alongside its error")
				}
				return
			}
			s.Close()
		})
	}
}

// TestServiceOptionsInstallWhatTheyName pins the wiring between each constructor option and the
// behavior it is supposed to change. An option that is accepted and then never consulted passes
// every test of the rule it was meant to install, so the clock, the notifier, and the token are
// each proven at the surface they act on.
func TestServiceOptionsInstallWhatTheyName(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
	dir := t.TempDir()
	id := testIdentity(t)
	var mu sync.Mutex
	var notified []RecordedFinding
	s, err := NewService(id, dir, time.Minute, []string{feed.srv.URL}, nil, feed.srv.Client(),
		WithServiceClock(func() time.Time { return fixedClock }),
		WithServiceNotify(func(server string, f Finding) {
			mu.Lock()
			defer mu.Unlock()
			notified = append(notified, RecordedFinding{Server: server, Kind: f.Kind, Detail: f.Detail})
		}),
		WithServiceReadToken("tok"))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(s.Close)

	// The clock reaches the attestation, so a signed statement carries the time the service was
	// told about rather than whatever the machine's wall clock says.
	a, err := s.Attest(ServerKey(feed.srv.URL))
	if err != nil || a == nil {
		t.Fatalf("Attest() = %v, %v", a, err)
	}
	if !a.MintedAt.Equal(fixedClock) {
		t.Errorf("attestation minted at %s, want the installed clock %s", a.MintedAt, fixedClock)
	}

	// The notifier reaches every finding, and the clock reaches the record it is built from.
	feed.set([]Beat{beat(1, 10, link("a"))}, 0)
	s.CheckAll(context.Background())
	mu.Lock()
	got := append([]RecordedFinding(nil), notified...)
	mu.Unlock()
	if len(got) != 1 || got[0].Kind != "head_regression" {
		t.Fatalf("notified = %+v, want the truncation delivered to the installed notifier", got)
	}
	if got[0].Server != NormalizeServer(feed.srv.URL) {
		t.Errorf("notified server = %q, want the normalized watched URL", got[0].Server)
	}
	recs, err := s.tail("")
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if len(recs) != 1 || !recs[0].At.Equal(fixedClock) {
		t.Errorf("recorded finding = %+v, want it stamped with the installed clock", recs)
	}

	// The token reaches the gate, so the read surface really is closed on this service.
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	if code, _ := getStatus(t, api.URL+"/witness/servers"); code != http.StatusUnauthorized {
		t.Errorf("GET /witness/servers without a token = %d, want 401", code)
	}
}

// TestOpenServiceServesEveryRouteAnonymously pins the other half of the token wiring: with no token
// configured the whole read surface is open, which is the correct single-tenant and local mode. A
// gate that closed anyway would break every local run, and one that stayed open with a token
// configured would publish the watched-server list.
func TestOpenServiceServesEveryRouteAnonymously(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	s, key := startedService(t, feed, t.TempDir(), testIdentity(t))
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	for testNum, path := range []string{
		"/healthz",
		"/witness/servers",
		"/witness/findings",
		"/witness/servers/" + key + "/checkpoint",
		"/witness/servers/" + key + "/attestation",
		"/witness/servers/" + key + "/findings",
	} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if code, msg := getStatus(t, api.URL+path); code != http.StatusOK {
				t.Errorf("GET %s = %d %q, want 200 on an ungated service", path, code, msg)
			}
		})
	}
}

// TestUnknownServerKeysAreRefusedWithoutLeakingTheArchive pins that every keyed route refuses a key
// it does not know, and that the refusal names nothing about the filesystem. The API is meant to
// face auditors who are not the operator, so a 404 that spells out a state directory hands a
// stranger the layout of the archive.
func TestUnknownServerKeysAreRefusedWithoutLeakingTheArchive(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	s, _ := startedService(t, feed, dir, testIdentity(t))
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	keys := []string{"nope", "..", "%2e%2e", strings.Repeat("k", 512), "-", "0123456789abcdef"}
	routes := []string{"checkpoint", "attestation", "findings"}
	testNum := 0
	for _, key := range keys {
		for _, route := range routes {
			path := "/witness/servers/" + key + "/" + route
			t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
				t.Parallel()
				code, msg := getStatus(t, api.URL+path)
				if code == http.StatusOK {
					t.Fatalf("GET %s = 200, want an unknown key refused", path)
				}
				if strings.Contains(msg, dir) || strings.Contains(msg, ".json") ||
					strings.Contains(msg, findingsFile) {
					t.Errorf("GET %s refused with %q, which names the archive", path, msg)
				}
			})
			testNum++
		}
	}
}

// TestCheckpointRouteRefusesAMemoryItCannotVerify pins that a state file which fails verification
// is a server error rather than a served document. The checkpoint route is where a relying party
// reads what the witness holds, so answering with an unverifiable memory would let anyone who can
// write the archive publish whatever they like under the witness's name.
func TestCheckpointRouteRefusesAMemoryItCannotVerify(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	id := testIdentity(t)
	s, key := startedService(t, feed, dir, id)
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)

	// The honest memory is served and it verifies.
	var c Checkpoint
	getJSON(t, api.URL+"/witness/servers/"+key+"/checkpoint", &c)
	if signer, err := Verify(&c); err != nil || signer != id.PublicKeyHex() {
		t.Fatalf("served checkpoint verify = %q, %v, want this witness's key", signer, err)
	}

	// Somebody with write access replaces it with a document signed by their own key.
	forged, _, err := Check(nil, feed.srv.URL, []Beat{beat(1, 10, link("a"))}, fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	state := filepath.Join(dir, StateFileName(feed.srv.URL))
	if err := Save(state, forged, testIdentity(t)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	code, msg := getStatus(t, api.URL+"/witness/servers/"+key+"/checkpoint")
	if code != http.StatusInternalServerError {
		t.Errorf("GET checkpoint over a replaced state file = %d, want 500", code)
	}
	if strings.Contains(msg, dir) || strings.Contains(msg, id.PublicKeyHex()) {
		t.Errorf("the refusal %q names the archive or the witness key", msg)
	}
}

// TestNothingWitnessedYetIsNotAnEmptyAttestation pins that a witness with no memory says so rather
// than serving a document. An attestation of nothing wearing a valid signature reads to a relying
// party as an attestation of health.
func TestNothingWitnessedYetIsNotAnEmptyAttestation(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, nil)
	feed.set(nil, http.StatusInternalServerError)
	s, key := startedService(t, feed, t.TempDir(), testIdentity(t))
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	for testNum, route := range []string{"checkpoint", "attestation"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := "/witness/servers/" + key + "/" + route
			code, msg := getStatus(t, api.URL+path)
			if code != http.StatusNotFound {
				t.Errorf("GET %s with nothing witnessed = %d %q, want 404", path, code, msg)
			}
		})
	}
	if a, err := s.Attest(key); err != nil || a != nil {
		t.Errorf("Attest() = %v, %v, want nothing to attest", a, err)
	}
	if _, err := s.Attest("no-such-key"); err == nil {
		t.Error("Attest() minted a statement about a server this witness does not watch")
	}
}

// TestFindingsAreServedPerServerAndBounded pins the two properties of the findings surface an
// auditor depends on: one server's route never carries another's findings, and no answer grows
// without bound however long the archive gets. Cross-server leakage would tell a caller who else
// this witness watches.
func TestFindingsAreServedPerServerAndBounded(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	s, key := startedService(t, feed, dir, testIdentity(t))
	mine := NormalizeServer(feed.srv.URL)

	// The archive already holds more findings than one answer may carry, for this server and for
	// another the caller must never see.
	f, err := os.OpenFile(filepath.Join(dir, findingsFile),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	for i := 0; i < findingsTail+50; i++ {
		for _, rec := range []RecordedFinding{
			{At: fixedClock, Server: mine, Kind: "rewritten_beat", Detail: fmt.Sprintf("mine %d", i)},
			{At: fixedClock, Server: "https://other.example", Kind: "rewritten_beat",
				Detail: fmt.Sprintf("theirs %d", i)},
		} {
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if _, err := f.Write(append(line, '\n')); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	mineOnly, err := s.tail(mine)
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if len(mineOnly) != findingsTail {
		t.Errorf("per-server answer carried %d findings, want the %d cap", len(mineOnly), findingsTail)
	}
	for _, rec := range mineOnly {
		if rec.Server != mine {
			t.Fatalf("the per-server answer carries %q, which is another watched server", rec.Server)
		}
	}
	// The newest are what an operator needs, so the cap drops the oldest.
	if last := mineOnly[len(mineOnly)-1].Detail; last != fmt.Sprintf("mine %d", findingsTail+49) {
		t.Errorf("newest served finding = %q, want the most recent one", last)
	}

	all, err := s.tail("")
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if len(all) != findingsTail {
		t.Errorf("cross-server answer carried %d findings, want the %d cap", len(all), findingsTail)
	}

	// The HTTP surface agrees with what tail computed.
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	var served struct {
		Findings []RecordedFinding `json:"findings"`
		Count    int               `json:"count"`
	}
	getJSON(t, api.URL+"/witness/servers/"+key+"/findings", &served)
	if served.Count != len(mineOnly) || served.Count != len(served.Findings) {
		t.Errorf("served count = %d with %d findings, want %d", served.Count,
			len(served.Findings), len(mineOnly))
	}
}

// TestFindingsAnswerIsAnEmptyListNotNull pins the shape of an answer with nothing in it. A client
// decoding null into a list and then ranging over it is the kind of break that only shows up on the
// quiet witness nobody was watching.
func TestFindingsAnswerIsAnEmptyListNotNull(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	s, key := startedService(t, feed, t.TempDir(), testIdentity(t))
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	for testNum, path := range []string{"/witness/findings", "/witness/servers/" + key + "/findings"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			res, err := http.Get(api.URL + path)
			if err != nil {
				t.Fatalf("GET %s error = %v", path, err)
			}
			defer func() { _ = res.Body.Close() }()
			var raw map[string]json.RawMessage
			if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if got := string(raw["findings"]); got != "[]" {
				t.Errorf("GET %s findings = %s, want an empty list", path, got)
			}
			if got := string(raw["count"]); got != "0" {
				t.Errorf("GET %s count = %s, want 0", path, got)
			}
		})
	}
	// tail agrees, so the emptiness is a property of the reader and not of the encoder.
	recs, err := s.tail("")
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if recs == nil {
		t.Error("tail() returned nil rather than an empty list")
	}
}

// TestTheArchiveSurvivesRubbishInTheRecord pins that lines the reader cannot parse are skipped
// rather than aborting the recount or the findings answer. The record is appended to by a live
// process, so a torn last line after a crash must not brick the restart that reads it.
func TestTheArchiveSurvivesRubbishInTheRecord(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	id := testIdentity(t)
	mine := NormalizeServer(feed.srv.URL)
	good, err := json.Marshal(RecordedFinding{At: fixedClock, Server: mine,
		Kind: "rewritten_beat", Detail: "real"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var content strings.Builder
	content.Write(good)
	content.WriteString("\n")
	content.WriteString("\n")
	content.WriteString("not json at all\n")
	content.WriteString("{\"at\": broken\n")
	content.Write(good)
	content.WriteString("\n")
	// A torn final line with no newline, exactly what a crash mid-append leaves.
	content.WriteString(string(good[:len(good)/2]))
	record := filepath.Join(dir, findingsFile)
	if err := os.WriteFile(record, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	s, key := startedService(t, feed, dir, id)
	a, err := s.Attest(key)
	if err != nil || a == nil {
		t.Fatalf("Attest() = %v, %v", a, err)
	}
	if a.FindingsTotal != 2 {
		t.Errorf("recount = %d, want the two parsable findings counted and the rubbish skipped",
			a.FindingsTotal)
	}
	recs, err := s.tail("")
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("tail() served %d findings, want the two parsable ones", len(recs))
	}
}

// TestARecordLineOverTheReaderCapFailsClosed pins that a line no reader can consume stops the
// service loudly instead of being skipped. The write side clips well below this bound, so such a
// line can only have been put there from outside, and quietly undercounting it would let anyone
// with write access to the archive erase findings from every future attestation.
func TestARecordLineOverTheReaderCapFailsClosed(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	id := testIdentity(t)
	line := append([]byte(`{"detail":"`), append([]byte(strings.Repeat("x", maxRecordLine+16)),
		[]byte(`"}`+"\n")...)...)
	if err := os.WriteFile(filepath.Join(dir, findingsFile), line, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	s, err := NewService(id, dir, time.Minute, []string{feed.srv.URL}, nil, feed.srv.Client())
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer s.Close()
	if err := s.Start(); err == nil {
		t.Error("Start() carried on over a record line it cannot read, so every attestation from " +
			"here on understates what this witness saw")
	}
	if _, err := s.tail(""); err == nil {
		t.Error("tail() served an answer over a record line it cannot read")
	}
}

// TestFindingsRouteReportsAnUnreadableArchive pins that a findings answer the service cannot
// produce is a server error rather than an empty list. An empty list reads as "this witness has
// seen nothing wrong", which is the opposite of "this witness cannot read its own record".
func TestFindingsRouteReportsAnUnreadableArchive(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where file permissions do not refuse a read")
	}
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	s, key := startedService(t, feed, dir, testIdentity(t))
	if err := os.Chmod(filepath.Join(dir, findingsFile), 0o000); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, findingsFile), 0o600) })
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	for testNum, path := range []string{"/witness/findings", "/witness/servers/" + key + "/findings"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			code, msg := getStatus(t, api.URL+path)
			if code != http.StatusInternalServerError {
				t.Errorf("GET %s over an unreadable record = %d, want 500, never an empty list",
					path, code)
			}
			if strings.Contains(msg, dir) {
				t.Errorf("the refusal %q names the archive directory", msg)
			}
		})
	}
}

// TestRecordRefusesToWriteWithNoOpenRecord pins that a finding offered to a service that was never
// started is refused rather than dropped on the floor. Silently succeeding would let a finding
// count toward an attestation that nothing on disk backs.
func TestRecordRefusesToWriteWithNoOpenRecord(t *testing.T) {
	t.Parallel()
	s, err := NewService(testIdentity(t), t.TempDir(), time.Minute,
		[]string{"https://st.example"}, nil, nil)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer s.Close()
	s.mu.Lock()
	err = s.record(RecordedFinding{At: fixedClock, Server: "https://st.example", Kind: "k",
		Detail: "d"})
	s.mu.Unlock()
	if err == nil {
		t.Error("record() reported success with no open record, so the finding vanished silently")
	}
}

// TestServerStateRoundTripsThroughTheAPI pins every field the listing answers with, so a field that
// stops being reported, or starts being reported wrong, fails here. The listing is what an operator
// reads to decide whether the witness is working at all.
func TestServerStateRoundTripsThroughTheAPI(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
	dir := t.TempDir()
	id := testIdentity(t)
	s, key := startedService(t, feed, dir, id)
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	var listing struct {
		Witness struct {
			PublicKey string `json:"public_key"`
			KeyID     string `json:"key_id"`
		} `json:"witness"`
		Servers []ServerState `json:"servers"`
	}
	getJSON(t, api.URL+"/witness/servers", &listing)
	if listing.Witness.PublicKey != id.PublicKeyHex() || listing.Witness.KeyID != id.KeyID() {
		t.Errorf("listed witness = %+v, want this witness's key and key id", listing.Witness)
	}
	if len(listing.Servers) != 1 {
		t.Fatalf("listed %d servers, want 1", len(listing.Servers))
	}
	want := ServerState{
		Server: NormalizeServer(feed.srv.URL), Key: key, LastCheck: fixedClock, Blind: false,
		FindingsTotal: 0, LastBeat: 2, LastSeq: 20, LastHead: link("b"),
	}
	got := listing.Servers[0]
	// The checkpoint's own timestamp is the real wall clock inside the watcher, so it is compared
	// for presence rather than for value.
	if got.CheckpointAt.IsZero() {
		t.Error("the listing reports no checkpoint time, so a reader cannot tell fresh memory " +
			"from stale")
	}
	got.CheckpointAt = time.Time{}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("server state mismatch (-want +got):\n%s", diff)
	}
}

// TestListingOrderFollowsTheServersAsGiven pins that the API answers stably. An operator diffing
// two listings to see what changed cannot do that if the order moves with Go's map iteration.
func TestListingOrderFollowsTheServersAsGiven(t *testing.T) {
	t.Parallel()
	servers := []string{"https://d.example", "https://a.example", "https://c.example",
		"https://b.example"}
	s, err := NewService(testIdentity(t), t.TempDir(), time.Minute, servers, nil, nil,
		WithServiceClock(func() time.Time { return fixedClock }))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer s.Close()
	want := make([]string, 0, len(servers))
	for _, raw := range servers {
		want = append(want, NormalizeServer(raw))
	}
	for round := 0; round < 20; round++ {
		got := make([]string, 0, len(servers))
		for _, st := range s.states() {
			got = append(got, st.Server)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("listing order moved on round %d (-want +got):\n%s", round, diff)
		}
	}
}

// TestBlindEdgeReportsOutageEdgesOnly pins that an outage pages once when it starts and once when
// it ends. Alerting on every failed poll trains a reader to mute the channel the real finding will
// arrive on, and never alerting hides a server going dark on its witness.
func TestBlindEdgeReportsOutageEdgesOnly(t *testing.T) {
	t.Parallel()
	boom := errors.New("the feed answered 500 Internal Server Error")
	tests := []struct {
		// Sequence is the run of check outcomes, nil for a poll that succeeded.
		Sequence []error
		// WantKinds is the finding kind produced by each outcome, empty for none.
		WantKinds []string
		// WantBlind is the state left behind.
		WantBlind bool
	}{ // Test 0: A run of healthy polls says nothing at all.
		{Sequence: []error{nil, nil, nil}, WantKinds: []string{"", "", ""}, WantBlind: false},
		// Test 1: The first failure pages and the rest of the outage is quiet.
		{Sequence: []error{boom, boom, boom},
			WantKinds: []string{"witness_blind", "", ""}, WantBlind: true},
		// Test 2: Recovery is announced once.
		{Sequence: []error{boom, nil, nil},
			WantKinds: []string{"witness_blind", "witness_seeing", ""}, WantBlind: false},
		// Test 3: A flapping feed pages on each edge, which is the point: the flapping is the news.
		{Sequence: []error{boom, nil, boom, nil},
			WantKinds: []string{"witness_blind", "witness_seeing", "witness_blind", "witness_seeing"},
			WantBlind: false},
		// Test 4: A witness that starts healthy never announces that it can see, since it always
		// could.
		{Sequence: []error{nil}, WantKinds: []string{""}, WantBlind: false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var edge BlindEdge
			got := make([]string, 0, len(test.Sequence))
			for _, err := range test.Sequence {
				f := edge.Observe(err)
				if f == nil {
					got = append(got, "")
					continue
				}
				got = append(got, f.Kind)
				if err != nil && !strings.Contains(f.Detail, err.Error()) {
					t.Errorf("blind detail = %q, want the reason named so an operator can act",
						f.Detail)
				}
			}
			if diff := cmp.Diff(test.WantKinds, got); diff != "" {
				t.Errorf("edge findings mismatch (-want +got):\n%s", diff)
			}
			if edge.Blind() != test.WantBlind {
				t.Errorf("Blind() = %v, want %v", edge.Blind(), test.WantBlind)
			}
		})
	}
}

// TestDeltaReportsAConditionOnceUntilItChanges pins the deduplication that keeps a standing
// condition from being counted once per poll. A findings total inflated by the poll interval
// overstates what the witness saw, and an attestation carrying that number is a false statement.
func TestDeltaReportsAConditionOnceUntilItChanges(t *testing.T) {
	t.Parallel()
	trunc := Finding{Kind: "head_regression", Detail: "the newest beat is 1", Key: "trunc"}
	truncLater := Finding{Kind: "head_regression", Detail: "the newest beat is 2", Key: "trunc"}
	other := Finding{Kind: "head_regression", Detail: "a different truncation", Key: "other"}
	plain := Finding{Kind: "missing_beat", Detail: "beats 3 to 7 are gone"}
	plainReworded := Finding{Kind: "missing_beat", Detail: "beats 3 to 8 are gone"}
	tests := []struct {
		// Polls is the run of finding sets each poll produced.
		Polls [][]Finding
		// WantCounts is how many findings each poll should have been recorded as fresh.
		WantCounts []int
	}{ // Test 0: Nothing is always nothing.
		{Polls: [][]Finding{nil, nil}, WantCounts: []int{0, 0}},
		// Test 1: One standing condition is one event, however many polls see it.
		{Polls: [][]Finding{{trunc}, {trunc}, {trunc}}, WantCounts: []int{1, 0, 0}},
		// Test 2: The same event reworded as the feed moves is still one event, which is what the
		// stable key is for.
		{Polls: [][]Finding{{trunc}, {truncLater}, {trunc}}, WantCounts: []int{1, 0, 0}},
		// Test 3: A different event under the same kind is news.
		{Polls: [][]Finding{{trunc}, {other}}, WantCounts: []int{1, 1}},
		// Test 4: A condition that clears and returns is reported again, because it is a new event.
		{Polls: [][]Finding{{trunc}, nil, {trunc}}, WantCounts: []int{1, 0, 1}},
		// Test 5: A finding with no stable key is identified by its full text, so a reworded one is
		// a new event.
		{Polls: [][]Finding{{plain}, {plain}, {plainReworded}}, WantCounts: []int{1, 0, 1}},
		// Test 6: Two findings in one poll both count, and neither counts again.
		{Polls: [][]Finding{{trunc, other}, {trunc, other}}, WantCounts: []int{2, 0}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var d Delta
			got := make([]int, 0, len(test.Polls))
			for _, poll := range test.Polls {
				got = append(got, len(d.Fresh(poll)))
			}
			if diff := cmp.Diff(test.WantCounts, got); diff != "" {
				t.Errorf("fresh counts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFindingKeyIdentifiesTheEvent pins what two findings have to share to be the same event. A key
// that collided across kinds would silence a real finding because an unrelated one was already
// standing.
func TestFindingKeyIdentifiesTheEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// A and B are the two findings compared.
		A Finding
		B Finding
		// WantSame is whether they must count as one event.
		WantSame bool
	}{ // Test 0: Identical findings with no key.
		{A: Finding{Kind: "k", Detail: "d"}, B: Finding{Kind: "k", Detail: "d"}, WantSame: true},
		// Test 1: The same kind with different wording and no key is two events.
		{A: Finding{Kind: "k", Detail: "d"}, B: Finding{Kind: "k", Detail: "e"}, WantSame: false},
		// Test 2: A stable key makes the wording irrelevant.
		{A: Finding{Kind: "k", Detail: "d", Key: "x"}, B: Finding{Kind: "k", Detail: "e", Key: "x"},
			WantSame: true},
		// Test 3: The same key under a different kind is a different event.
		{A: Finding{Kind: "k", Detail: "d", Key: "x"}, B: Finding{Kind: "j", Detail: "d", Key: "x"},
			WantSame: false},
		// Test 4: A keyed finding and an unkeyed one under different kinds never collide, so the
		// two forms cannot silence each other across the kinds this package raises.
		{A: Finding{Kind: "j", Detail: "x"}, B: Finding{Kind: "k", Detail: "d", Key: "x"},
			WantSame: false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := findingKey(test.A) == findingKey(test.B); got != test.WantSame {
				t.Errorf("findingKey(%+v) == findingKey(%+v) is %v, want %v",
					test.A, test.B, got, test.WantSame)
			}
		})
	}
}

// TestStateFileNamesAreSafeAndDistinct pins the two things a state file name has to be: usable as a
// filename and as an API path segment, and different for every server. A name carrying a slash or a
// traversal would let a watched server's URL decide where the witness writes its memory.
func TestStateFileNamesAreSafeAndDistinct(t *testing.T) {
	t.Parallel()
	servers := []string{
		"https://st.example",
		"https://st.example/",
		"http://st.example",
		"https://st.example:8443",
		"https://ST.EXAMPLE",
		"https://st.example/base",
		"https://other.example",
		"http://[::1]:8080",
		"https://st.example/../../etc/passwd",
		"https://üñïçø∂é.example",
		"https://" + strings.Repeat("a", 300) + ".example",
		"not a url at all",
		"",
	}
	seen := map[string]string{}
	for testNum, server := range servers {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			name := StateFileName(server)
			if strings.ContainsAny(name, `/\:?#[]@ `) {
				t.Errorf("StateFileName(%q) = %q, which is not safe as a path segment", server, name)
			}
			if strings.Contains(name, "..") {
				t.Errorf("StateFileName(%q) = %q, which can traverse out of the archive",
					server, name)
			}
			if !strings.HasSuffix(name, ".json") {
				t.Errorf("StateFileName(%q) = %q, want a .json file", server, name)
			}
			if got := ServerKey(server); got+".json" != name {
				t.Errorf("ServerKey(%q) = %q, want the state file name without its extension",
					server, got)
			}
			// The name is derived from the URL alone, so a restart lands on the same memory.
			if again := StateFileName(server); again != name {
				t.Errorf("StateFileName(%q) is not deterministic: %q then %q", server, name, again)
			}
		})
		// Two spellings of one server share a memory; two servers never do.
		normalized := NormalizeServer(server)
		name := StateFileName(server)
		if prev, ok := seen[name]; ok && prev != normalized {
			t.Errorf("%q and %q share the state file %q", prev, normalized, name)
		}
		seen[name] = normalized
	}
}

// TestFetchRefusesEveryDishonestAnswer pins the shape enforcement on the one input the watched
// operator fully controls. The feed is untrusted, so an answer that is not a bounded list of beats
// must fail the poll rather than land in the witness's memory or its findings record.
func TestFetchRefusesEveryDishonestAnswer(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	manyBeats := func(n int) []byte {
		var b strings.Builder
		b.WriteByte('[')
		for i := 1; i <= n; i++ {
			if i > 1 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"beat":%d,"at":"","seq":%d,"head":"aa"}`, i, i)
		}
		b.WriteByte(']')
		return []byte(b.String())
	}
	tests := []struct {
		// Status and Body are what the feed answers.
		Status int
		Body   []byte
		// WantErr is whether the poll must fail.
		WantErr bool
		// WantCount is how many beats the fetch must return when it succeeds.
		WantCount int
	}{ // Test 0: An honest answer.
		{Status: 200, Body: []byte(`[{"beat":1,"at":"","seq":1,"head":"aa"}]`), WantCount: 1},
		// Test 1: An empty list is honest; it is the check that decides what an empty feed means.
		{Status: 200, Body: []byte(`[]`), WantCount: 0},
		// Test 2: A JSON null decodes to no beats rather than crashing.
		{Status: 200, Body: []byte(`null`), WantCount: 0},
		// Test 3: An object where a list belongs.
		{Status: 200, Body: []byte(`{}`), WantErr: true},
		// Test 4: Not JSON at all.
		{Status: 200, Body: []byte(`<html>nope</html>`), WantErr: true},
		// Test 5: An empty body.
		{Status: 200, Body: nil, WantErr: true},
		// Test 6: A truncated list.
		{Status: 200, Body: []byte(`[{"beat":1,`), WantErr: true},
		// Test 7: Not found.
		{Status: 404, Body: []byte(`[]`), WantErr: true},
		// Test 8: A server error.
		{Status: 500, Body: []byte(`[]`), WantErr: true},
		// Test 9: An answer that looks fine but is not 200, so a maintenance page cannot read as an
		// empty chain.
		{Status: 503, Body: []byte(`[{"beat":1,"at":"","seq":1,"head":"aa"}]`), WantErr: true},
		// Test 10: Exactly the number of beats asked for.
		{Status: 200, Body: manyBeats(FeedLimit), WantCount: FeedLimit},
		// Test 11: One more than asked for is a feed serving what the witness cannot remember.
		{Status: 200, Body: manyBeats(FeedLimit + 1), WantErr: true},
		// Test 12: A head at the length bound.
		{Status: 200, Body: []byte(`[{"beat":1,"at":"","seq":1,"head":"` +
			strings.Repeat("a", maxHeadLen) + `"}]`), WantCount: 1},
		// Test 13: One byte past the bound is a payload, not a hash.
		{Status: 200, Body: []byte(`[{"beat":1,"at":"","seq":1,"head":"` +
			strings.Repeat("a", maxHeadLen+1) + `"}]`), WantErr: true},
		// Test 14: A body far past any honest feed's size, padded in a field no other bound sees.
		{Status: 200, Body: []byte(`[{"beat":1,"at":"` + strings.Repeat("a", feedBodyCap) +
			`","seq":1,"head":"aa"}]`), WantErr: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv := rawBody(t, test.Status, test.Body)
			w := NewWatcher(srv.URL, filepath.Join(t.TempDir(), "state.json"), testIdentity(t),
				srv.Client())
			beats, err := w.fetchBeats(context.Background())
			if got := err != nil; got != test.WantErr {
				t.Fatalf("fetchBeats() error = %v, want an error = %v", err, test.WantErr)
			}
			if test.WantErr {
				if beats != nil {
					t.Errorf("fetchBeats() returned %d beats alongside its error", len(beats))
				}
				return
			}
			if len(beats) != test.WantCount {
				t.Errorf("fetchBeats() returned %d beats, want %d", len(beats), test.WantCount)
			}
		})
	}
}

// TestFetchIsBoundedByItsContext pins that a poll a caller canceled stops rather than hanging on a
// feed that never answers. A hosted witness blocked forever on one server stops watching every
// other one at the exact moment that is most interesting.
func TestFetchIsBoundedByItsContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	w := NewWatcher(srv.URL, filepath.Join(t.TempDir(), "state.json"), testIdentity(t), srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := w.CheckOnce(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("CheckOnce() reported success on a canceled poll, so a fetch that never " +
				"happened would be attested as a clean watch")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CheckOnce() ignored a canceled context and is still waiting on the feed")
	}
}

// TestCheckOnceReturnsWhatItSawEvenWhenItCannotSaveIt pins the documented contract on a failed
// save. Dropping the findings because the disk was full would lose the observation at the exact
// moment durability is already degraded, which is when an operator most needs to be told.
func TestCheckOnceReturnsWhatItSawEvenWhenItCannotSaveIt(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where directory permissions do not refuse a write")
	}
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
	dir := t.TempDir()
	w := NewWatcher(feed.srv.URL, filepath.Join(dir, "state.json"), testIdentity(t), feed.srv.Client())
	if _, _, err := w.CheckOnce(context.Background()); err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	feed.set([]Beat{beat(1, 10, link("a"))}, 0)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	next, findings, err := w.CheckOnce(context.Background())
	if err == nil {
		t.Fatal("CheckOnce() hid a failed save")
	}
	if next == nil {
		t.Error("CheckOnce() dropped the checkpoint it compared, so the caller cannot attest it")
	}
	if !hasKind(findings, "head_regression") {
		t.Errorf("findings = %v, want the truncation reported even though it could not be saved",
			kindsOf(findings))
	}
}

// TestCheckOnceRefusesAReplacedStateFile pins that the pin is enforced on the watch path and not
// only in Load. A forger who can write the archive generates their own key and writes a checkpoint
// matching the truncated feed, which is internally consistent and would otherwise be believed.
func TestCheckOnceRefusesAReplacedStateFile(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	state := filepath.Join(t.TempDir(), "state.json")
	w := NewWatcher(feed.srv.URL, state, testIdentity(t), feed.srv.Client())
	if _, _, err := w.CheckOnce(context.Background()); err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	forged, _, err := Check(nil, feed.srv.URL, []Beat{beat(1, 10, link("a"))}, fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if err := Save(state, forged, testIdentity(t)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	next, findings, err := w.CheckOnce(context.Background())
	if err == nil {
		t.Fatal("CheckOnce() believed a state file signed by a key that is not this witness's")
	}
	if next != nil || findings != nil {
		t.Error("CheckOnce() returned memory built on a state file it refused")
	}
	if _, err := w.Checkpoint(); err == nil {
		t.Error("Checkpoint() served a state file signed by another key")
	}
}

// TestTheApiIsReadOnly pins that no route accepts a write. The whole product rests on a witness
// saying only what it saw, so a mutating route would be a way to tell it what it saw.
func TestTheApiIsReadOnly(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	s, key := startedService(t, feed, t.TempDir(), testIdentity(t))
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)
	paths := []string{"/witness/servers", "/witness/findings",
		"/witness/servers/" + key + "/checkpoint", "/witness/servers/" + key + "/attestation"}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	testNum := 0
	for _, path := range paths {
		for _, method := range methods {
			t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
				t.Parallel()
				req, err := http.NewRequest(method, api.URL+path, strings.NewReader("{}"))
				if err != nil {
					t.Fatalf("NewRequest() error = %v", err)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s %s error = %v", method, path, err)
				}
				_ = res.Body.Close()
				if res.StatusCode == http.StatusOK {
					t.Errorf("%s %s = 200; the witness API has no mutating surface", method, path)
				}
			})
			testNum++
		}
	}
}

// TestConcurrentReadsAgainstALiveSweep pins that the API can be read while the watch loop is
// writing. A hosted witness answers auditors on the same process that polls, so a torn read here
// would either race-detect or hand a caller a statement pairing one poll's memory with another
// poll's totals.
func TestConcurrentReadsAgainstALiveSweep(t *testing.T) {
	t.Parallel()
	feeds := make([]*fakeFeed, 0, 4)
	urls := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		f := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
		feeds = append(feeds, f)
		urls = append(urls, f.srv.URL)
	}
	s, err := NewService(testIdentity(t), t.TempDir(), 10*time.Second, urls, nil, nil,
		WithServiceClock(func() time.Time { return fixedClock }),
		WithServiceNotify(func(string, Finding) {}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(s.Close)
	api := httptest.NewServer(s.Handler())
	t.Cleanup(api.Close)

	var wg sync.WaitGroup
	// One goroutine keeps sweeping while the others read every surface the API exposes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			feeds[i%len(feeds)].set([]Beat{beat(1, 10, link("a"))}, 0)
			s.CheckAll(context.Background())
		}
	}()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = s.states()
				for _, key := range s.order {
					if _, err := s.Attest(key); err != nil {
						t.Errorf("Attest() error = %v", err)
						return
					}
				}
				res, err := http.Get(api.URL + "/witness/servers")
				if err != nil {
					t.Errorf("GET error = %v", err)
					return
				}
				_ = res.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// TestCloseIsSafeToCallTwice pins that shutting the service down twice does not panic on a closed
// record or a canceled context. A serve command with both a defer and a signal handler calls it
// twice, and a witness that panics on shutdown loses whatever the last sweep had not yet written.
func TestCloseIsSafeToCallTwice(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	s, _ := startedService(t, feed, t.TempDir(), testIdentity(t))
	s.Close()
	s.Close()
	// A sweep after shutdown still refuses to write rather than panicking on the closed record.
	s.CheckAll(context.Background())
}

// TestAttestationCarriesTheMemoryTheServiceHoldsNotTheDisk pins that a statement is minted from the
// checkpoint this process holds. After a save failure the disk is behind, and attesting the disk
// would understate what the witness actually saw at the exact moment durability is degraded.
func TestAttestationCarriesTheMemoryTheServiceHoldsNotTheDisk(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where directory permissions do not refuse a write")
	}
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
	dir := t.TempDir()
	id := testIdentity(t)
	s, key := startedService(t, feed, dir, id)

	feed.set([]Beat{beat(1, 10, link("a")), beat(2, 20, link("b")), beat(3, 30, link("c"))}, 0)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s.CheckAll(context.Background())
	a, err := s.Attest(key)
	if err != nil || a == nil {
		t.Fatalf("Attest() = %v, %v", a, err)
	}
	if a.LastBeat != 3 {
		t.Errorf("attestation reports beat %d, want the beat 3 this poll actually witnessed",
			a.LastBeat)
	}
	if signer, err := VerifyAttestation(a); err != nil || signer != id.PublicKeyHex() {
		t.Errorf("attestation verify = %q, %v, want signed by this witness", signer, err)
	}
	if a.BeatsRemembered != 3 {
		t.Errorf("attestation remembers %d beats, want 3", a.BeatsRemembered)
	}
}

// TestAttestationRoundTripsThroughJSON pins that a statement handed to a relying party over HTTP
// still verifies after decoding. Verification happens offline against a pinned key, so a field that
// does not survive the wire is a signature that fails for an honest auditor.
func TestAttestationRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	a := &Attestation{
		Server: "https://st.example", MintedAt: fixedClock, CheckpointAt: fixedClock.Add(-time.Hour),
		LastBeat: 7, LastSeq: 70, LastHead: link("h"), BeatsRemembered: 7, FindingsTotal: 2,
		Blind: true,
	}
	if err := SignAttestation(a, id); err != nil {
		t.Fatalf("SignAttestation() error = %v", err)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var back Attestation
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(*a, back); diff != "" {
		t.Errorf("attestation did not round-trip (-signed +decoded):\n%s", diff)
	}
	if signer, err := VerifyAttestation(&back); err != nil || signer != id.PublicKeyHex() {
		t.Errorf("decoded attestation verify = %q, %v, want this witness's key", signer, err)
	}
}

// TestRecordedDetailIsClippedBeforeItReachesAnyone pins that a hostile feed's text is bounded on
// every path out of the service at once: the record on disk, the answer the API serves, and the
// copy handed to the operator's notifier. A bound applied on only one of the three lets the other
// two carry a megabyte of whatever the watched server chose.
func TestRecordedDetailIsClippedBeforeItReachesAnyone(t *testing.T) {
	t.Parallel()
	// A head that is legal but long, so the finding's wording carries the feed's own bytes.
	long := strings.Repeat("Z", maxHeadLen)
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	dir := t.TempDir()
	var mu sync.Mutex
	var notified []Finding
	s, err := NewService(testIdentity(t), dir, time.Minute, []string{feed.srv.URL}, nil,
		feed.srv.Client(), WithServiceClock(func() time.Time { return fixedClock }),
		WithServiceNotify(func(_ string, f Finding) {
			mu.Lock()
			defer mu.Unlock()
			notified = append(notified, f)
		}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(s.Close)
	feed.set([]Beat{beat(1, 10, long)}, 0)
	s.CheckAll(context.Background())

	recs, err := s.tail("")
	if err != nil {
		t.Fatalf("tail() error = %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("the rewrite was not recorded")
	}
	mu.Lock()
	got := append([]Finding(nil), notified...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the rewrite was not notified")
	}
	for _, rec := range recs {
		if len(rec.Detail) > maxDetailLen+3 {
			t.Errorf("recorded detail is %d bytes, over the %d bound", len(rec.Detail), maxDetailLen)
		}
	}
	for _, f := range got {
		if len(f.Detail) > maxDetailLen+3 {
			t.Errorf("notified detail is %d bytes, over the %d bound", len(f.Detail), maxDetailLen)
		}
	}
	// The notified copy is the recorded copy, so an operator reading an alert and an auditor
	// reading the record are looking at the same text.
	if got[0].Detail != recs[len(recs)-1].Detail {
		t.Errorf("the notified detail and the recorded detail differ:\n%q\n%q",
			got[0].Detail, recs[len(recs)-1].Detail)
	}
}

// TestSeqRegressionIsRaisedWhenTheChainGetsShorter pins the detector that watches the one
// coordinate the watched server cannot renumber. Beat numbers are its own counter and it can
// restart them at will, so a chain rewound or replaced while the beat numbering keeps climbing
// presents nothing but new beat numbers. A chain only appends, so its newest position never moves
// backwards, and a newest position behind the witnessed one is a history that got shorter.
func TestSeqRegressionIsRaisedWhenTheChainGetsShorter(t *testing.T) {
	t.Parallel()
	now := fixedClock
	// The witness watches a server whose chain is already a hundred entries deep.
	honest := []Beat{beat(1, 100, link("a")), beat(2, 101, link("b")), beat(3, 102, link("c"))}
	tests := []struct {
		// Newest is the single beat the next answer serves.
		Newest Beat
		// WantKind is the finding it must raise, empty when the answer is honest.
		WantKind string
	}{ // Test 0: The chain keeps growing under a climbing beat number.
		{Newest: beat(4, 103, link("d")), WantKind: ""},
		// Test 1: The beat number climbs while the chain is shorter than it was, which is the
		// replacement a beat-number-only detector reads as healthy growth.
		{Newest: beat(4, 50, link("replaced")), WantKind: "seq_regression"},
		// Test 2: The beat number is unchanged and the chain is shorter, the same event seen from
		// the other side.
		{Newest: beat(3, 50, link("replaced")), WantKind: "seq_regression"},
		// Test 3: One position back is the narrowest regression there is.
		{Newest: beat(4, 101, link("replaced")), WantKind: "seq_regression"},
		// Test 4: A beat number that also went backwards is named by head_regression instead, so
		// the two detectors do not both report one event.
		{Newest: beat(2, 50, link("replaced")), WantKind: "head_regression"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			first, findings, err := Check(nil, "https://st.example", honest, now)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if len(findings) != 0 {
				t.Fatalf("baseline findings = %v, want none", kindsOf(findings))
			}
			next, findings, err := Check(first, "https://st.example", []Beat{test.Newest},
				now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if test.WantKind == "" {
				if len(findings) != 0 {
					t.Fatalf("an honest advance raised %v, want nothing", kindsOf(findings))
				}
				return
			}
			if !hasKind(findings, test.WantKind) {
				t.Fatalf("a chain shortened to position %d raised %v, want %s",
					test.Newest.Seq, kindsOf(findings), test.WantKind)
			}
			// No head from a poll that saw the chain move backwards is adopted, or the witness
			// signs the shortened history into its own testimony.
			if next.LastHead != link("c") || next.LastSeq != 102 {
				t.Errorf("checkpoint = seq %d head %q, want the memory kept at the witnessed tail",
					next.LastSeq, next.LastHead)
			}
		})
	}
}

// TestARepeatedBeatInsideOneAnswerIsReported pins the branch that keeps a feed serving beats out of
// order from being read as a gap. The gap arithmetic subtracts one beat number from another, so a
// repeat or a step backwards inside one answer used to be reported as a negative count of missing
// beats, which is a finding no operator can act on.
func TestARepeatedBeatInsideOneAnswerIsReported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Beats is the answer the feed serves.
		Beats []Beat
		// WantKinds is exactly the findings it must raise.
		WantKinds []string
	}{ // Test 0: The same beat twice in a row.
		{Beats: []Beat{beat(1, 1, link("a")), beat(1, 1, link("a"))},
			WantKinds: []string{"duplicate_beat"}},
		// Test 1: A step backwards.
		{Beats: []Beat{beat(1, 1, link("a")), beat(5, 5, link("e")), beat(2, 2, link("b"))},
			WantKinds: []string{"missing_beat", "duplicate_beat"}},
		// Test 2: A repeat carrying a different link is both a repeat and a contradiction, and the
		// contradiction is the one that matters.
		{Beats: []Beat{beat(1, 1, link("a")), beat(1, 2, link("forged"))},
			WantKinds: []string{"duplicate_beat", "rewritten_beat"}},
		// Test 3: An answer served entirely backwards raises one finding per out-of-order pair and
		// nothing else, since there is no memory yet for a regression to be measured against.
		{Beats: []Beat{beat(3, 3, link("c")), beat(2, 2, link("b")), beat(1, 1, link("a"))},
			WantKinds: []string{"duplicate_beat", "duplicate_beat"}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, findings, err := Check(nil, "https://st.example", test.Beats, fixedClock)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			got := kindsOf(findings)
			if diff := cmp.Diff(test.WantKinds, got, cmpopts.SortSlices(func(a, b string) bool {
				return a < b
			})); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
			for _, f := range findings {
				if f.Kind == "duplicate_beat" && strings.Contains(f.Detail, "-") {
					t.Errorf("duplicate detail = %q, want no negative count of missing beats",
						f.Detail)
				}
			}
		})
	}
}

// TestSaveReportsADestinationItCannotReplace pins that a checkpoint which cannot be put in place is
// an error the caller sees. Reporting success would leave the witness attesting from memory that
// never reached the disk, so the next restart would silently start over.
func TestSaveReportsADestinationItCannotReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The state path is a directory with something in it, so the rename into place cannot succeed.
	path := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	c, _, err := Check(nil, "https://st.example", chainBeats(2), fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if err := Save(path, c, testIdentity(t)); err == nil {
		t.Error("Save() reported success on a destination it cannot replace")
	}
}

// TestCheckOnceRefusesAMemoryOfAnotherServer pins that the one-state-file-per-server invariant is
// enforced on the watch path. A checkpoint held against a different server's feed invents findings
// from the difference between two unrelated chains and overwrites, poll by poll, the memory that
// would have caught a real rewrite of either one.
func TestCheckOnceRefusesAMemoryOfAnotherServer(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a"))})
	id := testIdentity(t)
	state := filepath.Join(t.TempDir(), "state.json")
	// A memory of a completely different server, correctly signed by this very witness.
	other, _, err := Check(nil, "https://elsewhere.example", chainBeats(3), fixedClock)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if err := Save(state, other, id); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	w := NewWatcher(feed.srv.URL, state, id, feed.srv.Client())
	next, findings, err := w.CheckOnce(context.Background())
	if err == nil {
		t.Fatal("CheckOnce() held one server's memory against another server's feed")
	}
	if next != nil || findings != nil {
		t.Error("CheckOnce() returned memory built on a checkpoint it refused")
	}
	if !strings.Contains(err.Error(), "one state file per server") {
		t.Errorf("error = %q, want it to say how to fix the configuration", err)
	}
}

// TestFetchRefusesAServerUrlItCannotRequest pins that a base URL which cannot form a request fails
// the poll rather than panicking or silently returning no beats. No beats is what an empty chain
// looks like, so a misconfiguration must never be able to wear that costume.
func TestFetchRefusesAServerUrlItCannotRequest(t *testing.T) {
	t.Parallel()
	w := NewWatcher("http://st.exa\x7fmple", filepath.Join(t.TempDir(), "state.json"),
		testIdentity(t), nil)
	beats, err := w.fetchBeats(context.Background())
	if err == nil {
		t.Fatalf("fetchBeats() accepted an unusable base URL and returned %d beats", len(beats))
	}
	if _, _, err := w.CheckOnce(context.Background()); err == nil {
		t.Error("CheckOnce() reported a clean watch of a server it cannot even address")
	}
}

// TestFetchRefusesBytesPastTheBodyCap pins the bound that stops a watched server from exhausting
// its own witness. A hosted witness OOM-killed by one hostile feed stops watching every other
// server it holds, at the exact moment that is most interesting, and a valid JSON array followed by
// megabytes of padding slips past every bound that looks only at the decoded beats.
func TestFetchRefusesBytesPastTheBodyCap(t *testing.T) {
	t.Parallel()
	body := append([]byte(`[{"beat":1,"at":"","seq":1,"head":"aa"}] `),
		[]byte(strings.Repeat("x", 2<<20))...)
	srv := rawBody(t, http.StatusOK, body)
	w := NewWatcher(srv.URL, filepath.Join(t.TempDir(), "state.json"), testIdentity(t), srv.Client())
	beats, err := w.fetchBeats(context.Background())
	if err == nil {
		t.Fatalf("fetchBeats() read a feed padded past its bound and returned %d beats", len(beats))
	}
	if !strings.Contains(err.Error(), "refusing to read it") {
		t.Errorf("error = %q, want the refusal to name the bound", err)
	}
}

// TestStartFailsClosedOnAnUnusableArchive pins that a witness which cannot set up its archive
// refuses to start. Starting anyway would mean a witness running with no durable record and no
// prior memory, which reports itself as healthy and attests nothing worth having, and the first
// finding is the worst possible moment to learn the disk was never usable.
func TestStartFailsClosedOnAnUnusableArchive(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	tests := []struct {
		// Prepare sets the archive up in the broken shape under test.
		Prepare func(t *testing.T, dir, server string)
	}{{ // Test 0: The findings record is a directory, so it can never be appended to.
		Prepare: func(t *testing.T, dir, _ string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(dir, findingsFile), 0o700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
		},
	}, { // Test 1: The findings record exists but cannot be read, so the totals an attestation
		// carries cannot be recovered and would silently reset to zero.
		Prepare: func(t *testing.T, dir, _ string) {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("running as root, where file permissions do not refuse a read")
			}
			if err := os.WriteFile(filepath.Join(dir, findingsFile), []byte("{}\n"), 0o000); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		},
	}, { // Test 2: A state file signed by somebody else's key. Starting with an empty memory here
		// is exactly the reset a truncating operator wants, so it must be refused loudly.
		Prepare: func(t *testing.T, dir, server string) {
			t.Helper()
			c, _, err := Check(nil, server, chainBeats(2), fixedClock)
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if err := Save(filepath.Join(dir, StateFileName(server)), c, testIdentity(t)); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
	}, { // Test 3: A state file that is not a checkpoint at all.
		Prepare: func(t *testing.T, dir, server string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, StateFileName(server)),
				[]byte("not a checkpoint"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			const server = "https://st.example"
			test.Prepare(t, dir, server)
			s, err := NewService(id, dir, time.Minute, []string{server}, nil, nil)
			if err != nil {
				t.Fatalf("NewService() error = %v", err)
			}
			defer s.Close()
			if err := s.Start(); err == nil {
				t.Error("Start() carried on over an archive it cannot use")
			}
			// A failed start leaves no open record behind, since the caller will not call Close on
			// a service that never started.
			s.mu.Lock()
			leaked := s.findings != nil
			s.mu.Unlock()
			if leaked {
				t.Error("Start() left the findings record open after failing")
			}
		})
	}
}

// TestTailOverAnArchiveThatDoesNotExistYet pins that reading findings before anything has been
// recorded is an empty answer rather than an error. The findings route is what an auditor polls, so
// a brand new witness must answer it rather than fail it.
func TestTailOverAnArchiveThatDoesNotExistYet(t *testing.T) {
	t.Parallel()
	s, err := NewService(testIdentity(t), filepath.Join(t.TempDir(), "unused"), time.Minute,
		[]string{"https://st.example"}, nil, nil)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer s.Close()
	recs, err := s.tail("")
	if err != nil {
		t.Fatalf("tail() over a missing archive error = %v, want an empty answer", err)
	}
	if diff := cmp.Diff([]RecordedFinding{}, recs, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("tail() mismatch (-want +got):\n%s", diff)
	}
}

// TestAFindingThatCannotBeWrittenStillCountsAndAlerts pins that a failed write degrades durability
// alone. Dropping the count as well would make the attestation understate what this witness saw,
// which is the one number a relying party cannot check for themselves.
func TestAFindingThatCannotBeWrittenStillCountsAndAlerts(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, link("a")), beat(2, 20, link("b"))})
	var mu sync.Mutex
	var notified []Finding
	s, err := NewService(testIdentity(t), t.TempDir(), time.Minute, []string{feed.srv.URL}, nil,
		feed.srv.Client(), WithServiceClock(func() time.Time { return fixedClock }),
		WithServiceNotify(func(_ string, f Finding) {
			mu.Lock()
			defer mu.Unlock()
			notified = append(notified, f)
		}))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	key := ServerKey(feed.srv.URL)
	// Closing the service closes the record while the watchers stay usable, which is the shape of
	// a record that has stopped accepting writes.
	s.Close()
	feed.set([]Beat{beat(1, 10, link("a"))}, 0)
	s.CheckAll(context.Background())

	a, err := s.Attest(key)
	if err != nil || a == nil {
		t.Fatalf("Attest() = %v, %v", a, err)
	}
	if a.FindingsTotal != 1 {
		t.Errorf("findings total = %d, want the truncation counted even though it could not be "+
			"written down", a.FindingsTotal)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notified) != 1 || notified[0].Kind != "head_regression" {
		t.Errorf("notified = %+v, want the truncation still delivered to the operator", notified)
	}
}
