package server

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// TestTypoedControlIsRefusedAndNothingIsStored drives POST /v1/policies, which writes an approval
// rule, and proves that a misspelled control is refused rather than dropped and that the refusal
// leaves the store exactly as it was.
//
// An approval policy is the shape of the defect at its worst. max_destroy is the number of
// resources a Terraform plan may remove before the run is held for a person, and exclude_dry_run
// decides whether a check run is held at all. encoding/json drops a key it does not recognize, so
// max_destroyy stored a rule with no destroy ceiling and answered 201 with a policy body that
// looked like the one that was asked for. Nothing downstream could tell the difference, and the
// next apply that deleted a database was not held.
func TestTypoedControlIsRefusedAndNothingIsStored(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the row exercises.
		Name string
		// Body is the request body posted to /v1/policies.
		Body string
		// WantStatus is the expected response status.
		WantStatus int
		// WantError is the exact error message the response must carry, empty when it must carry none.
		WantError string
		// WantStored is the policy the store must hold afterward, nil when nothing may be written.
		WantStored *policy.Policy
	}{{ // Test 0: The correctly spelled controls are accepted and stored as sent.
		Name: "correct spelling",
		Body: `{"name":"terraform destroys","tool":"terraform","max_destroy":3,` +
			`"exclude_dry_run":true}`,
		WantStatus: http.StatusCreated,
		WantStored: &policy.Policy{
			Name: "terraform destroys", Tool: "terraform", MaxDestroy: 3, ExcludeDryRun: true,
		},
	}, { // Test 1: A misspelled destroy ceiling is refused, named, and stores nothing.
		Name:       "typo in max_destroy",
		Body:       `{"name":"terraform destroys","tool":"terraform","max_destroyy":3}`,
		WantStatus: http.StatusBadRequest,
		WantError:  `unknown field "max_destroyy" in the request body`,
	}, { // Test 2: A misspelled dry-run exclusion is refused, named, and stores nothing.
		Name:       "typo in exclude_dry_run",
		Body:       `{"name":"holds","tool":"terraform","exclude_dry_runs":true}`,
		WantStatus: http.StatusBadRequest,
		WantError:  `unknown field "exclude_dry_runs" in the request body`,
	}, { // Test 3: A body that is not JSON keeps the old generic message, not a parser dump.
		Name: "malformed json", Body: `{`,
		WantStatus: http.StatusBadRequest, WantError: "invalid request body",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := policy.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithPolicies(store)).Handler()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/policies",
				strings.NewReader(test.Body)))

			// Not fatal, so a refusal that did not happen still reports what was written. The
			// status alone is the weakest half of this test.
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
			if got := responseError(t, rec); got != test.WantError {
				t.Errorf("error message = %q, want %q", got, test.WantError)
			}
			stored, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var want []*policy.Policy
			if test.WantStored != nil {
				want = []*policy.Policy{test.WantStored}
			}
			// The id and creation time are minted by the handler, so they are dropped from the
			// comparison; every field the caller sent is compared exactly.
			diff := cmp.Diff(want, stored, cmpopts.EquateEmpty(),
				cmpopts.IgnoreFields(policy.Policy{}, "ID", "CreatedAt"))
			if diff != "" {
				t.Errorf("stored policies mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTypoedLaunchOverrideNeverReachesTheDispatcher drives the template launch endpoint, the path
// an agent over MCP and the overrides dialog both take, and proves a misspelled override stops the
// launch instead of firing it unconstrained.
//
// limit is the field that narrows a launch to one canary host. Dropped, the run reaches every host
// in the inventory and the caller is answered 202 with a run that looks like the one they asked
// for. The assertion is on what the dispatcher was handed, not on the status code.
func TestTypoedLaunchOverrideNeverReachesTheDispatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the row exercises.
		Name string
		// Body is the launch body, empty for no body at all.
		Body string
		// WantStatus is the expected response status.
		WantStatus int
		// WantError is the exact error message the response must carry, empty when it must carry none.
		WantError string
		// WantSubmitted is whether the launch must have reached the dispatcher.
		WantSubmitted bool
		// WantLimit is the host pattern the submitted run must carry.
		WantLimit string
		// WantDryRun is the no-change mode the submitted run must carry.
		WantDryRun bool
	}{{ // Test 0: A correctly spelled limit narrows the submitted run.
		Name: "correct limit", Body: `{"limit":"canary-01"}`,
		WantStatus: http.StatusAccepted, WantSubmitted: true, WantLimit: "canary-01",
	}, { // Test 1: A misspelled limit is refused and nothing is dispatched.
		Name: "typo in limit", Body: `{"limmit":"canary-01"}`,
		WantStatus: http.StatusBadRequest,
		WantError:  `unknown field "limmit" in the request body`,
	}, { // Test 2: A misspelled dry run is refused rather than launching for real.
		Name: "typo in dry_run", Body: `{"dry_runn":true}`,
		WantStatus: http.StatusBadRequest,
		WantError:  `unknown field "dry_runn" in the request body`,
	}, { // Test 3: A correctly spelled dry run still reaches the dispatcher.
		Name: "correct dry run", Body: `{"dry_run":true}`,
		WantStatus: http.StatusAccepted, WantSubmitted: true, WantDryRun: true,
	}, { // Test 4: No body at all remains a valid launch with no overrides.
		Name: "no body", Body: "",
		WantStatus: http.StatusAccepted, WantSubmitted: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			templates := template.NewMemStore()
			if err := templates.Save(context.Background(), &template.Template{
				ID: "tpl_1", Name: "deploy", Playbook: "site.yml", CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &fakeSubmitter{run: &run.Run{ID: "run_launched"}}
			handler := New(run.NewMemStore(), sub, zap.NewNop(),
				WithTemplates(templates)).Handler()

			var body io.Reader
			if test.Body != "" {
				body = strings.NewReader(test.Body)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/v1/templates/tpl_1/launch", body))

			// Not fatal, so a launch that was not refused still reports what the dispatcher was
			// handed. A bare status code is the weakest half of this test.
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
			if got := responseError(t, rec); got != test.WantError {
				t.Errorf("error message = %q, want %q", got, test.WantError)
			}
			if !test.WantSubmitted {
				if sub.gotRun != nil {
					t.Fatalf("the dispatcher was handed a run anyway: limit %q, dry run %v",
						sub.gotRun.Limit, sub.gotRun.DryRun)
				}
				return
			}
			if sub.gotRun == nil {
				t.Fatal("the launch never reached the dispatcher")
			}
			if sub.gotRun.Limit != test.WantLimit {
				t.Errorf("submitted run Limit = %q, want %q", sub.gotRun.Limit, test.WantLimit)
			}
			if sub.gotRun.DryRun != test.WantDryRun {
				t.Errorf("submitted run DryRun = %v, want %v", sub.gotRun.DryRun, test.WantDryRun)
			}
		})
	}
}

// TestForeignPayloadsStayLenient pins the named exception. A git host's webhook delivery, a vendor
// export, and a model's reply are written by somebody else and gain fields without asking us, so
// refusing one on an unknown key would break a push the day GitHub added a field. Each is asserted
// on its effect: a run reached the dispatcher, a plan named the project, a proposal was created.
func TestForeignPayloadsStayLenient(t *testing.T) {
	t.Parallel()

	t.Run("test 0 webhook delivery", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		templates := template.NewMemStore()
		if err := templates.Save(ctx, &template.Template{
			ID: "tpl_hook", Name: "deploy", Playbook: "site.yml", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		triggers := trigger.NewMemStore()
		token, tg, err := trigger.New("push", "tpl_hook")
		if err != nil {
			t.Fatalf("trigger.New() error = %v", err)
		}
		if err := triggers.Save(ctx, tg); err != nil {
			t.Fatalf("triggers.Save() error = %v", err)
		}
		sub := &fakeSubmitter{run: &run.Run{ID: "run_from_hook"}}
		handler := New(run.NewMemStore(), sub, zap.NewNop(),
			WithTemplates(templates), WithTriggers(triggers, nil)).Handler()

		// A real GitHub push body, shortened, with a field this server has never heard of.
		const delivery = `{"ref":"refs/heads/main","after":"deadbeef","zen":"Keep it logically awesome",
			"a_field_github_added_last_tuesday":{"nested":true}}`
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+token,
			strings.NewReader(delivery)))

		if rec.Code != http.StatusAccepted {
			t.Fatalf("hook status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		if sub.gotRun == nil {
			t.Fatal("the delivery was accepted but no run reached the dispatcher")
		}
		if sub.gotPlaybook != "site.yml" {
			t.Errorf("fired playbook = %q, want site.yml", sub.gotPlaybook)
		}
	})

	t.Run("test 1 vendor export", func(t *testing.T) {
		t.Parallel()
		handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()
		// An AWX export carrying keys this importer does not map, top level and nested.
		const export = `{"awx_version":"24.6.1","projects":[{"name":"infra",
			"scm_type":"git","scm_url":"https://git.example.com/infra.git",
			"custom_virtualenv":"/venv/awx"}],"unmapped_section":[{"whatever":1}]}`
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/import/awx",
			strings.NewReader(export)))

		if rec.Code != http.StatusOK {
			t.Fatalf("import status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var got importResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode plan: %v", err)
		}
		if diff := cmp.Diff([]string{"infra"}, got.Projects, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("planned projects mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("test 2 model reply", func(t *testing.T) {
		t.Parallel()
		// The model answers with the run plus a key of its own invention.
		provider := ai.ProviderFunc(func(context.Context, string, string) (string, error) {
			return `{"tool":"bash","command":"uptime","summary":"check uptime",
				"confidence":0.9,"model_notes":"harmless"}`, nil
		})
		sub := &fakeSubmitter{run: &run.Run{ID: "run_proposed"}}
		handler := New(run.NewMemStore(), sub, zap.NewNop(), WithAI(provider)).Handler()

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
			strings.NewReader(`{"intent":"check uptime"}`)))

		if rec.Code != http.StatusAccepted {
			t.Fatalf("propose status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		if sub.gotRun == nil {
			t.Fatal("the proposal was accepted but no run reached the dispatcher")
		}
		if sub.gotRun.Command != "uptime" {
			t.Errorf("proposed command = %q, want uptime", sub.gotRun.Command)
		}
	})

	t.Run("test 3 first party body is still strict", func(t *testing.T) {
		t.Parallel()
		// The propose endpoint's own request body is ours, so it is held to the strict rule even
		// though the model's reply on the same path is not.
		provider := ai.ProviderFunc(func(context.Context, string, string) (string, error) {
			return `{"tool":"bash","command":"uptime"}`, nil
		})
		sub := &fakeSubmitter{run: &run.Run{ID: "run_proposed"}}
		handler := New(run.NewMemStore(), sub, zap.NewNop(), WithAI(provider)).Handler()

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ai/propose-run",
			strings.NewReader(`{"intentt":"check uptime"}`)))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("propose status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
		}
		got, want := responseError(t, rec), `unknown field "intentt" in the request body`
		if got != want {
			t.Errorf("error message = %q, want %q", got, want)
		}
		if sub.gotRun != nil {
			t.Error("a refused proposal still reached the dispatcher")
		}
	})
}

