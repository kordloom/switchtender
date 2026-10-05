package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/run"
)

// rotationClock is a settable clock for the issuer behind a rotation test.
type rotationClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the current reading.
	now time.Time
}

// Now returns the clock's reading.
func (c *rotationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *rotationClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// flakyKeys is a key store that fails its reads or writes when told to, so the error paths a person
// never sees in a healthy install are driven too. It keeps the sealed private half of every key it
// was asked to save, saved or not, so a leak of a key that never reached the store is caught too.
type flakyKeys struct {
	// KeyStore is the store every call reaches when it is not told to fail.
	federation.KeyStore
	// mu guards failSave, failList, and sealed.
	mu sync.Mutex
	// failSave makes every Save fail.
	failSave bool
	// failList makes every List fail.
	failList bool
	// sealed holds every non-empty sealed private half a Save was handed.
	sealed []string
}

// Save records the key's sealed private half, then fails when failSave is set and saves otherwise.
func (f *flakyKeys) Save(ctx context.Context, k *federation.Key) error {
	f.mu.Lock()
	fail := f.failSave
	if k.Sealed != "" {
		f.sealed = append(f.sealed, k.Sealed)
	}
	f.mu.Unlock()
	if fail {
		return errors.New("the database is read only")
	}
	return f.KeyStore.Save(ctx, k)
}

// seen returns every sealed private half a Save was handed.
func (f *flakyKeys) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sealed...)
}

// List fails when failList is set and lists otherwise.
func (f *flakyKeys) List(ctx context.Context) ([]*federation.Key, error) {
	f.mu.Lock()
	fail := f.failList
	f.mu.Unlock()
	if fail {
		return nil, errors.New("the database is unreachable")
	}
	return f.KeyStore.List(ctx)
}

// set changes which operations fail.
func (f *flakyKeys) set(failSave, failList bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSave, f.failList = failSave, failList
}

// rotationServer is a handler with federation on over keys and clock, the audit chain recording,
// and logs captured, with the plain text of an admin token.
type rotationServer struct {
	// h is the handler.
	h http.Handler
	// issuer is the issuer behind it.
	issuer *federation.Issuer
	// audits is the chain the server records changes in.
	audits audit.Store
	// logs holds everything the server logged.
	logs *observer.ObservedLogs
	// admin is an admin token.
	admin string
}

