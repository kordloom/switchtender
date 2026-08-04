package witness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// fakeFeed serves a mutable beat feed the way a watched server does.
type fakeFeed struct {
	// mu guards beats and status.
	mu sync.Mutex
	// beats is what the feed serves.
	beats []Beat
	// status, when nonzero, is answered instead of the beats.
	status int
	// srv is the running test server.
	srv *httptest.Server
}

// newFakeFeed starts a feed serving the given beats.
func newFakeFeed(t *testing.T, beats []Beat) *fakeFeed {
	t.Helper()
	f := &fakeFeed{beats: beats}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(f.beats)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// set replaces what the feed serves.
func (f *fakeFeed) set(beats []Beat, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats, f.status = beats, status
}

// testIdentity returns a fresh witness identity in its own directory.
func testIdentity(t *testing.T) audit.Identity {
	t.Helper()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	return id
}

func TestWatcherRoundTripAndPin(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")})
	dir := t.TempDir()
	id := testIdentity(t)
	w := NewWatcher(feed.srv.URL, filepath.Join(dir, StateFileName(feed.srv.URL)), id, feed.srv.Client())

	findings, err := w.CheckOnce(context.Background())
	if err != nil || len(findings) != 0 {
		t.Fatalf("first watch = %v, %v, want a clean baseline", findings, err)
	}
	c, err := w.Checkpoint()
	if err != nil || c == nil || c.LastBeat != 2 {
		t.Fatalf("checkpoint = %+v, %v, want the newest beat remembered", c, err)
	}

	// The chain loses its tail; the watcher reports it from its own memory.
	feed.set([]Beat{beat(1, 10, "aa")}, 0)
	findings, err = w.CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if len(findings) != 1 || findings[0].Kind != "head_regression" {
		t.Fatalf("findings = %v, want the truncation reported", findings)
	}

	// A state file replaced and re-signed by another key is refused, not believed.
	forger := testIdentity(t)
	forged, _, _ := Check(nil, feed.srv.URL, []Beat{beat(1, 10, "aa")}, time.Now())
	if err := Save(w.state, forged, forger); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := w.CheckOnce(context.Background()); err == nil {
		t.Fatal("CheckOnce() accepted a checkpoint signed by a key that is not this witness's")
	}
}

func TestServiceRefusesADuplicateServerHoweverSpelled(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	_, err := NewService(id, t.TempDir(), time.Minute,
		[]string{"https://st.example", "https://st.example/"}, nil, nil)
	if err == nil {
		t.Fatal("NewService() accepted the same server twice; two watchers over one state file " +
			"take turns overwriting the memory that catches a rewrite")
	}
}

func TestServiceStartRefusesAnUnusableArchive(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	s, err := NewService(id, filepath.Join(file, "state"), time.Minute,
		[]string{"https://st.example"}, nil, nil)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	defer s.Close()
	if err := s.Start(); err == nil {
		t.Fatal("Start() accepted an archive it cannot write, and would have learned at the " +
			"first finding, the worst possible moment")
	}
}

// startedService returns a running service over one fake feed, with its key.
func startedService(t *testing.T, feed *fakeFeed, dir string, id audit.Identity) (*Service, string) {
	t.Helper()
	s, err := NewService(id, dir, time.Minute, []string{feed.srv.URL}, nil, feed.srv.Client(),
		WithServiceClock(func() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(s.Close)
	return s, ServerKey(feed.srv.URL)
}

func TestServiceWitnessesRecordsAndAttests(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")})
	dir := t.TempDir()
	id := testIdentity(t)
	s, key := startedService(t, feed, dir, id)
	s.CheckAll(context.Background())

	api := httptest.NewServer(s.Handler())
	defer api.Close()

	// The server list names the witness key and the watched server's state.
	var listing struct {
		Witness struct {
			PublicKey string `json:"public_key"`
		} `json:"witness"`
		Servers []ServerState `json:"servers"`
	}
	getJSON(t, api.URL+"/witness/servers", &listing)
	if listing.Witness.PublicKey != id.PublicKeyHex() {
		t.Errorf("listed key = %s, want the witness key", listing.Witness.PublicKey)
	}
	if len(listing.Servers) != 1 || listing.Servers[0].Key != key ||
		listing.Servers[0].LastBeat != 2 || listing.Servers[0].Blind {
		t.Fatalf("servers = %+v, want the watched server seen at beat 2", listing.Servers)
	}

	// The checkpoint endpoint serves the signed memory, and it verifies.
	var c Checkpoint
	getJSON(t, api.URL+"/witness/servers/"+key+"/checkpoint", &c)
	if signer, err := Verify(&c); err != nil || signer != id.PublicKeyHex() {
		t.Fatalf("served checkpoint verify = %s, %v, want signed by this witness", signer, err)
	}

	// The chain loses its tail: the finding is recorded, served, and counted in the attestation.
	feed.set([]Beat{beat(1, 10, "aa")}, 0)
	s.CheckAll(context.Background())

	var found struct {
		Findings []RecordedFinding `json:"findings"`
		Count    int               `json:"count"`
	}
	getJSON(t, api.URL+"/witness/servers/"+key+"/findings", &found)
	if found.Count != 1 || found.Findings[0].Kind != "head_regression" {
		t.Fatalf("findings = %+v, want the truncation on record", found)
	}

	var a Attestation
	getJSON(t, api.URL+"/witness/servers/"+key+"/attestation", &a)
	if signer, err := VerifyAttestation(&a); err != nil || signer != id.PublicKeyHex() {
		t.Fatalf("attestation verify = %s, %v, want signed by this witness", signer, err)
	}
	if a.FindingsTotal != 1 || a.LastBeat != 2 {
		t.Errorf("attestation = %+v, want one finding and the memory at beat 2, which the "+
			"truncated feed cannot take back", a)
	}

	// A relying party alters one field: the signature no longer verifies.
	tampered := a
	tampered.FindingsTotal = 0
	if _, err := VerifyAttestation(&tampered); err == nil {
		t.Fatal("VerifyAttestation() accepted an altered attestation")
	}
}

func TestServiceRestartKeepsItsTotals(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, "aa"), beat(2, 20, "bb")})
	dir := t.TempDir()
	id := testIdentity(t)
	s, key := startedService(t, feed, dir, id)

	// A standing truncation is recorded when it appears, and not again on the next poll: one
	// event, one record, whatever the poll count.
	feed.set([]Beat{beat(1, 10, "aa")}, 0)
	s.CheckAll(context.Background())
	s.CheckAll(context.Background())
	if a, err := s.Attest(key); err != nil || a == nil || a.FindingsTotal != 1 {
		t.Fatalf("Attest() = %+v, %v, want one record for one standing truncation", a, err)
	}
	s.Close()

	// Each restarted process recounts the record and re-attests the condition it still sees,
	// exactly once. Totals must climb 1, 2, 3 across the restarts: a reset to zero means the
	// recount is broken, and a total that fails to climb means the reopened record clobbered
	// history from offset zero instead of appending to it.
	for round, want := range []int64{2, 3} {
		restarted, err := NewService(id, dir, time.Minute,
			[]string{feed.srv.URL}, nil, feed.srv.Client())
		if err != nil {
			t.Fatalf("restart %d: NewService() error = %v", round, err)
		}
		if err := restarted.Start(); err != nil {
			t.Fatalf("restart %d: Start() error = %v", round, err)
		}
		a, err := restarted.Attest(key)
		restarted.Close()
		if err != nil || a == nil {
			t.Fatalf("restart %d: Attest() = %v, %v", round, a, err)
		}
		if a.FindingsTotal != want {
			t.Errorf("restart %d: findings = %d, want %d", round, a.FindingsTotal, want)
		}
	}
}

