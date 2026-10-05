package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// callbackInventory is the host list every callback test matches against. Two hosts share one
// address on purpose, so an ambiguous match can be asked for.
const callbackInventory = "[web]\nweb01 ansible_host=10.0.0.11\nweb02 ansible_host=10.0.0.12\n" +
	"db01.example.com\n[pair]\ndup-a ansible_host=10.0.0.50\ndup-b ansible_host=10.0.0.50\n"

// fakeResolver answers lookups from fixed tables, so host matching is tested without a network.
type fakeResolver struct {
	// names maps an address to the names it reverse resolves to.
	names map[string][]string
	// addrs maps a name to the addresses it resolves to.
	addrs map[string][]string
}

// LookupAddr returns the names addr reverse resolves to, or an error when the table has none.
func (f fakeResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if n, ok := f.names[addr]; ok {
		return n, nil
	}
	return nil, errors.New("no reverse entry")
}

// LookupHost returns the addresses host resolves to, or an error when the table has none.
func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if a, ok := f.addrs[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

// callbackFixture is one server wired for provisioning callbacks.
type callbackFixture struct {
	// handler is the server's whole handler, gate included.
	handler http.Handler
	// templates holds the callback template.
	templates template.Store
	// store is the run store the server and its pending check read.
	store run.Store
	// audits records what the callback writes to the chain.
	audits *recordingAudits
}

// newCallbackFixture builds a server with the callback inventory, one Ansible template accepting
// callbacks, an enabled sealer, and the given submitter, resolver, and extra options.
func newCallbackFixture(t *testing.T, sub Submitter, res callbackResolver, tpl *template.Template,
	opts ...Option) *callbackFixture {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	templates := template.NewMemStore()
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: callbackInventory, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if tpl == nil {
		tpl = &template.Template{ID: "tpl_cb", Name: "boot", Playbook: "boot.yml",
			InventoryID: "inv_1", Limit: "web", AllowCallbacks: true, CreatedAt: time.Now()}
	}
	if err := templates.Save(ctx, tpl); err != nil {
		t.Fatalf("save template: %v", err)
	}
	audits := &recordingAudits{}
	all := append([]Option{
		WithTemplates(templates), WithInventories(inventories), WithAudit(audits),
		WithCredentials(credential.NewMemStore(), credential.NewSealer("pass", "salt")),
		func(s *Server) { s.callbackResolver = res },
		WithCallbackLimitMatcher(webLimit()),
	}, opts...)
	return &callbackFixture{handler: New(store, sub, zap.NewNop(), all...).Handler(),
		templates: templates, store: store, audits: audits}
}

// mintKey mints a callback key through the API and returns its plaintext.
func (f *callbackFixture) mintKey(t *testing.T, templateID string, header http.Header) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/templates/"+templateID+"/callback-key", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint key status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp callbackKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.HostConfigKey == "" {
		t.Fatalf("mint key response %s: %v", rec.Body.String(), err)
	}
	return resp.HostConfigKey
}

// callBack posts a callback from addr with body and content type and returns the recorder.
func (f *callbackFixture) callBack(templateID, addr, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/templates/"+templateID+"/callback",
		strings.NewReader(body))
	req.RemoteAddr = addr + ":40000"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// code posts a JSON callback carrying key from addr and returns the status.
func (f *callbackFixture) code(addr, key string) int {
	return f.callBack("tpl_cb", addr, "application/json", keyBody(key)).Code
}

// keyBody is the JSON body AWX's documented curl sends.
func keyBody(key string) string { return `{"host_config_key": "` + key + `"}` }

