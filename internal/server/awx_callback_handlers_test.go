package server

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// awxTemplateID is the AWX job template id the AWX callback tests' template was imported from.
const awxTemplateID = 42

// newAWXFixture builds a callback fixture whose template an import bound to AWX job template 42,
// with the AWX-compatible address on. mutate, when set, changes the template before it is saved.
func newAWXFixture(t *testing.T, sub Submitter, mutate func(*template.Template),
	opts ...Option) *callbackFixture {
	t.Helper()
	tpl := &template.Template{ID: "tpl_cb", Name: "boot", Playbook: "boot.yml", InventoryID: "inv_1",
		Limit: "web", AllowCallbacks: true, AWXCallback: true, CreatedAt: time.Now()}
	if mutate != nil {
		mutate(tpl)
	}
	f := newCallbackFixture(t, sub, fakeResolver{}, tpl, opts...)
	now := time.Now()
	if err := f.templates.BindAWX(context.Background(), template.AWXBinding{AWXID: awxTemplateID,
		TemplateID: "tpl_cb", Organization: "Platform", Name: "boot", CreatedAt: now,
		UpdatedAt: now}); err != nil {
		t.Fatalf("BindAWX() error = %v", err)
	}
	return f
}

// awxPath is the AWX-compatible callback address of id, as AWX writes it.
func awxPath(id int64) string {
	return "/api/v2/job_templates/" + strconv.FormatInt(id, 10) + "/callback/"
}

// post sends method to path from addr with body and content type and returns the recorder.
func (f *callbackFixture) post(method, path, addr, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr + ":40000"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// TestAWXCallbackAddressLaunchesLikeTheNativeOne pins decision twenty-one's address: AWX's own
// callback path, with or without its trailing slash, with the key as JSON or as a form, launches
// the bound template for the matched host exactly as the native address does. The chain entry says
// the callback arrived on the AWX-compatible address, and the binding records when it was last
// called.
func TestAWXCallbackAddressLaunchesLikeTheNativeOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path        string
		ContentType string
		Body        func(key string) string
	}{{ // Test 0: The address exactly as AWX writes it.
		Path: awxPath(awxTemplateID), ContentType: "application/json", Body: keyBody,
	}, { // Test 1: Without the trailing slash.
		Path: "/api/v2/job_templates/42/callback", ContentType: "application/json", Body: keyBody,
	}, { // Test 2: The form body AWX's boot script sends.
		Path: awxPath(awxTemplateID), ContentType: "application/x-www-form-urlencoded",
		Body: func(key string) string { return "host_config_key=" + key },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
			f := newAWXFixture(t, sub, nil)
			key := f.mintKey(t, "tpl_cb", nil)
			minted := len(f.audits.entries)
			rec := f.post(http.MethodPost, test.Path, "10.0.0.11", test.ContentType, test.Body(key))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d body %s, want 201", rec.Code, rec.Body.String())
			}
			want := callbackShape{Limit: "web01", Source: "callback", SourceID: "tpl_cb",
				ActorType: "host", Actor: "host web01 from 10.0.0.11", InventoryID: "inv_1"}
			if sub.gotRun == nil {
				t.Fatal("the callback launched nothing")
			}
			if diff := cmp.Diff(want, shapeOf(sub.gotRun)); diff != "" {
				t.Errorf("launched run mismatch (-want +got):\n%s", diff)
			}
			entries := f.audits.entries[minted:]
			if len(entries) != 1 || entries[0].Path != "/v1/templates/tpl_cb/callback/fired/awx/42" {
				t.Errorf("chain entries %+v, want one recording the AWX-compatible address", entries)
			}
			b, err := f.templates.AWXBindingFor(context.Background(), awxTemplateID)
			if err != nil || b.LastCalledAt == nil {
				t.Errorf("binding %+v %v, want when it was last called", b, err)
			}
		})
	}
}

