package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// secretSurveySealer is one sealer for every test in this file. Deriving a key costs an argon2id
// pass, so sharing it keeps the file fast without sharing any state a test changes.
//
//nolint:gochecknoglobals // Not modified, simplifies testing.
var secretSurveySealer = sync.OnceValue(func() *credential.Sealer {
	return credential.NewSealer("secret-survey-test-passphrase", "secret-survey-test-salt")
})

// specCapture is a runner that records the variables each execution received and echoes the secret
// answer into the run's output, the way a careless playbook or a task failure would.
type specCapture struct {
	// mu guards vars.
	mu sync.Mutex
	// vars is the extra vars of the last execution.
	vars map[string]any
}

// Run records the spec's variables and prints the db_password answer.
func (c *specCapture) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	c.mu.Lock()
	c.vars = spec.ExtraVars
	c.mu.Unlock()
	_, _ = fmt.Fprintf(out, "connecting with password %v\n", spec.ExtraVars["db_password"])
	return roundhouse.Result{ExitCode: 0}, nil
}

// seen returns the variables the last execution received.
func (c *specCapture) seen() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vars
}

// waitTerminal polls the store until the run finishes. The deadline is one no healthy run on a
// busy machine comes near, since it only bounds a run that will never finish.
func waitTerminal(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		r, err := store.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", id)
	return nil
}