// TestCallbackMatchesTheCallingHost covers AWX's host matching rules and the refusals around them:
// the caller is matched by ansible_host, by reverse lookup of its address, or by forward lookup of
// the inventory's names, and launched limited to that one host. An unknown host, an ambiguous one,
// a wrong key, and a template that does not exist are refused without a launch.
func TestCallbackMatchesTheCallingHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantHost    string
		WantBody    string
		Addr        string
		ContentType string
		Body        func(key string) string
		Template    string
		Resolver    fakeResolver
		WantStatus  int
	}{{ // Test 0: The caller's address is a host's ansible_host.
		Addr: "10.0.0.11", Body: keyBody, WantStatus: http.StatusCreated, WantHost: "web01",
	}, { // Test 1: The caller's address reverse resolves to a host's name.
		Addr: "10.9.9.9", Body: keyBody, WantStatus: http.StatusCreated, WantHost: "db01.example.com",
		Resolver: fakeResolver{names: map[string][]string{"10.9.9.9": {"DB01.example.com."}}},
	}, { // Test 2: A host's name resolves forward to the caller's address.
		Addr: "10.7.7.7", Body: keyBody, WantStatus: http.StatusCreated, WantHost: "db01.example.com",
		Resolver: fakeResolver{addrs: map[string][]string{"db01.example.com": {"10.7.7.7"}}},
	}, { // Test 3: AWX's form body is accepted as well as JSON.
		Addr: "10.0.0.12", ContentType: "application/x-www-form-urlencoded",
		Body:       func(key string) string { return "host_config_key=" + key },
		WantStatus: http.StatusCreated, WantHost: "web02",
	}, { // Test 4: An address no host matches is refused.
		Addr: "203.0.113.9", Body: keyBody, WantStatus: http.StatusBadRequest,
		WantBody: "no host in this template's inventory matches",
	}, { // Test 5: An address two hosts share is refused rather than guessed.
		Addr: "10.0.0.50", Body: keyBody, WantStatus: http.StatusBadRequest,
		WantBody: "dup-a, dup-b",
	}, { // Test 6: A wrong key is refused.
		Addr: "10.0.0.11", Body: func(string) string { return keyBody("hck_wrong") },
		WantStatus: http.StatusForbidden, WantBody: "invalid host config key",
	}, { // Test 7: A template that does not exist answers exactly as a wrong key does.
		Addr: "10.0.0.11", Template: "tpl_missing", Body: keyBody,
		WantStatus: http.StatusForbidden, WantBody: "invalid host config key",
	}, { // Test 8: A callback cannot change what the template runs.
		Addr: "10.0.0.11", WantStatus: http.StatusBadRequest, WantBody: "extra_vars is refused",
		Body: func(key string) string {
			return `{"host_config_key": "` + key + `", "extra_vars": {"x": 1}}`
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
			f := newCallbackFixture(t, sub, test.Resolver, nil)
			key := f.mintKey(t, "tpl_cb", nil)
			id := "tpl_cb"
			if test.Template != "" {
				id = test.Template
			}
			contentType := test.ContentType
			if contentType == "" {
				contentType = "application/json"
			}
			rec := f.callBack(id, test.Addr, contentType, test.Body(key))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantBody) {
				t.Errorf("body %s does not say %q", rec.Body.String(), test.WantBody)
			}
			if test.WantHost == "" {
				if sub.gotRun != nil {
					t.Errorf("a refused callback launched a run: %+v", sub.gotRun)
				}
				if len(f.audits.entries) != 0 {
					t.Errorf("a refused callback wrote %d chain entries, want none", len(f.audits.entries))
				}
				return
			}
			if sub.gotRun == nil {
				t.Fatal("the callback launched nothing")
			}
			got := sub.gotRun
			want := callbackShape{Limit: test.WantHost, Source: "callback", SourceID: "tpl_cb",
				ActorType: "host", Actor: "host " + test.WantHost + " from " + test.Addr,
				InventoryID: "inv_1"}
			if diff := cmp.Diff(want, shapeOf(got)); diff != "" {
				t.Errorf("launched run mismatch (-want +got):\n%s", diff)
			}
			if rec.Header().Get("Location") != "/v1/runs/run_cb" {
				t.Errorf("Location = %q, want the created run", rec.Header().Get("Location"))
			}
		})
	}
}

// callbackShape is the part of a launched run a callback decides.
type callbackShape struct {
	// Limit is the host the run is confined to.
	Limit string
	// Source is what fired the run.
	Source string
	// SourceID is the template behind it.
	SourceID string
	// ActorType is how the requester is classified.
	ActorType string
	// Actor names the requester.
	Actor string
	// InventoryID is the inventory the run targets.
	InventoryID string
}

