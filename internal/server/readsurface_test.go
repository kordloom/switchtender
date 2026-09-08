package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// withActor returns a request carrying the given authenticated caller, the way the gate hands one
// to a handler.
func withActor(t *testing.T, method, path string, a Actor) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	return req.WithContext(context.WithValue(req.Context(), actorKey{}, a))
}

// restrictedAuthz returns an authorizer under strict grants where the caller may use only the named
// inventory, so a handler's read filter has something real to narrow against.
func restrictedAuthz(t *testing.T, subject, object string) *authorizer {
	t.Helper()
	return &authorizer{
		strict: true,
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			object: {{Subject: subject, Access: grant.AccessUse}},
		}},
	}
}

// TestWorkersListIsWithheldFromACallerWhoMayReadNothing pins the refusal on the executor list. The
// list names who is running what across the whole estate, with no run id on the rows to filter by,
// so the only correct answer for a caller who may read none of that work is to show nothing. The
// handler's own comment says so and nothing was exercising it.
func TestWorkersListIsWithheldFromACallerWhoMayReadNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	claimed := time.Now()
	theirs := &run.Run{
		ID: "run_theirs", Playbook: "theirs.yml", InventoryID: "inv_theirs",
		Status: run.StatusRunning, CreatedAt: claimed, ClaimedBy: "worker-in-prod",
		ClaimedAt: &claimed,
	}
	if err := store.Save(ctx, theirs); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	tests := []struct {
		Name      string
		Actor     Actor
		Authz     *authorizer
		WantCount int
		WantNames bool
	}{{ // Test 0: A caller granted nothing sees no executors at all.
		Name:  "granted nothing",
		Actor: Actor{UserID: "user_nobody", Role: user.RoleViewer},
		Authz: restrictedAuthz(t, "user_granted", "inv_theirs"),
	}, { // Test 1: A caller who may read the work behind the leases sees the executors.
		Name:      "granted the inventory",
		Actor:     Actor{UserID: "user_granted", Role: user.RoleViewer},
		Authz:     restrictedAuthz(t, "user_granted", "inv_theirs"),
		WantCount: 1, WantNames: true,
	}, { // Test 2: An admin under strict grants reads the estate, since the filter keeps everything
		// for a caller grants are not enforced against.
		Name:      "admin",
		Actor:     Actor{UserID: "user_admin", Role: user.RoleAdmin},
		Authz:     restrictedAuthz(t, "user_granted", "inv_theirs"),
		WantCount: 1, WantNames: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			workersHandler(store, test.Authz, zap.NewNop()).ServeHTTP(rec,
				withActor(t, http.MethodGet, "/v1/workers", test.Actor))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 (%q)", test.Name, rec.Code, rec.Body.String())
			}
			var got workersResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode workers: %v", err)
			}
			if diff := cmp.Diff(test.WantCount, got.Count); diff != "" {
				t.Errorf("%s: count mismatch (-want +got):\n%s", test.Name, diff)
			}
			named := strings.Contains(rec.Body.String(), "worker-in-prod")
			if named != test.WantNames {
				t.Errorf("%s: executor named = %v, want %v (body %q)",
					test.Name, named, test.WantNames, rec.Body.String())
			}
		})
	}
}