// waitOutcome polls the chain until it holds run id's outcome entry. The dispatcher records a run
// finished and then commits its outcome, so a read of the run's receipt or evidence made between
// the two finds nothing to attest yet, and on a busy machine the gap between them is long enough
// to land in.
func waitOutcome(t *testing.T, audits audit.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		chain, err := audits.Chain(context.Background())
		if err == nil {
			for _, e := range chain {
				if strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the outcome of run %s never reached the chain", id)
}

// TestASecretSurveyAnswerNeverLeavesTheExecution drives a secret survey field end to end: a
// template asks for a password, a launch answers it, a real dispatcher executes the run, and every
// surface a person, an agent, or an auditor can read is checked for the answer.
//
// AWX stores a password survey answer encrypted. Until this field type existed a survey answer here
// was stored in plain text on the run, so every importer refused to carry a password prompt across.
// The answer now reaches the tool and nothing else: not the run row, not the run's JSON, not its
// log, not the audit chain, not the receipt, not the dossier.
func TestASecretSurveyAnswerNeverLeavesTheExecution(t *testing.T) {
	// No t.Parallel: this test sets an environment variable, which the identity loader reads.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	const answer = "s3cret-survey-answer-value"
	ctx := context.Background()
	sealer := secretSurveySealer()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	templates := template.NewMemStore()
	capture := &specCapture{}
	d := dispatch.New(runs, capture, zap.NewNop(),
		dispatch.WithCredentials(credential.NewMemStore(), sealer), dispatch.WithAudits(audits),
		dispatch.WithRunFilesRoot(t.TempDir()))
	defer d.Close()
	// An authenticated admin, so the gate records every change on the chain the way it does in
	// production and the run carries a creation receipt.
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	admin, err := user.New("ops-admin", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	bearer, tok, err := auth.New("ops-admin-token")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	handler := New(runs, d, zap.NewNop(), WithTemplates(templates),
		WithCredentials(credential.NewMemStore(), sealer), WithAudit(audits),
		WithProducerIdentity(&id, "v-test"), WithTokens(tokens), WithUsers(users)).Handler()

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// AWX's spelling of the type is accepted as the secret type.
	rec := call(http.MethodPost, "/v1/templates", `{"name":"db migrate","tool":"bash",`+
		`"command":"migrate","survey":[{"var":"db_password","label":"DB password",`+
		`"type":"password","required":true},{"var":"env","label":"Env","type":"text"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create template = %d: %s", rec.Code, rec.Body.String())
	}
	var tpl template.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &tpl); err != nil {
		t.Fatalf("decode template: %v", err)
	}
	if tpl.Survey[0].Type != template.FieldSecret {
		t.Fatalf("survey type = %q, want %q", tpl.Survey[0].Type, template.FieldSecret)
	}

	rec = call(http.MethodPost, "/v1/templates/"+tpl.ID+"/launch",
		`{"answers":{"db_password":"`+answer+`","env":"staging"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch = %d: %s", rec.Code, rec.Body.String())
	}
	surfaces := map[string]string{"launch response": rec.Body.String()}
	var launched run.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &launched); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	final := waitTerminal(t, runs, launched.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("run status = %s (%s), want succeeded", final.Status, final.Error)
	}
	waitOutcome(t, audits, final.ID)

	// The tool received the answer, inside the execution.
	if got := capture.seen()["db_password"]; got != answer {
		t.Errorf("the tool received db_password = %v, want the launch's answer", got)
	}

	// The run row keeps only the sealed form, which opens to the answer and is not the answer.
	if _, ok := final.ExtraVars["db_password"]; ok {
		t.Errorf("the run's extra vars carry the secret answer: %v", final.ExtraVars)
	}
	if diff := cmp.Diff([]string{"db_password"}, final.SealedNames); diff != "" {
		t.Errorf("secret var names mismatch (-want +got):\n%s", diff)
	}
	sealed := final.SealedVars["db_password"]
	if sealed == "" || strings.Contains(sealed, answer) {
		t.Fatalf("sealed answer = %q, want ciphertext", sealed)
	}
	if opened, err := sealer.Open(sealed); err != nil || opened != answer {
		t.Errorf("the sealed answer opens to %q (%v), want the launch's answer", opened, err)
	}
	if final.ExtraVars["env"] != "staging" {
		t.Errorf("a plain answer was lost: %v", final.ExtraVars)
	}

	for _, path := range []string{
		"/v1/runs/" + final.ID,
		"/v1/runs",
		"/v1/runs/" + final.ID + "/logs",
		"/v1/runs/" + final.ID + "/events",
		"/v1/runs/" + final.ID + "/evidence?format=json",
		"/v1/runs/" + final.ID + "/evidence",
		"/v1/runs/" + final.ID + "/receipt",
		"/v1/audit",
		"/v1/audit/bundle",
		"/v1/changes",
		"/v1/templates",
	} {
		rec := call(http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		surfaces["GET "+path] = rec.Body.String()
	}
	logs := surfaces["GET /v1/runs/"+final.ID+"/logs"]
	if !strings.Contains(logs, "connecting with password ***") {
		t.Errorf("the log does not show the answer masked: %s", logs)
	}
	if !strings.Contains(surfaces["GET /v1/runs/"+final.ID], `"sealed_vars":["db_password"]`) {
		t.Errorf("the run does not say a secret answer was supplied: %s",
			surfaces["GET /v1/runs/"+final.ID])
	}

	// The chain, which feeds the SIEM stream and every export, holds neither form.
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("the chain is empty, so this test proves nothing about it")
	}
	for _, e := range chain {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		surfaces["chain entry "+e.ID] = string(b)
	}
	spec, err := outcome.Spec(final)
	if err != nil {
		t.Fatalf("Spec() error = %v", err)
	}
	if !strings.Contains(string(spec), `"sealed_vars":["db_password"]`) {
		t.Errorf("the committed spec does not record that the answer was supplied: %s", spec)
	}
	surfaces["committed spec"] = string(spec)

	for name, body := range surfaces {
		if strings.Contains(body, answer) {
			t.Errorf("%s carries the secret answer in plain text: %s", name, body)
		}
		if strings.Contains(body, sealed) {
			t.Errorf("%s carries the sealed answer: %s", name, body)
		}
	}
}

// TestASecretSurveyDefaultIsSealedAndNeverShown covers a secret field's default. AWX keeps a
// password field's default encrypted and shows it as set, and so does this: it is sealed when the
// template is saved, read back as a mask by every caller including an admin, kept by an edit that
// sends the mask back, and carried onto a launch that leaves the field unanswered without being
// opened.
func TestASecretSurveyDefaultIsSealedAndNeverShown(t *testing.T) {
	t.Parallel()
	const def = "default-secret-value"
	sealer := secretSurveySealer()
	templates := template.NewMemStore()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(templates),
		WithCredentials(credential.NewMemStore(), sealer)).Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	stored := func(id string) template.SurveyField {
		t.Helper()
		got, err := templates.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		return got.Survey[0]
	}
	surveyBody := func(def string) string {
		return `{"name":"t","tool":"bash","command":"c","survey":[{"var":"token","label":"Token",` +
			`"type":"secret","default":` + def + `}]}`
	}

	// A caller-supplied sealed_default is discarded, so ciphertext copied from elsewhere cannot be
	// planted as a default.
	rec := call(http.MethodPost, "/v1/templates", `{"name":"t","tool":"bash","command":"c",`+
		`"survey":[{"var":"token","label":"Token","type":"secret","default":"`+def+`",`+
		`"sealed_default":"planted"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	created := rec.Body.String()
	if strings.Contains(created, def) || strings.Contains(created, "sealed_default") {
		t.Errorf("the create response shows the default or its ciphertext: %s", rec.Body.String())
	}
	var tpl template.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &tpl); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tpl.Survey[0].Default != scrub.Marker {
		t.Errorf("default reads %v, want %q to say it is set", tpl.Survey[0].Default, scrub.Marker)
	}
	field := stored(tpl.ID)
	if field.Default != nil || field.SealedDefault == "planted" {
		t.Fatalf("stored field = %+v, want a sealed default and no plain one", field)
	}
	if opened, err := sealer.Open(field.SealedDefault); err != nil || opened != def {
		t.Fatalf("sealed default opens to %q (%v), want the default", opened, err)
	}
	firstSeal := field.SealedDefault

	// An admin reading the template sees the mask too.
	for _, path := range []string{"/v1/templates/" + tpl.ID, "/v1/templates"} {
		body := call(http.MethodGet, path, "").Body.String()
		if strings.Contains(body, def) || strings.Contains(body, firstSeal) {
			t.Errorf("GET %s shows the default or its ciphertext: %s", path, body)
		}
	}

	tests := []struct {
		// Name says what the edit sends.
		Name string
		// Default is the JSON default the edit sends for the field.
		Default string
		// WantSame is whether the stored sealed default must be unchanged.
		WantSame bool
		// WantOpened is what the stored default must open to, empty for none.
		WantOpened string
		// Want is the status the update must answer.
		Want int
	}{{ // Test 0: The mask sent back, as an untouched edit form does, keeps the default.
		Name: "mask echoed", Default: `"` + scrub.Marker + `"`, WantSame: true, WantOpened: def,
		Want: http.StatusOK,
	}, { // Test 1: A new default replaces it, sealed.
		Name: "new default", Default: `"rotated-default-value"`, WantOpened: "rotated-default-value",
		Want: http.StatusOK,
	}, { // Test 2: An empty default clears it.
		Name: "cleared", Default: `""`, Want: http.StatusOK,
	}, { // Test 3: A default that is not text is refused.
		Name: "not text", Default: `42`, Want: http.StatusBadRequest,
	}}
	for testNum, test := range tests {
		before := stored(tpl.ID).SealedDefault
		rec := call(http.MethodPut, "/v1/templates/"+tpl.ID, surveyBody(test.Default))
		if rec.Code != test.Want {
			t.Fatalf("test %d %s: update = %d, want %d: %s", testNum, test.Name, rec.Code, test.Want,
				rec.Body.String())
		}
		if test.Want != http.StatusOK {
			continue
		}
		after := stored(tpl.ID)
		if test.WantSame && after.SealedDefault != before {
			t.Errorf("test %d %s: the sealed default changed", testNum, test.Name)
		}
		if test.WantOpened == "" {
			if after.SealedDefault != "" {
				t.Errorf("test %d %s: a default is still stored", testNum, test.Name)
			}
			continue
		}
		if opened, err := sealer.Open(after.SealedDefault); err != nil || opened != test.WantOpened {
			t.Errorf("test %d %s: default opens to %q (%v), want %q", testNum, test.Name, opened,
				err, test.WantOpened)
		}
	}

	// A mask sent for a field with no stored default cannot stand for anything and is refused.
	masked := surveyBody(`"` + scrub.Marker + `"`)
	rec = call(http.MethodPut, "/v1/templates/"+tpl.ID, masked)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("echoing a mask with no stored default = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// A launch that leaves the field unanswered carries the sealed default, unopened.
	rec = call(http.MethodPut, "/v1/templates/"+tpl.ID, surveyBody(`"`+def+`"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset default = %d: %s", rec.Code, rec.Body.String())
	}
	rec = call(http.MethodPost, "/v1/templates/"+tpl.ID+"/launch", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch = %d: %s", rec.Code, rec.Body.String())
	}
	got := sub.gotRun
	if got == nil {
		t.Fatal("nothing was submitted")
	}
	wantSealed := map[string]string{"token": stored(tpl.ID).SealedDefault}
	if diff := cmp.Diff(wantSealed, got.SealedVars); diff != "" {
		t.Errorf("sealed vars mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]any{}, got.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the default reached the plain vars (-want +got):\n%s", diff)
	}
}

// TestASecretSurveyAnswerNeedsAKey pins the refusal for an install with no encryption key. A secret
// answer has nowhere to go but the run in plain text without one, so the launch is refused and so
// is a secret default.
func TestASecretSurveyAnswerNeedsAKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which request is made.
		Name string
		// Path is the request path.
		Path string
		// Body is the request body.
		Body string
	}{{ // Test 0: A launch answering a secret field.
		Name: "launch", Path: "/v1/templates/tpl_1/launch", Body: `{"answers":{"token":"abcdefgh"}}`,
	}, { // Test 1: A template saved with a secret default.
		Name: "create", Path: "/v1/templates", Body: `{"name":"t","tool":"bash","command":"c",` +
			`"survey":[{"var":"token","label":"Token","type":"secret","default":"abcdefgh"}]}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			templates := template.NewMemStore()
			if err := templates.Save(context.Background(), &template.Template{
				ID: "tpl_1", Name: "t", Tool: "bash", Command: "c",
				Survey: []template.SurveyField{{Var: "token", Label: "Token", Type: template.FieldSecret}},
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
			handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(templates)).Handler()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, test.Path, strings.NewReader(test.Body))
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "abcdefgh") {
				t.Errorf("the refusal repeats the secret: %s", rec.Body.String())
			}
			if sub.gotRun != nil {
				t.Error("a run was submitted without a key to seal its secret answer")
			}
		})
	}
}

// TestAnAgentSuppliesButNeverReadsASecretAnswer covers the MCP principal. An agent that may launch
// a template may answer its secret question, which is all an operator's menu asks of it, and
// reading the run back shows that an answer was supplied and nothing more.
func TestAnAgentSuppliesButNeverReadsASecretAnswer(t *testing.T) {
	t.Parallel()
	const answer = "agent-supplied-secret"
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	runs := run.NewMemStore()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "rotate", Tool: "bash", Command: "rotate",
		Survey: []template.SurveyField{
			{Var: "api_key", Label: "Key", Type: template.FieldSecret, Required: true},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	account, err := user.New("bot-owner", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, account); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	plain, tok, err := auth.New("deploy-bot")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok.UserID = account.ID
	tok.Kind = auth.KindAgent
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	sub := &storingSubmitter{store: runs}
	handler := New(runs, sub, zap.NewNop(), WithTemplates(templates), WithTokens(tokens),
		WithUsers(users), WithCredentials(credential.NewMemStore(), secretSurveySealer())).Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	rec := call(http.MethodPost, "/v1/templates/tpl_1/launch", `{"answers":{"api_key":"`+answer+`"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("agent launch = %d: %s", rec.Code, rec.Body.String())
	}
	var launched run.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &launched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stored, err := runs.Get(ctx, launched.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.SealedVars["api_key"] == "" {
		t.Fatal("the agent's answer was not sealed onto the run")
	}
	for _, path := range []string{"/v1/runs/" + launched.ID, "/v1/runs", "/v1/templates/tpl_1"} {
		body := call(http.MethodGet, path, "").Body.String()
		if strings.Contains(body, answer) || strings.Contains(body, stored.SealedVars["api_key"]) {
			t.Errorf("the agent read the secret answer back through GET %s: %s", path, body)
		}
	}
	if !strings.Contains(rec.Body.String(), `"sealed_vars":["api_key"]`) {
		t.Errorf("the run does not say a secret answer was supplied: %s", rec.Body.String())
	}
}

// storingSubmitter is a Submitter that saves the submitted run to a store, so a test can read back
// exactly what a launch put on the run without running a dispatcher.
type storingSubmitter struct {
	fakeSubmitter
	// store receives every submitted run.
	store run.Store
}

// Submit builds the run from opts and saves it.
func (s *storingSubmitter) Submit(ctx context.Context, playbook, inventory string,
	opts ...run.SubmitOption) (*run.Run, error) {
	r := &run.Run{ID: run.NewID(), Playbook: playbook, Inventory: inventory, Status: run.StatusPending,
		CreatedAt: time.Now()}
	run.ApplyOptions(r, opts)
	if err := s.store.Save(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}