// shapeOf reduces a run to its callback shape.
func shapeOf(r *run.Run) callbackShape {
	return callbackShape{Limit: r.Limit, Source: r.Source, SourceID: r.SourceID,
		ActorType: r.ActorType, Actor: r.Actor, InventoryID: r.InventoryID}
}

// TestCallbackTrustsForwardedAddressOnlyFromATrustedProxy pins that the caller's address follows
// the server's existing proxy rule. Behind a trusted proxy the forwarded address is the caller, so
// the host behind the proxy is matched. From anybody else the header is a value the caller chose,
// so it is ignored, and a stranger cannot claim another host's address to launch its job.
func TestCallbackTrustsForwardedAddressOnlyFromATrustedProxy(t *testing.T) {
	// Not parallel: the trusted-proxy configuration is a package global.
	withTrustedProxy(t, "192.0.2.0/24")
	tests := []struct {
		Peer       string
		WantStatus int
	}{
		{Peer: "192.0.2.10", WantStatus: http.StatusCreated},      // Test 0: A trusted proxy forwards.
		{Peer: "198.51.100.7", WantStatus: http.StatusBadRequest}, // Test 1: A stranger forges.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
			f := newCallbackFixture(t, sub, fakeResolver{}, nil)
			key := f.mintKey(t, "tpl_cb", nil)
			req := httptest.NewRequest(http.MethodPost, "/v1/templates/tpl_cb/callback",
				strings.NewReader(keyBody(key)))
			req.RemoteAddr = test.Peer + ":40000"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", "10.0.0.11")
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestCallbackRefusesWhileARunIsPending pins AWX's replay protection: while a callback run for the
// same template and host has not finished, another callback is refused rather than stacked. A run
// for a different host, or one that has finished, does not block.
func TestCallbackRefusesWhileARunIsPending(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Seeded     *run.Run
		WantStatus int
	}{{ // Test 0: A pending callback run for the same host blocks.
		Seeded: &run.Run{ID: "run_prev", Status: run.StatusPending, Source: "callback",
			SourceID: "tpl_cb", Limit: "web01"},
		WantStatus: http.StatusConflict,
	}, { // Test 1: One held for approval blocks too, since it has not run yet.
		Seeded: &run.Run{ID: "run_prev", Status: run.StatusPendingApproval, Source: "callback",
			SourceID: "tpl_cb", Limit: "web01"},
		WantStatus: http.StatusConflict,
	}, { // Test 2: A running one blocks.
		Seeded: &run.Run{ID: "run_prev", Status: run.StatusRunning, Source: "callback",
			SourceID: "tpl_cb", Limit: "web01"},
		WantStatus: http.StatusConflict,
	}, { // Test 3: A finished one does not.
		Seeded: &run.Run{ID: "run_prev", Status: run.StatusSucceeded, Source: "callback",
			SourceID: "tpl_cb", Limit: "web01"},
		WantStatus: http.StatusCreated,
	}, { // Test 4: Another host's pending callback does not.
		Seeded: &run.Run{ID: "run_prev", Status: run.StatusPending, Source: "callback",
			SourceID: "tpl_cb", Limit: "web02"},
		WantStatus: http.StatusCreated,
	}, { // Test 5: A pending run the template launched another way does not.
		Seeded: &run.Run{ID: "run_prev", Status: run.StatusPending, Source: "template",
			SourceID: "tpl_cb", Limit: "web01"},
		WantStatus: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
			f := newCallbackFixture(t, sub, fakeResolver{}, nil)
			test.Seeded.CreatedAt = time.Now().Add(-time.Hour)
			if err := f.store.Save(context.Background(), test.Seeded); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			key := f.mintKey(t, "tpl_cb", nil)
			rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			if launched := sub.gotRun != nil; launched != (test.WantStatus == http.StatusCreated) {
				t.Errorf("launched = %v for status %d", launched, test.WantStatus)
			}
		})
	}
}