// newRotationServer builds a rotationServer over keys on clock.
func newRotationServer(t *testing.T, keys federation.KeyStore, clock *rotationClock) *rotationServer {
	t.Helper()
	issuer, err := federation.NewIssuer("https://st.example.com", keys, fedTestSealer,
		federation.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	if err := issuer.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	tokens := auth.NewMemStore()
	plain, tok, err := auth.New("admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	if err := tokens.Save(context.Background(), tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	core, logs := observer.New(zapcore.DebugLevel)
	audits := audit.NewMemStore()
	h := New(run.NewMemStore(), &fakeSubmitter{}, zap.New(core), WithTokens(tokens),
		WithAudit(audits), WithFederation(issuer)).Handler()
	return &rotationServer{h: h, issuer: issuer, audits: audits, logs: logs, admin: plain}
}

// call sends method path as the admin, or with no token when anonymous, and returns the recorder.
func (s *rotationServer) call(t *testing.T, method, path string, anonymous bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if !anonymous {
		req.Header.Set("Authorization", "Bearer "+s.admin)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// keyStates decodes a key listing or rotation answer into each key's state, by id.
func keyStates(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var doc struct {
		// Keys are the listed keys.
		Keys []federation.KeyInfo `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode keys: %v\n%s", err, body)
	}
	out := map[string]string{}
	for _, k := range doc.Keys {
		out[k.ID] = k.State
	}
	return out
}

// TestFederationRotationOverTheAPI drives both rotations through the routes an admin and the UI
// use: a normal rotation publishes a pending key and keeps the current one signing, a second one is
// refused with 409 until the switch, the switch happens on the clock, and an emergency rotation
// leaves only its own key published. Each rotation is on the audit chain under its own path.
func TestFederationRotationOverTheAPI(t *testing.T) {
	t.Parallel()
	clock := &rotationClock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	s := newRotationServer(t, federation.NewMemKeyStore(), clock)
	var listing federationKeysResponse
	if err := json.Unmarshal(s.call(t, http.MethodGet, "/v1/federation/keys", false).Body.Bytes(),
		&listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing.Keys) != 1 {
		t.Fatalf("keys before any rotation = %d, want the first key", len(listing.Keys))
	}
	first := listing.Keys[0].ID

	rec := s.call(t, http.MethodPost, "/v1/federation/keys/rotate", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var rotated federationRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotation: %v", err)
	}
	second := rotated.Key.ID
	if rotated.Emergency || rotated.Key.State != federation.StatePending || rotated.Key.Signing ||
		rotated.Key.ActivatedAt == nil || !rotated.Key.ActivatedAt.Equal(clock.Now().Add(24*time.Hour)) {
		t.Errorf("rotation answer = %+v, want a pending key that signs in 24 hours", rotated)
	}
	want := map[string]string{first: federation.StateSigning, second: federation.StatePending}
	if diff := cmp.Diff(want, keyStates(t, rec.Body.Bytes())); diff != "" {
		t.Errorf("states after a normal rotation (-want +got):\n%s", diff)
	}

	again := s.call(t, http.MethodPost, "/v1/federation/keys/rotate", false)
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), second) {
		t.Errorf("second rotation = %d %s, want 409 naming %s", again.Code, again.Body.String(), second)
	}

	clock.Advance(24 * time.Hour)
	want = map[string]string{first: federation.StateRetired, second: federation.StateSigning}
	got := keyStates(t, s.call(t, http.MethodGet, "/v1/federation/keys", false).Body.Bytes())
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("states a day later (-want +got):\n%s", diff)
	}

	rec = s.call(t, http.MethodPost, "/v1/federation/keys/rotate/emergency", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("emergency rotation = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var emergency federationRotateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &emergency); err != nil {
		t.Fatalf("decode emergency rotation: %v", err)
	}
	if !emergency.Emergency || emergency.Key.State != federation.StateSigning ||
		!emergency.Key.Signing {
		t.Errorf("emergency answer = %+v, want a key signing at once", emergency)
	}
	want = map[string]string{first: federation.StateRemoved, second: federation.StateRemoved,
		emergency.Key.ID: federation.StateSigning}
	if diff := cmp.Diff(want, keyStates(t, rec.Body.Bytes())); diff != "" {
		t.Errorf("states after an emergency rotation (-want +got):\n%s", diff)
	}
	var set struct {
		// Keys are the published keys.
		Keys []struct {
			// Kid is the key id.
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(s.call(t, http.MethodGet, "/.well-known/jwks.json", true).Body.Bytes(),
		&set); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(set.Keys) != 1 || set.Keys[0].Kid != emergency.Key.ID {
		t.Errorf("published after the emergency rotation = %+v, want only %s", set.Keys,
			emergency.Key.ID)
	}

	chain, err := s.audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var paths []string
	for _, e := range chain {
		if strings.HasPrefix(e.Path, "/v1/federation/") {
			paths = append(paths, e.Method+" "+e.Path)
		}
	}
	wantPaths := []string{"POST /v1/federation/keys/rotate", "POST /v1/federation/keys/rotate",
		"POST /v1/federation/keys/rotate/emergency"}
	if diff := cmp.Diff(wantPaths, paths); diff != "" {
		t.Errorf("rotations on the audit chain (-want +got):\n%s", diff)
	}
}

// TestFederationAnswersAndLogsCarryNoPrivateMaterial drives every federation route, healthy and
// failing, and pins that no answer and no log line carries any key's sealed private half or the PEM
// it opens to, or a private JSON web key parameter.
func TestFederationAnswersAndLogsCarryNoPrivateMaterial(t *testing.T) {
	t.Parallel()
	clock := &rotationClock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	keys := &flakyKeys{KeyStore: federation.NewMemKeyStore()}
	s := newRotationServer(t, keys, clock)
	var answers []string
	call := func(method, path string, anonymous bool) {
		answers = append(answers, s.call(t, method, path, anonymous).Body.String())
	}
	// Every route fails both ways before a rotation is under way, so a normal rotation reaches the
	// save it fails on, then once more while one is pending, and each rotation goes through once.
	failing := func() {
		for _, step := range []struct {
			// FailSave and FailList set the store's failures for the calls.
			FailSave, FailList bool
		}{{true, false}, {false, true}} {
			keys.set(step.FailSave, step.FailList)
			call(http.MethodGet, "/v1/federation/keys", false)
			call(http.MethodGet, "/.well-known/jwks.json", true)
			call(http.MethodGet, "/.well-known/openid-configuration", true)
			call(http.MethodPost, "/v1/federation/keys/rotate", false)
			call(http.MethodPost, "/v1/federation/keys/rotate/emergency", false)
		}
		keys.set(false, false)
	}
	failing()
	call(http.MethodPost, "/v1/federation/keys/rotate", false)
	call(http.MethodGet, "/v1/federation/keys", false)
	failing()
	call(http.MethodPost, "/v1/federation/keys/rotate/emergency", false)
	call(http.MethodGet, "/.well-known/jwks.json", true)
	var private []string
	for _, sealed := range keys.seen() {
		plain, err := fedTestSealer.Open(sealed)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		private = append(private, sealed, plain)
	}
	if len(private) < 10 {
		t.Fatalf("collected %d private values, want the sealed half and PEM of every key a save was "+
			"handed, failed saves included", len(private))
	}
	if s.logs.Len() == 0 {
		t.Fatal("the failing calls logged nothing, so the log check below checks nothing")
	}
	var logText []string
	for _, entry := range s.logs.All() {
		blob, err := json.Marshal(entry.ContextMap())
		if err != nil {
			t.Fatalf("marshal log fields: %v", err)
		}
		logText = append(logText, entry.Message+" "+string(blob))
	}
	for _, place := range []struct {
		// What names the place.
		What string
		// Texts are what it holds.
		Texts []string
	}{{"an API answer", answers}, {"a log line", logText}} {
		for _, text := range place.Texts {
			for _, p := range private {
				if strings.Contains(text, p) {
					t.Errorf("%s of %d bytes carries private key material", place.What, len(text))
				}
			}
			for _, marker := range []string{"PRIVATE KEY", `"d":`, `"p":`, `"q":`, `"dp":`,
				`"dq":`, `"qi":`} {
				if strings.Contains(text, marker) {
					t.Errorf("%s of %d bytes carries %s", place.What, len(text), marker)
				}
			}
		}
	}
}