// TestHostFactsAreRefusedWhenTheGatheringRunIsNot pins that a host's facts follow the run that
// gathered them. Facts describe a machine in detail, the run they came from is named on them, and a
// caller who may not read that run may not read what it learned about the host. The refusal is a
// not-found rather than a forbidden, so the endpoint does not confirm the host exists.
func TestHostFactsAreRefusedWhenTheGatheringRunIsNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	gathering := &run.Run{
		ID: "run_secret", Playbook: "facts.yml", InventoryID: "inv_theirs",
		Status: run.StatusSucceeded, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, gathering); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := store.SaveHostFacts(ctx, "run_secret", []run.HostFacts{{
		Host: "db01", Facts: map[string]string{"distribution": "Debian"}, RunID: "run_secret",
	}}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}

	tests := []struct {
		Name       string
		Host       string
		Actor      Actor
		WantStatus int
		WantFacts  bool
	}{{ // Test 0: A caller with no grant on the gathering run's inventory is refused, and told only
		// that no facts were gathered.
		Name: "ungranted caller", Host: "db01",
		Actor:      Actor{UserID: "user_nobody", Role: user.RoleViewer},
		WantStatus: http.StatusNotFound,
	}, { // Test 1: The granted caller reads the facts.
		Name: "granted caller", Host: "db01",
		Actor:      Actor{UserID: "user_granted", Role: user.RoleViewer},
		WantStatus: http.StatusOK, WantFacts: true,
	}, { // Test 2: A host that was never gathered is a not found, the same answer a refused one
		// gets, so the two cannot be told apart.
		Name: "never gathered", Host: "web99",
		Actor:      Actor{UserID: "user_granted", Role: user.RoleViewer},
		WantStatus: http.StatusNotFound,
	}, { // Test 3: An empty host name is not a wildcard, it is a host nobody has facts for.
		Name: "empty host", Host: "",
		Actor:      Actor{UserID: "user_granted", Role: user.RoleViewer},
		WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			authz := restrictedAuthz(t, "user_granted", "inv_theirs")
			req := withActor(t, http.MethodGet, "/v1/hosts/x/facts", test.Actor)
			req.SetPathValue("host", test.Host)
			rec := httptest.NewRecorder()
			hostFactsHandler(store, authz, zap.NewNop()).ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: status = %d, want %d (%q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
			if got := strings.Contains(rec.Body.String(), "Debian"); got != test.WantFacts {
				t.Errorf("%s: facts disclosed = %v, want %v", test.Name, got, test.WantFacts)
			}
		})
	}
}

// TestTaskTrendsAreWithheldFromACallerWhoMayReadNothing pins the same refusal on the task trend
// view. Task names and their durations describe work with no run id on the rows to check, so the
// whole view is withheld from a caller who can read none of it rather than filtered row by row.
func TestTaskTrendsAreWithheldFromACallerWhoMayReadNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx, &run.Run{
		ID: "run_theirs", Playbook: "theirs.yml", InventoryID: "inv_theirs",
		Status: run.StatusSucceeded, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	authz := restrictedAuthz(t, "user_granted", "inv_theirs")

	rec := httptest.NewRecorder()
	taskTrendsHandler(store, authz, zap.NewNop()).ServeHTTP(rec,
		withActor(t, http.MethodGet, "/v1/tasks",
			Actor{UserID: "user_nobody", Role: user.RoleViewer}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	var got taskTrendsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode task trends: %v", err)
	}
	if got.Count != 0 || len(got.Tasks) != 0 {
		t.Errorf("a caller who may read nothing was shown %d tasks", got.Count)
	}
	if got.Window <= 0 {
		t.Errorf("window = %d, want the requested window echoed back", got.Window)
	}
}

// TestTrustDocumentIsPublicAndCarriesNoSecret pins the one deliberately unauthenticated endpoint
// that exists for a party with no account here. A relying party checking a bundle fetches it once
// and pins the fingerprint, which is what lets them say a bundle came from this install rather than
// from anyone who owns a keypair. It must carry the public half and nothing else.
func TestTrustDocumentIsPublicAndCarriesNoSecret(t *testing.T) {
	// No t.Parallel: the identity loader reads an environment variable this test sets.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithProducerIdentity(&id, "v-test")).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/loomseal.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	var doc trustDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode trust document: %v", err)
	}
	if doc.InstallID != id.InstallID {
		t.Errorf("install_id = %q, want %q", doc.InstallID, id.InstallID)
	}
	if doc.KeyID != id.KeyID() || doc.KeyID == "" {
		t.Errorf("key_id = %q, want the identity's fingerprint %q", doc.KeyID, id.KeyID())
	}
	if doc.PublicKey != id.PublicKeyBase64() || doc.PublicKey == "" {
		t.Error("public_key does not match the identity's public key")
	}
	if doc.ProductVersion != "v-test" {
		t.Errorf("product_version = %q, want v-test", doc.ProductVersion)
	}
	if doc.Format == "" || doc.ChainProfile == "" {
		t.Errorf("the document does not name its format and chain profile: %+v", doc)
	}
	// The signing key must never leave the process. Its base64 form appears; nothing else may.
	body := rec.Body.String()
	for _, forbidden := range []string{"PRIVATE", "private_key", "seed"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the trust document mentions %q: %s", forbidden, body)
		}
	}
}