// TestCallbackKeyRotationAndRevocation pins that a minted key works, that rotating it retires the
// old one at once, that revoking it refuses everybody, and that turning callbacks off on the
// template revokes the key so turning them back on does not bring an old key back to life.
func TestCallbackKeyRotationAndRevocation(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newCallbackFixture(t, sub, fakeResolver{}, nil)
	call := func(key string) int {
		t.Helper()
		sub.gotRun = nil
		return f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key)).Code
	}
	first := f.mintKey(t, "tpl_cb", nil)
	if code := call(first); code != http.StatusCreated {
		t.Fatalf("first key status = %d, want 201", code)
	}
	second := f.mintKey(t, "tpl_cb", nil)
	if first == second {
		t.Fatal("rotation returned the same key")
	}
	if code := call(first); code != http.StatusForbidden {
		t.Errorf("rotated-out key status = %d, want 403", code)
	}
	if code := call(second); code != http.StatusCreated {
		t.Errorf("new key status = %d, want 201", code)
	}
	stored, err := f.templates.Get(context.Background(), "tpl_cb")
	if err != nil {
		t.Fatal(err)
	}
	if stored.HostConfigKey == "" || strings.Contains(stored.HostConfigKey, second) {
		t.Errorf("stored key = %q, want the key sealed rather than in the clear", stored.HostConfigKey)
	}

	rec := httptest.NewRecorder()
	revoke := httptest.NewRequest(http.MethodDelete, "/v1/templates/tpl_cb/callback-key", nil)
	f.handler.ServeHTTP(rec, revoke)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body %s", rec.Code, rec.Body.String())
	}
	if code := call(second); code != http.StatusForbidden {
		t.Errorf("revoked key status = %d, want 403", code)
	}

	third := f.mintKey(t, "tpl_cb", nil)
	put := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/v1/templates/tpl_cb", strings.NewReader(body))
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("update status = %d, body %s", rec.Code, rec.Body.String())
		}
	}
	// An edit that does not mention callbacks leaves them, and the key, alone.
	put(`{"name":"boot","playbook":"boot.yml","inventory_id":"inv_1"}`)
	if code := call(third); code != http.StatusCreated {
		t.Errorf("after an edit that left callbacks alone, status = %d, want 201", code)
	}
	put(`{"name":"boot","playbook":"boot.yml","inventory_id":"inv_1","allow_callbacks":false}`)
	put(`{"name":"boot","playbook":"boot.yml","inventory_id":"inv_1","allow_callbacks":true}`)
	if code := call(third); code != http.StatusForbidden {
		t.Errorf("after callbacks were turned off and on, the old key status = %d, want 403", code)
	}
}

// TestCallbackKeyIsNeverServed pins that a template read says a key exists and never carries it,
// sealed or plain.
func TestCallbackKeyIsNeverServed(t *testing.T) {
	t.Parallel()
	f := newCallbackFixture(t, &fakeSubmitter{}, fakeResolver{}, nil)
	key := f.mintKey(t, "tpl_cb", nil)
	stored, err := f.templates.Get(context.Background(), "tpl_cb")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/templates", nil))
	body := rec.Body.String()
	if strings.Contains(body, key) || strings.Contains(body, stored.HostConfigKey) {
		t.Errorf("the template list carries the callback key: %s", body)
	}
	if !strings.Contains(body, `"host_config_key_set":true`) ||
		!strings.Contains(body, `"allow_callbacks":true`) {
		t.Errorf("the template list does not say a key is set: %s", body)
	}
}

// TestCallbackIsRecordedBeforeItLaunches pins the evidence: the chain entry is written before the
// run exists, names the host and the address it called from, and is classified as a host. An
// unhealthy chain refuses the callback, so nothing launches unrecorded.
func TestCallbackIsRecordedBeforeItLaunches(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newCallbackFixture(t, sub, fakeResolver{}, nil)
	f.audits.sub = sub
	key := f.mintKey(t, "tpl_cb", nil)
	minted := len(f.audits.entries)
	rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	entries := f.audits.entries[minted:]
	if len(entries) != 1 {
		t.Fatalf("callback wrote %d chain entries, want 1", len(entries))
	}
	e := entries[0]
	want := audit.Entry{Actor: "host web01 from 10.0.0.11", ActorType: "host",
		Method: http.MethodPost, Path: "/v1/templates/tpl_cb/callback/fired"}
	got := audit.Entry{Actor: e.Actor, ActorType: e.ActorType, Method: e.Method, Path: e.Path}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("chain entry mismatch (-want +got):\n%s", diff)
	}
	if f.audits.runAtAppend != nil {
		t.Errorf("the run existed before its chain entry: %+v", f.audits.runAtAppend)
	}
	if rec.Header().Get(AuditReceiptHeader) == "" {
		t.Error("the callback answered with no audit receipt")
	}

	failing := newCallbackFixture(t, sub, fakeResolver{}, nil)
	failKey := failing.mintKey(t, "tpl_cb", nil)
	failing.audits.err = errors.New("disk full")
	sub.gotRun = nil
	if code := failing.code("10.0.0.11", failKey); code < 500 {
		t.Errorf("status = %d with an unhealthy chain, want a 5xx refusal", code)
	}
	if sub.gotRun != nil {
		t.Error("a callback launched although it could not be recorded")
	}
}