// TestAWXCallbackAddressSaysNothingAboutWhichIDsExist pins that the address does not enumerate. An
// unknown id, a malformed one, a wrong key, a template with callbacks off, and one with the AWX
// address off all answer exactly as a wrong key does, and each spends the caller's wrong-key
// budget.
func TestAWXCallbackAddressSaysNothingAboutWhichIDsExist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Mutate func(*template.Template)
		Path   string
		Key    func(key string) string
	}{{ // Test 0: An id nothing is bound to.
		Path: awxPath(43),
	}, { // Test 1: An id that is not a number.
		Path: "/api/v2/job_templates/4x2/callback/",
	}, { // Test 2: The wrong key.
		Path: awxPath(awxTemplateID), Key: func(string) string { return "hck_wrong" },
	}, { // Test 3: The AWX-compatible address turned off on the template.
		Path: awxPath(awxTemplateID), Mutate: func(t *template.Template) { t.AWXCallback = false },
	}, { // Test 4: Callbacks turned off on the template.
		Path: awxPath(awxTemplateID), Mutate: func(t *template.Template) { t.AllowCallbacks = false },
	}}
	var bodies []string
	for testNum, test := range tests {
		sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
		f := newAWXFixture(t, sub, test.Mutate)
		key := f.mintKey(t, "tpl_cb", nil)
		presented := key
		if test.Key != nil {
			presented = test.Key(key)
		}
		rec := f.post(http.MethodPost, test.Path, "10.0.0.11", "application/json", keyBody(presented))
		if rec.Code != http.StatusForbidden {
			t.Errorf("test %d: status = %d body %s, want 403", testNum, rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
		if sub.gotRun != nil {
			t.Errorf("test %d: a refused callback launched %+v", testNum, sub.gotRun)
		}
		for range DefaultCallbackKeyFailureLimit - 1 {
			f.post(http.MethodPost, test.Path, "10.0.0.11", "application/json", keyBody(presented))
		}
		if code := f.code("10.0.0.11", key); code != http.StatusTooManyRequests {
			t.Errorf("test %d: after %d refusals the right key on the native address = %d, want "+
				"429: these refusals spend the wrong-key budget", testNum,
				DefaultCallbackKeyFailureLimit, code)
		}
	}
	for testNum, body := range bodies {
		if body != bodies[0] {
			t.Errorf("test %d answered %q and test 0 answered %q: the answers tell ids apart",
				testNum, body, bodies[0])
		}
	}
}

// TestAWXCallbackAddressSpendsTheBudgetFirst pins that the rate limit is spent before anything is
// looked up, and that the native and AWX-compatible addresses share one budget per address, so
// alternating them gains a caller nothing.
func TestAWXCallbackAddressSpendsTheBudgetFirst(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newAWXFixture(t, sub, nil, WithCallbackRateLimits(2, 10))
	key := f.mintKey(t, "tpl_cb", nil)
	if rec := f.post(http.MethodPost, awxPath(43), "10.0.0.11", "application/json",
		keyBody(key)); rec.Code != http.StatusForbidden {
		t.Fatalf("an unknown id = %d, want 403", rec.Code)
	}
	if code := f.code("10.0.0.11", key); code != http.StatusCreated {
		t.Fatalf("the native address = %d, want 201", code)
	}
	rec := f.post(http.MethodPost, awxPath(awxTemplateID), "10.0.0.11", "application/json",
		keyBody(key))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("a third callback across both addresses = %d, want 429", rec.Code)
	}
	if rec := f.post(http.MethodPost, awxPath(43), "10.0.0.11", "application/json",
		keyBody(key)); rec.Code != http.StatusTooManyRequests {
		t.Errorf("an unknown id over budget = %d, want the same 429", rec.Code)
	}
}