func TestServiceAttestsBlindnessRatherThanGoingQuiet(t *testing.T) {
	t.Parallel()
	feed := newFakeFeed(t, []Beat{beat(1, 10, "aa")})
	dir := t.TempDir()
	id := testIdentity(t)
	var notified []Finding
	s, err := NewService(id, dir, time.Minute, []string{feed.srv.URL}, nil, feed.srv.Client(),
		WithServiceNotify(func(_ string, f Finding) { notified = append(notified, f) }))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()
	key := ServerKey(feed.srv.URL)
	s.CheckAll(context.Background())

	// The feed goes dark. The attestation says so, signed: a server going dark on its witness is
	// itself a signal, and an attestation that hid it would read as ordinary health.
	feed.set(nil, http.StatusInternalServerError)
	s.CheckAll(context.Background())
	a, err := s.Attest(key)
	if err != nil || a == nil {
		t.Fatalf("Attest() = %v, %v", a, err)
	}
	if !a.Blind {
		t.Error("attestation hides that the witness cannot currently see the feed")
	}
	if a.LastBeat != 1 {
		t.Errorf("attestation memory = beat %d, want the memory kept through the outage", a.LastBeat)
	}

	// The outage pages on its edges, not on every poll.
	s.CheckAll(context.Background())
	blinds := 0
	for _, f := range notified {
		if f.Kind == "witness_blind" {
			blinds++
		}
	}
	if blinds != 1 {
		t.Errorf("witness_blind notifications = %d, want one per outage, not one per poll", blinds)
	}
	feed.set([]Beat{beat(1, 10, "aa")}, 0)
	s.CheckAll(context.Background())
	seeing := 0
	for _, f := range notified {
		if f.Kind == "witness_seeing" {
			seeing++
		}
	}
	if seeing != 1 {
		t.Errorf("witness_seeing notifications = %d, want the recovery announced once", seeing)
	}
}