// TestCallbackGoesThroughTheGate drives a callback into a real dispatcher with an approval policy.
// The run is held for approval exactly as a person's launch would be, carries the host and the
// callback provenance, and a second callback for the host is refused while it waits. A deny rule
// refuses the callback outright.
func TestCallbackGoesThroughTheGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{ID: "pol_1", Name: "hold boots", InventoryID: "inv_1",
		MaxDestroy: -1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: callbackInventory, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := dispatch.New(store, runner, zap.NewNop(), dispatch.WithPolicies(policies),
		dispatch.WithInventories(inventories), dispatch.WithNoJanitor())
	t.Cleanup(d.Close)
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{ID: "tpl_cb", Name: "boot", Playbook: "boot.yml",
		InventoryID: "inv_1", AllowCallbacks: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	handler := New(store, d, zap.NewNop(), WithTemplates(templates), WithInventories(inventories),
		WithAudit(&recordingAudits{}),
		WithCredentials(credential.NewMemStore(), credential.NewSealer("pass", "salt")),
		func(s *Server) { s.callbackResolver = fakeResolver{} }).Handler()
	f := &callbackFixture{handler: handler, templates: templates, store: store}
	key := f.mintKey(t, "tpl_cb", nil)

	rec := f.callBack("tpl_cb", "10.0.0.12", "application/json", keyBody(key))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp callbackResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != run.StatusPendingApproval || resp.Host != "web02" {
		t.Fatalf("response = %+v, want web02 held for approval", resp)
	}
	held, err := store.Get(ctx, resp.Run)
	if err != nil {
		t.Fatal(err)
	}
	if held.HeldByPolicy != "hold boots" || held.Limit != "web02" || held.Source != "callback" ||
		held.ActorType != "host" || held.AuditReceipt == "" {
		t.Errorf("held run = policy %q limit %q source %q actor type %q receipt %q, want the "+
			"callback held by the policy and tied to its chain entry", held.HeldByPolicy, held.Limit,
			held.Source, held.ActorType, held.AuditReceipt)
	}
	if again := f.code("10.0.0.12", key); again != http.StatusConflict {
		t.Errorf("second callback while held: status = %d, want 409", again)
	}

	denied := newCallbackFixture(t, &fakeSubmitter{err: dispatch.ErrPolicyDenied}, fakeResolver{}, nil)
	deniedKey := denied.mintKey(t, "tpl_cb", nil)
	if code := denied.code("10.0.0.11", deniedKey); code != http.StatusForbidden {
		t.Errorf("denied callback status = %d, want 403", code)
	}
}

// TestCallbackIsPublicButKeyManagementIsNot runs on an install that enforces tokens. A booting host
// has no account, so its callback needs none and leaves no gate entry, while minting a key still
// needs a token like any other change to a template.
func TestCallbackIsPublicButKeyManagementIsNot(t *testing.T) {
	t.Parallel()
	tokens := auth.NewMemStore()
	plain, tok, err := auth.New("admin-cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := tokens.Save(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newCallbackFixture(t, sub, fakeResolver{}, nil, WithTokens(tokens))

	rec := httptest.NewRecorder()
	mint := httptest.NewRequest(http.MethodPost, "/v1/templates/tpl_cb/callback-key", nil)
	f.handler.ServeHTTP(rec, mint)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("minting a key without a token: status = %d, want 401", rec.Code)
	}
	key := f.mintKey(t, "tpl_cb", http.Header{"Authorization": {"Bearer " + plain}})
	before := len(f.audits.entries)
	if code := f.code("10.0.0.11", "hck_guess"); code != http.StatusForbidden {
		t.Errorf("a guessed key: status = %d, want 403", code)
	}
	if len(f.audits.entries) != before {
		t.Errorf("a refused guess wrote %d chain entries, want none", len(f.audits.entries)-before)
	}
	if code := f.code("10.0.0.11", key); code != http.StatusCreated {
		t.Errorf("a callback with no token: status = %d, want 201", code)
	}
	for _, p := range []string{"/v1/templates/tpl_cb/callback/../launch", "/v1/templates/a/b/callback",
		"/templates/tpl_cb/callback"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/templates/x", nil)
		req.URL.Path = p
		if isCallback(req) {
			t.Errorf("isCallback(%q) = true, want only the exact clean callback path public", p)
		}
	}
}

// TestCallbackRefusals covers what stops a callback before any host is matched: guessing keys past
// the per-address budget, a survey that needs a person, and a template that cannot launch from a
// callback.
func TestCallbackRefusals(t *testing.T) {
	t.Parallel()
	t.Run("key guessing is rate limited", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
		f := newCallbackFixture(t, sub, fakeResolver{}, nil)
		key := f.mintKey(t, "tpl_cb", nil)
		for range DefaultCallbackKeyFailureLimit {
			f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody("hck_guess"))
		}
		if code := f.code("10.0.0.11", key); code != http.StatusTooManyRequests {
			t.Errorf("after %d wrong keys the right one: status = %d, want 429",
				DefaultCallbackKeyFailureLimit, code)
		}
		if code := f.code("10.0.0.12", key); code != http.StatusCreated {
			t.Errorf("another address: status = %d, want 201", code)
		}
	})
	t.Run("a required survey answer needs a person", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
		f := newCallbackFixture(t, sub, fakeResolver{}, &template.Template{ID: "tpl_cb", Name: "boot",
			Playbook: "boot.yml", InventoryID: "inv_1", AllowCallbacks: true,
			Survey: []template.SurveyField{{Var: "role", Label: "Role", Type: template.FieldText,
				Required: true}}})
		key := f.mintKey(t, "tpl_cb", nil)
		rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key))
		if rec.Code != http.StatusBadRequest ||
			!strings.Contains(rec.Body.String(), "user input required") {
			t.Errorf("status = %d body %s, want 400 asking for user input", rec.Code, rec.Body.String())
		}
	})
	t.Run("callbacks off refuses a valid key", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
		f := newCallbackFixture(t, sub, fakeResolver{}, &template.Template{ID: "tpl_cb", Name: "boot",
			Playbook: "boot.yml", InventoryID: "inv_1"})
		key := f.mintKey(t, "tpl_cb", nil)
		if code := f.code("10.0.0.11", key); code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", code)
		}
	})
	t.Run("an inventory resolved at launch has no host list", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		inventories := inventory.NewMemStore()
		if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "vaulted",
			ContentSource: "command", ContentConfig: "sealed", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
		f := newCallbackFixture(t, sub, fakeResolver{}, nil, WithInventories(inventories))
		key := f.mintKey(t, "tpl_cb", nil)
		if code := f.code("10.0.0.11", key); code != http.StatusConflict {
			t.Errorf("status = %d, want 409", code)
		}
	})
	t.Run("a composed inventory has no host list of its own", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		inventories := inventory.NewMemStore()
		// The content would match the caller if it were read as a host list, so only the refusal
		// keeps the callback from launching.
		if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "composed",
			Kind: inventory.KindSmart, Content: callbackInventory, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
		f := newCallbackFixture(t, sub, fakeResolver{}, nil, WithInventories(inventories))
		key := f.mintKey(t, "tpl_cb", nil)
		rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "smart inventory") {
			t.Errorf("status = %d body %s, want 409 naming the smart inventory", rec.Code,
				rec.Body.String())
		}
		if sub.gotRun != nil {
			t.Error("a callback against a composed inventory launched a run")
		}
	})
}