// TestAWXCallbackAddressOfADeletedTemplateIsGone pins that a deleted template's AWX address answers
// gone and never falls through: not to a template created afterward under the same name, and not
// by spending the caller's wrong-key budget, since no key was ever judged.
func TestAWXCallbackAddressOfADeletedTemplateIsGone(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newAWXFixture(t, sub, nil, WithCallbackRateLimits(1000, 10))
	key := f.mintKey(t, "tpl_cb", nil)
	ctx := context.Background()
	if err := f.templates.Delete(ctx, "tpl_cb"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := f.templates.Save(ctx, &template.Template{ID: "tpl_new", Name: "boot",
		Playbook: "boot.yml", InventoryID: "inv_1", AllowCallbacks: true, AWXCallback: true,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save(new) error = %v", err)
	}
	newKey := f.mintKey(t, "tpl_new", nil)
	for _, presented := range []string{key, newKey, "hck_anything"} {
		for range DefaultCallbackKeyFailureLimit {
			rec := f.post(http.MethodPost, awxPath(awxTemplateID), "10.0.0.11", "application/json",
				keyBody(presented))
			if rec.Code != http.StatusGone {
				t.Fatalf("status = %d body %s, want 410", rec.Code, rec.Body.String())
			}
		}
	}
	if sub.gotRun != nil {
		t.Errorf("the deleted template's address launched %+v", sub.gotRun)
	}
	if code := f.callBack("tpl_new", "10.0.0.11", "application/json", keyBody(newKey)).Code; code !=
		http.StatusCreated {
		t.Errorf("after many gone answers the native address = %d, want 201: gone spends no "+
			"wrong-key budget", code)
	}
}

// TestAWXCallbackAddressRefusesGetAndExtraVars pins the two things the address does not do that
// AWX's does. A GET is refused, because AWX's GET can return the key and SwitchTender never returns
// one, and extra_vars is refused with a clear reason rather than ignored.
func TestAWXCallbackAddressRefusesGetAndExtraVars(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newAWXFixture(t, sub, nil)
	key := f.mintKey(t, "tpl_cb", nil)
	for _, path := range []string{awxPath(awxTemplateID), "/api/v2/job_templates/42/callback"} {
		rec := f.post(http.MethodGet, path, "10.0.0.11", "", "")
		if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"),
			http.MethodPost) {
			t.Errorf("GET %s = %d Allow %q, want 405 allowing POST", path, rec.Code,
				rec.Header().Get("Allow"))
		}
		if strings.Contains(rec.Body.String(), key) {
			t.Errorf("GET %s returned the key", path)
		}
	}
	rec := f.post(http.MethodPost, awxPath(awxTemplateID), "10.0.0.11", "application/json",
		`{"host_config_key": "`+key+`", "extra_vars": {"role": "db"}}`)
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "extra_vars is refused") {
		t.Errorf("extra_vars: status = %d body %s, want 400 refusing it", rec.Code, rec.Body.String())
	}
	if sub.gotRun != nil {
		t.Errorf("a refused callback launched %+v", sub.gotRun)
	}
}

// TestAWXCallbackAddressIsPublicOnlyInItsExactShape runs on an install that enforces tokens. A
// booting host has no account, so the exact AWX callback address needs none, while every spelling
// that only resolves to it, every other method, and every other AWX path still needs a token.
func TestAWXCallbackAddressIsPublicOnlyInItsExactShape(t *testing.T) {
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
	f := newAWXFixture(t, sub, nil, WithTokens(tokens))
	key := f.mintKey(t, "tpl_cb", http.Header{"Authorization": {"Bearer " + plain}})
	if rec := f.post(http.MethodPost, awxPath(awxTemplateID), "10.0.0.11", "application/json",
		keyBody(key)); rec.Code != http.StatusCreated {
		t.Fatalf("a host with no token: status = %d body %s, want 201", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"/api/v2/job_templates/42/callback/../launch/",
		"/api/v2/job_templates/42/callback//", "/api/v2/job_templates//callback/",
		"/api/v2/job_templates/4x2/callback/", "/api/v2/job_templates/42/callback/x",
		"/api/v2/job_templates/-42/callback/", "/api/v2/job_templates/0/callback/",
		"/api/v2/ping/", "/api/v2/job_templates/42/launch/"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/templates/x", nil)
		req.URL.Path = p
		if isCallback(req) {
			t.Errorf("isCallback(%q) = true, want only the exact AWX callback address public", p)
		}
	}
	req := httptest.NewRequest(http.MethodGet, awxPath(awxTemplateID), nil)
	if isCallback(req) {
		t.Error("isCallback(GET) = true, want a GET to need a token like any other read")
	}
}

// TestOnlyTheCallbackIsServedUnderTheAWXAPI pins the line decision twenty-one draws: the callback
// address is the single AWX path SwitchTender answers. Every other AWX API path is not found, and
// no source file in the server registers anything else under /api/.
func TestOnlyTheCallbackIsServedUnderTheAWXAPI(t *testing.T) {
	t.Parallel()
	f := newAWXFixture(t, &fakeSubmitter{}, nil)
	for _, p := range []string{"/api/v2/ping/", "/api/v2/job_templates/", "/api/v2/job_templates/42/",
		"/api/v2/job_templates/42/launch/", "/api/v2/job_templates/42/callback/x",
		"/api/v2/workflow_job_templates/42/callback/", "/api/v2/me/"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if rec := f.post(method, p, "10.0.0.11", "", ""); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, p, rec.Code)
			}
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.Contains(value, "/api/") && value != awxCallbackPrefix {
				t.Errorf("%s names the AWX API path %q: the callback address is the only one served",
					name, value)
			}
			return true
		})
	}
}