// TestTrustDocumentIsCacheable demonstrates that the trust handler's caching never takes effect.
//
// The handler sets "public, max-age=300" and then calls respondJSON, which unconditionally Sets
// Cache-Control to no-store for every JSON reply. Set replaces rather than appends, so the handler's
// header is overwritten and the document it deliberately made cacheable is served uncacheable. The
// direction is safe, but the stated behavior does not hold: a relying party polling the endpoint is
// answered from scratch every time, which is exactly what the comment says it should not be.
func TestTrustDocumentIsCacheable(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	rec := httptest.NewRecorder()
	trustHandler(&id, "v-test", zap.NewNop()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/.well-known/loomseal.json", nil))
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
		t.Errorf("Cache-Control = %q, want the public max-age the handler sets", got)
	}
}

// TestTrustDocumentIsNotFoundWithoutAnIdentity pins that an install with no signing identity says so
// plainly instead of serving an empty document. A document with blank fields would verify nothing
// while looking like an answer, which is worse than no answer.
func TestTrustDocumentIsNotFoundWithoutAnIdentity(t *testing.T) {
	t.Parallel()
	rec := serveWith(t, http.MethodGet, "/.well-known/loomseal.json", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 without a signing identity", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "signing identity") {
		t.Errorf("body = %q, want it to say the install has no signing identity", rec.Body.String())
	}
}