// TestTemplateCacheAndCallbackSettings pins the template API's validation of the two settings and
// that an omitted setting is left as it was on an update.
func TestTemplateCacheAndCallbackSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantBody   string
		Body       string
		WantStatus int
	}{{ // Test 0: Both on an Ansible template with a stored inventory.
		Body: `{"name":"a","playbook":"p.yml","inventory_id":"inv_1","use_fact_cache":true,` +
			`"fact_cache_timeout":60,"allow_callbacks":true}`,
		WantStatus: http.StatusCreated, WantBody: `"fact_cache_timeout":60`,
	}, { // Test 1: The fact cache needs a stored inventory.
		Body:       `{"name":"a","playbook":"p.yml","inventory":"hosts.ini","use_fact_cache":true}`,
		WantStatus: http.StatusBadRequest, WantBody: "use_fact_cache needs inventory_id",
	}, { // Test 2: Callbacks are an Ansible feature.
		Body: `{"name":"a","tool":"bash","command":"true","inventory_id":"inv_1",` +
			`"allow_callbacks":true}`,
		WantStatus: http.StatusBadRequest, WantBody: "allow_callbacks is an Ansible feature",
	}, { // Test 3: A negative timeout.
		Body: `{"name":"a","playbook":"p.yml","inventory_id":"inv_1","use_fact_cache":true,` +
			`"fact_cache_timeout":-1}`,
		WantStatus: http.StatusBadRequest, WantBody: "cannot be negative",
	}, { // Test 4: A workflow cannot carry either.
		Body: `{"name":"a","inventory_id":"inv_1","use_fact_cache":true,` +
			`"steps":[{"name":"s","playbook":"p.yml"}]}`,
		WantStatus: http.StatusBadRequest, WantBody: "this template is a workflow",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := template.NewMemStore()
			h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTemplates(store)).Handler()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/templates", strings.NewReader(test.Body))
			h.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantBody) {
				t.Errorf("status = %d body %s, want %d containing %q", rec.Code, rec.Body.String(),
					test.WantStatus, test.WantBody)
			}
		})
	}

	t.Run("an omitted setting is unchanged", func(t *testing.T) {
		t.Parallel()
		store := template.NewMemStore()
		if err := store.Save(context.Background(), &template.Template{ID: "tpl_1", Name: "a",
			Playbook: "p.yml", InventoryID: "inv_1", UseFactCache: true, FactCacheTimeout: 30,
			AllowCallbacks: true}); err != nil {
			t.Fatal(err)
		}
		h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTemplates(store)).Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/tpl_1",
			strings.NewReader(`{"name":"b","playbook":"p.yml","inventory_id":"inv_1"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		got, err := store.Get(context.Background(), "tpl_1")
		if err != nil {
			t.Fatal(err)
		}
		if !got.UseFactCache || got.FactCacheTimeout != 30 || !got.AllowCallbacks || got.Name != "b" {
			t.Errorf("after an edit that omitted them: %+v, want the settings kept", got)
		}
	})
}

// TestCallbackCarriesSealedSurveyDefaults pins that a callback launch of a template whose survey
// has a secret field with a default carries that default onto the run still sealed, the way a
// launch that leaves the field blank does, and never as a plain extra var, even when the template
// also sets an extra var of the same name.
func TestCallbackCarriesSealedSurveyDefaults(t *testing.T) {
	t.Parallel()
	tpl := &template.Template{ID: "tpl_cb", Name: "boot", Playbook: "boot.yml",
		InventoryID: "inv_1", AllowCallbacks: true, CreatedAt: time.Now(),
		ExtraVars: map[string]any{"db_password": "plain-template-value", "region": "us"},
		Survey: []template.SurveyField{
			{Var: "db_password", Type: template.FieldSecret, SealedDefault: "sealed-default"},
			{Var: "size", Type: template.FieldText, Default: "small"},
		}}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newCallbackFixture(t, sub, fakeResolver{}, tpl)
	key := f.mintKey(t, "tpl_cb", nil)
	if rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key)); rec.Code !=
		http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if sub.gotRun == nil {
		t.Fatal("the callback launched nothing")
	}
	if diff := cmp.Diff(map[string]string{"db_password": "sealed-default"},
		sub.gotRun.SealedVars); diff != "" {
		t.Errorf("sealed vars mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]any{"region": "us", "size": "small"},
		sub.gotRun.ExtraVars); diff != "" {
		t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
	}
}