// TestTemplateShowsItsAWXAddress pins what the template page reads: the AWX job template id an
// import bound, whether the address is on, and when a host last called through it. The switch can
// be turned off and on again on a bound template, and never on for a template no import bound.
func TestTemplateShowsItsAWXAddress(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newAWXFixture(t, sub, nil)
	key := f.mintKey(t, "tpl_cb", nil)
	served := func() map[string]map[string]any {
		t.Helper()
		rec := f.post(http.MethodGet, "/v1/templates", "10.0.0.11", "", "")
		var page struct {
			// Templates are the served templates.
			Templates []map[string]any `json:"templates"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		out := map[string]map[string]any{}
		for _, tpl := range page.Templates {
			out[tpl["id"].(string)] = tpl
		}
		return out
	}
	got := served()["tpl_cb"]
	if got["awx_job_template_id"] != float64(awxTemplateID) || got["awx_callback"] != true ||
		got["awx_callback_called_at"] != nil {
		t.Errorf("served %v, want AWX id 42, the address on, and never called", got)
	}
	if rec := f.post(http.MethodPost, awxPath(awxTemplateID), "10.0.0.11", "application/json",
		keyBody(key)); rec.Code != http.StatusCreated {
		t.Fatalf("callback = %d body %s", rec.Code, rec.Body.String())
	}
	if got := served()["tpl_cb"]; got["awx_callback_called_at"] == nil {
		t.Errorf("served %v, want when the AWX address was last called", got)
	}

	put := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		return f.post(http.MethodPut, "/v1/templates/"+id, "10.0.0.11", "application/json", body)
	}
	if rec := put("tpl_cb", `{"name":"boot","playbook":"boot.yml","inventory_id":"inv_1",`+
		`"awx_callback":false}`); rec.Code != http.StatusOK {
		t.Fatalf("turning the address off = %d body %s", rec.Code, rec.Body.String())
	}
	if rec := f.post(http.MethodPost, awxPath(awxTemplateID), "10.0.0.12", "application/json",
		keyBody(key)); rec.Code != http.StatusForbidden {
		t.Errorf("a callback on an address turned off = %d, want 403", rec.Code)
	}
	if rec := put("tpl_cb", `{"name":"boot","playbook":"boot.yml","inventory_id":"inv_1",`+
		`"awx_callback":true}`); rec.Code != http.StatusOK {
		t.Errorf("turning a bound template's address back on = %d body %s", rec.Code,
			rec.Body.String())
	}
	if err := f.templates.Save(context.Background(), &template.Template{ID: "tpl_native",
		Name: "native", Playbook: "boot.yml", InventoryID: "inv_1", AllowCallbacks: true,
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rec := put("tpl_native", `{"name":"native","playbook":"boot.yml","inventory_id":"inv_1",`+
		`"awx_callback":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only an import") {
		t.Errorf("turning the address on for an unbound template = %d body %s, want 400",
			rec.Code, rec.Body.String())
	}
	rec = f.post(http.MethodPost, "/v1/templates", "10.0.0.11", "application/json",
		`{"name":"fresh","playbook":"boot.yml","inventory_id":"inv_1","awx_callback":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a new template asking for the AWX address = %d, want 400", rec.Code)
	}
}