// TestStreamTicketHandler pins the mint endpoint that replaced a bearer token in the stream URL. A
// ticket opens one run, once, for thirty seconds, and an install running open has no actor to bind
// one to and needs no ticket, so it says the mechanism is not in use rather than minting something
// bound to nobody.
func TestStreamTicketHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Actor      Actor
		HasActor   bool
		RunID      string
		WantStatus int
	}{{ // Test 0: An install running open has nobody to bind a ticket to.
		Name: "no actor", HasActor: false, RunID: "run_1", WantStatus: http.StatusNotFound,
	}, { // Test 1: An authenticated caller gets a ticket for the run in the path.
		Name: "authenticated", Actor: Actor{UserID: "user_1", Role: user.RoleViewer},
		HasActor: true, RunID: "run_1", WantStatus: http.StatusCreated,
	}, { // Test 2: A caller with only a credential label is still an actor and still gets a ticket.
		Name: "label only actor", Actor: Actor{Name: "ci-token", Role: user.RoleViewer},
		HasActor: true, RunID: "run_1", WantStatus: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tickets := newStreamTickets()
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+test.RunID+"/stream-ticket", nil)
			if test.HasActor {
				req = req.WithContext(context.WithValue(req.Context(), actorKey{}, test.Actor))
			}
			req.SetPathValue("id", test.RunID)
			rec := httptest.NewRecorder()
			streamTicketHandler(tickets, zap.NewNop()).ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: status = %d, want %d (%q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
			if test.WantStatus != http.StatusCreated {
				return
			}
			var body struct {
				// Ticket is the minted secret.
				Ticket string `json:"ticket"`
				// ExpiresIn is its lifetime in seconds.
				ExpiresIn int `json:"expires_in"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode ticket: %v", err)
			}
			if body.Ticket == "" {
				t.Fatal("no ticket value returned")
			}
			if body.ExpiresIn != int(streamTicketTTL.Seconds()) {
				t.Errorf("expires_in = %d, want %d", body.ExpiresIn,
					int(streamTicketTTL.Seconds()))
			}
			// The ticket replays the caller who minted it, so authorization on the stream is
			// unchanged, and it is spent by the first redemption.
			got, ok := tickets.redeem(body.Ticket, test.RunID)
			if !ok {
				t.Fatal("the minted ticket did not redeem against its own run")
			}
			if diff := cmp.Diff(test.Actor, got); diff != "" {
				t.Errorf("%s: replayed actor mismatch (-want +got):\n%s", test.Name, diff)
			}
			if _, ok := tickets.redeem(body.Ticket, test.RunID); ok {
				t.Error("the ticket redeemed a second time, so it is replayable")
			}
		})
	}
}

// TestTicketForOneRunOpensNoOther pins the narrowest thing a ticket is for. It stands in the query
// string, where every reverse proxy logs it, so the guarantee that it opens exactly one run is what
// makes it worth almost nothing to whoever reads that log.
//
// It also pins that presenting a ticket against the wrong run spends it. That is deliberate: an
// attacker holding a ticket gets one attempt rather than a free enumeration of run ids, and the
// legitimate holder simply mints another.
func TestTicketForOneRunOpensNoOther(t *testing.T) {
	t.Parallel()
	tickets := newStreamTickets()
	actor := Actor{UserID: "user_1", Role: user.RoleViewer}
	value, err := tickets.mint(actor, "run_mine")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := tickets.redeem(value, "run_theirs"); ok {
		t.Fatal("a ticket for one run redeemed against another")
	}
	if _, ok := tickets.redeem(value, "run_mine"); ok {
		t.Error("a ticket presented against the wrong run survived to be used on the right one")
	}

	// An empty ticket and an empty run are both refused outright, so a request that carries neither
	// cannot fall through into a lookup.
	fresh, err := tickets.mint(actor, "run_mine")
	if err != nil {
		t.Fatalf("mint again: %v", err)
	}
	if _, ok := tickets.redeem("", "run_mine"); ok {
		t.Error("an empty ticket value redeemed")
	}
	if _, ok := tickets.redeem(fresh, ""); ok {
		t.Error("a ticket redeemed against an empty run id")
	}
	if _, ok := tickets.redeem("not-a-real-ticket", "run_mine"); ok {
		t.Error("an invented ticket value redeemed")
	}
	// None of those refusals may have consumed the real ticket.
	if _, ok := tickets.redeem(fresh, "run_mine"); !ok {
		t.Error("a refused lookup consumed an unrelated live ticket")
	}
}

// TestExpiredTicketIsRefused pins the thirty second lifetime. The ticket only has to survive the
// moment between asking for it and the browser opening the stream, so one captured from an access
// log is almost always already dead, and that is the whole reason it may travel in a URL.
func TestExpiredTicketIsRefused(t *testing.T) {
	t.Parallel()
	tickets := newStreamTickets()
	now := time.Now()
	tickets.now = func() time.Time { return now }
	inside, err := tickets.mint(Actor{UserID: "user_1"}, "run_mine")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A moment before the lifetime lapses the ticket still works.
	now = now.Add(streamTicketTTL - time.Millisecond)
	if _, ok := tickets.redeem(inside, "run_mine"); !ok {
		t.Fatal("a ticket inside its lifetime was refused")
	}
	// One minted at the same moment is dead once the clock passes the lifetime.
	lapsed, err := tickets.mint(Actor{UserID: "user_1"}, "run_mine")
	if err != nil {
		t.Fatalf("mint again: %v", err)
	}
	now = now.Add(streamTicketTTL + time.Millisecond)
	if _, ok := tickets.redeem(lapsed, "run_mine"); ok {
		t.Error("a ticket past its lifetime still opened the stream")
	}
}

// TestStreamTicketHandlerPanicsWithoutAStore pins the constructor's refusal to build a handler with
// nothing to mint into. A nil ticket store here is a wiring mistake, and failing at startup is much
// better than a nil dereference on the first person who opens a run page.
func TestStreamTicketHandlerPanicsWithoutAStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("streamTicketHandler accepted a nil ticket store")
		}
	}()
	streamTicketHandler(nil, zap.NewNop())
}

// TestAuditRegisterRefusesABadPeriod pins the parameter validation on the change register. The
// period is caller controlled and drives a store query, so an unparsable or inverted range has to be
// refused rather than silently becoming a default window that the rendered document then claims was
// the period reviewed.
func TestAuditRegisterRefusesABadPeriod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Query      string
		NoAudits   bool
		WantStatus int
	}{{ // Test 0: The audit trail is off, so there is no register to draw.
		Name: "audit off", NoAudits: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: A to that is neither a date nor an RFC 3339 time is refused.
		Name: "bad to", Query: "?to=last-tuesday", WantStatus: http.StatusBadRequest,
	}, { // Test 2: A from that is neither is refused too.
		Name: "bad from", Query: "?from=whenever", WantStatus: http.StatusBadRequest,
	}, { // Test 3: An inverted period is refused rather than rendered empty.
		Name: "from after to", Query: "?from=2026-06-01&to=2026-01-01",
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: An empty period, where from equals to, is refused because from must precede to.
		Name: "from equals to", Query: "?from=2026-01-01&to=2026-01-01",
		WantStatus: http.StatusBadRequest,
	}, { // Test 5: A plain date period is accepted.
		Name: "date period", Query: "?from=2026-01-01&to=2026-06-01", WantStatus: http.StatusOK,
	}, { // Test 6: An RFC 3339 period is accepted.
		Name: "rfc3339 period", Query: "?from=2026-01-01T00:00:00Z&to=2026-06-01T00:00:00Z",
		WantStatus: http.StatusOK,
	}, { // Test 7: No period at all defaults to the last ninety days.
		Name: "default period", Query: "", WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var audits audit.Store
			if !test.NoAudits {
				audits = audit.NewMemStore()
			}
			rec := httptest.NewRecorder()
			auditRegisterHandler(run.NewMemStore(), audits, "install_1", zap.NewNop()).
				ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
					"/v1/audit/register"+test.Query, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (%q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestAuditRegisterHandlerPanicsWithoutAStore pins that the register handler refuses to be built
// without a run store, which is a wiring mistake rather than a runtime condition.
func TestAuditRegisterHandlerPanicsWithoutAStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("auditRegisterHandler accepted a nil run store")
		}
	}()
	auditRegisterHandler(nil, audit.NewMemStore(), "install_1", zap.NewNop())
}

// TestParseRegisterTime pins both accepted spellings of a period bound and the refusal of anything
// else. A bound the parser guesses at would silently move the window a compliance review believes
// it sampled.
func TestParseRegisterTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      string
		WantErr bool
	}{{ // Test 0: A plain date.
		Name: "date", In: "2026-01-31",
	}, { // Test 1: An RFC 3339 time with a zone.
		Name: "rfc3339 utc", In: "2026-01-31T12:00:00Z",
	}, { // Test 2: An RFC 3339 time with an offset.
		Name: "rfc3339 offset", In: "2026-01-31T12:00:00-06:00",
	}, { // Test 3: An empty string is not a time.
		Name: "empty", In: "", WantErr: true,
	}, { // Test 4: A prose date is refused rather than guessed at.
		Name: "prose", In: "31 January 2026", WantErr: true,
	}, { // Test 5: A date with no zone and a time is not either accepted format.
		Name: "naive datetime", In: "2026-01-31 12:00:00", WantErr: true,
	}, { // Test 6: A month that does not exist is refused.
		Name: "impossible month", In: "2026-13-01", WantErr: true,
	}, { // Test 7: A unix timestamp is not one of the two spellings.
		Name: "timestamp", In: "1767225600", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseRegisterTime(test.In)
			if test.WantErr {
				if err == nil {
					t.Errorf("%s: parsed %q as %v, want a refusal", test.Name, test.In, got)
				}
				return
			}
			if err != nil {
				t.Errorf("%s: parseRegisterTime(%q) error = %v", test.Name, test.In, err)
			}
			if got.IsZero() {
				t.Errorf("%s: parsed to the zero time", test.Name)
			}
		})
	}
}

// unreadableAudits is an audit store whose chain cannot be walked, standing in for a database that
// is unreachable rather than a chain that is broken.
type unreadableAudits struct {
	// err is what ChainScan reports.
	err error
}

// Append accepts nothing, since this store exists only to fail a read.
func (u *unreadableAudits) Append(context.Context, *audit.Entry) error { return u.err }

// AppendSpanBeat fails the same way.
func (u *unreadableAudits) AppendSpanBeat(context.Context, time.Time, int) (*audit.Entry, error) {
	return nil, u.err
}

// SpanBeats returns nothing.
func (u *unreadableAudits) SpanBeats(context.Context, int) ([]*audit.Entry, error) {
	return nil, u.err
}

// List returns nothing.
func (u *unreadableAudits) List(context.Context, int) ([]*audit.Entry, error) { return nil, u.err }

// Chain returns nothing.
func (u *unreadableAudits) Chain(context.Context) ([]*audit.Entry, error) { return nil, u.err }

// ChainScan reports the read failure, which is what marks the health view stale.
func (u *unreadableAudits) ChainScan(context.Context, int64, func(*audit.Entry) error) error {
	return u.err
}

// TestChainHealthReportsStaleRatherThanBroken pins the distinction an alarm depends on. A chain that
// cannot be read is not a chain that has been tampered with, and reporting one as the other pages
// the wrong person for the wrong reason. A relying alarm reads verified together with stale:
// verified false with stale false is a confirmed break, stale true is "could not check".
func TestChainHealthReportsStaleRatherThanBroken(t *testing.T) {
	t.Parallel()
	health := newChainHealth(&unreadableAudits{err: errStore}, "install_1")
	got := health.snapshot(context.Background())
	if !got.Stale {
		t.Error("an unreadable chain was not reported stale")
	}
	if got.Verified {
		t.Error("an unreadable chain was reported verified, which is the one direction this must " +
			"never move")
	}
	if got.Entries != 0 || got.BrokeAt != 0 {
		t.Errorf("an unreadable chain reported %d entries broken at %d, want the unverified "+
			"defaults", got.Entries, got.BrokeAt)
	}
}

// TestChainHealthNeverReportsAnUnwalkedChainSound pins the starting state. verified begins false, so
// a chain that has never been verified is never reported sound, which is what keeps a scrape taken
// before the first walk from reading as an all-clear.
func TestChainHealthNeverReportsAnUnwalkedChainSound(t *testing.T) {
	t.Parallel()
	health := newChainHealth(audit.NewMemStore(), "install_1")
	if health.verified {
		t.Error("a chain that has never been walked starts out reported verified")
	}
	got := health.snapshot(context.Background())
	if got.Stale {
		t.Error("an empty but readable chain was reported stale")
	}
}

// TestChainHealthServesTheCacheInsideItsWindow pins the brake on a scrape storm. Each refresh
// re-verifies the whole chain, which is the only way to catch an in-place rewrite, so repeated
// scrapes inside a short window must serve the last verdict rather than re-walking and turning the
// metrics endpoint into a busy loop.
func TestChainHealthServesTheCacheInsideItsWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	counting := &countingAudits{Store: audit.NewMemStore()}
	health := newChainHealth(counting, "install_1")
	now := time.Now()
	health.clock = func() time.Time { return now }

	health.snapshot(ctx)
	health.snapshot(ctx)
	health.snapshot(ctx)
	if counting.scans != 1 {
		t.Errorf("chain walks = %d, want 1 inside the window", counting.scans)
	}

	now = now.Add(chainHealthInterval + time.Second)
	health.snapshot(ctx)
	if counting.scans != 2 {
		t.Errorf("chain walks = %d, want a second walk once the window passed", counting.scans)
	}
}

// countingAudits wraps an audit store and counts the chain walks made through it.
type countingAudits struct {
	// Store is the store underneath.
	audit.Store
	// scans is how many times the chain has been walked.
	scans int
}

// ChainScan counts the walk and passes it through.
func (c *countingAudits) ChainScan(ctx context.Context, after int64,
	fn func(*audit.Entry) error) error {
	c.scans++
	return c.Store.ChainScan(ctx, after, fn)
}

// TestNewChainHealthPanicsOnANilStore pins that a missing audit store is caught as the wiring
// mistake it is. The caller decides whether the audit trail is configured, so a nil store reaching
// here is a programming error rather than a condition to report.
func TestNewChainHealthPanicsOnANilStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("newChainHealth accepted a nil audit store")
		}
	}()
	newChainHealth(nil, "install_1")
}