func TestServiceAttestationNeedsAMemoryFirst(t *testing.T) {
	t.Parallel()
	// The feed is dark from the first sweep, so the witness holds no memory of this server.
	feed := newFakeFeed(t, nil)
	feed.set(nil, http.StatusInternalServerError)
	id := testIdentity(t)
	s, key := startedService(t, feed, t.TempDir(), id)
	// Nothing has been witnessed: an attestation of nothing is noise wearing a signature.
	a, err := s.Attest(key)
	if err != nil || a != nil {
		t.Errorf("Attest() with nothing witnessed = %v, %v, want nothing to attest", a, err)
	}
}

func TestStateFileNamesNeverCollide(t *testing.T) {
	t.Parallel()
	a := StateFileName("https://st.example")
	b := StateFileName("http://st.example")
	if a == b {
		t.Error("two different servers share a state file")
	}
	if StateFileName("https://st.example") != StateFileName("https://st.example/") {
		t.Error("one server spelled two ways gets two memories")
	}
	if strings.ContainsAny(ServerKey("https://st.example:8443/x"), "/:") {
		t.Error("a server key carries characters that break a URL path")
	}
}

// getJSON fetches url and decodes it into v.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// TestAttestationSignatureCoversEveryField flips each field in turn and expects verification to
// fail, so no field can quietly become forgeable when the struct grows.
func TestAttestationSignatureCoversEveryField(t *testing.T) {
	t.Parallel()
	id := testIdentity(t)
	a := &Attestation{
		Server: "https://st.example", MintedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		CheckpointAt: time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC),
		LastBeat:     7, LastSeq: 70, LastHead: "aa", BeatsRemembered: 7, FindingsTotal: 2,
		Blind: false,
	}
	if err := SignAttestation(a, id); err != nil {
		t.Fatalf("SignAttestation() error = %v", err)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for name, value := range fields {
		if name == "public_key" || name == "sig" {
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
				if _, err := time.Parse(time.RFC3339, v); err == nil {
					tampered[name] = "2001-01-01T00:00:00Z"
				} else {
					tampered[name] = v + "x"
				}
			default:
				t.Fatalf("field %s has unhandled type %T", name, value)
			}
			doc, err := json.Marshal(tampered)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			var altered Attestation
			if err := json.Unmarshal(doc, &altered); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if _, err := VerifyAttestation(&altered); err == nil {
				t.Errorf("VerifyAttestation() accepted a change to %s; that field is forgeable", name)
			}
		})
	}
}