// responseError returns the error message a recorded response carries, or empty when it carries
// none. Reading the field rather than matching the raw body keeps the assertion off JSON's own
// escaping of the quoted field name.
func responseError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	return body.Error
}

// rawJSONExemptHandlers names the handlers still allowed to reach encoding/json directly instead of
// going through decodeStrict, and says why each one is not converted yet.
//
// This is a list to be emptied, not a place to add to. An entry means that endpoint still accepts a
// misspelled control silently, so it stays only as long as the reason does. A body whose shape
// belongs to somebody else is not exempted here; it goes through decodeForeign, which the guard
// recognizes by name.
var rawJSONExemptHandlers = map[string]string{
	"createRunHandler": "createRunRequest declares no extra_vars, which the plugin tool contract " +
		"sends, so strict decoding would refuse a valid submission. Convert with that field.",
}

// TestEveryHandlerDecodesStrictly reads the handler sources and fails on any request body decoded
// without the strict rule. It scans source rather than driving each endpoint because the defect it
// guards against is a new handler forgetting the setting, and a test that drives only today's
// routes would not see tomorrow's.
//
// Thirty-odd handlers each configured their own decoder, and every one of them left
// DisallowUnknownFields off, so the whole API silently accepted a misspelled field. Centralizing
// the decoder fixes today's handlers; this keeps the next one from reintroducing it.
func TestEveryHandlerDecodesStrictly(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	fset := token.NewFileSet()
	seen := make(map[string]bool)
	for _, name := range files {
		// decode.go is where the decoder is configured, so it is the one file allowed to name
		// encoding/json. Tests decode responses freely.
		if name == "decode.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("ParseFile(%s) error = %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall || !isEncodingJSONDecode(call.Fun) {
					return true
				}
				seen[fn.Name.Name] = true
				if why, exempt := rawJSONExemptHandlers[fn.Name.Name]; exempt {
					t.Logf("%s: %s decodes a body without the strict rule: %s",
						name, fn.Name.Name, why)
					return true
				}
				t.Errorf("%s: %s decodes a request body with encoding/json directly, so an "+
					"unknown field is silently dropped; use decodeStrict, or decodeForeign when "+
					"the payload's shape is somebody else's",
					name, fn.Name.Name)
				return true
			})
		}
	}
	// An exemption that no longer matches any handler is stale, and a stale entry is how a real
	// lenient decode gets waved through later under a name that used to be innocent.
	for handler := range rawJSONExemptHandlers {
		if !seen[handler] {
			t.Errorf("rawJSONExemptHandlers names %s, which no longer decodes a body directly; "+
				"delete the entry", handler)
		}
	}
}

// isEncodingJSONDecode reports whether fun is the decoding half of encoding/json: NewDecoder, which
// yields a decoder whose settings the caller chooses, or Unmarshal, which has none to choose. The
// encoding half is left alone, since writing a response cannot accept an unknown field.
func isEncodingJSONDecode(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "json" {
		return false
	}
	return sel.Sel.Name == "NewDecoder" || sel.Sel.Name == "Unmarshal"
}
