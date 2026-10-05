package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
)

// fedSealer seals the federation keys and credentials of the tests in this file. Deriving one runs
// argon2id, so the tests share it.
var fedSealer = credential.NewSealer("federation-dispatch-pass", "federation-dispatch-salt")

// fedSeen is what the federated runner observed of one run while it executed.
type fedSeen struct {
	// env is the run's environment as the runner received it.
	env map[string]string
	// files maps each credential file to its mode while the run executed.
	files map[string]os.FileMode
	// dirMode is the mode of the directory holding the files.
	dirMode os.FileMode
	// token is the token read from the file the run was pointed at.
	token string
}

// federatedRunner stands in for a tool that reads its federated credential. It records what it saw,
// prints the token the way a careless script would, and then ends the way it is told to.
type federatedRunner struct {
	// mu guards seen.
	mu sync.Mutex
	// seen holds one observation per run.
	seen []fedSeen
	// end is ok, fail, error, or block.
	end string
	// started is signaled once a run is executing, when non-nil.
	started chan struct{}
}

// Run records the run's credential material, prints the token, and ends as configured.
func (f *federatedRunner) Run(ctx context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result,
	error) {
	s := fedSeen{env: map[string]string{}, files: map[string]os.FileMode{}}
	for _, e := range spec.Env {
		k, v, _ := strings.Cut(e, "=")
		s.env[k] = v
	}
	for _, p := range spec.CredentialFiles {
		if info, err := os.Stat(p); err == nil {
			s.files[p] = info.Mode().Perm()
		}
		if info, err := os.Stat(filepath.Dir(p)); err == nil {
			s.dirMode = info.Mode().Perm()
		}
	}
	for _, name := range []string{federation.TokenFileEnvVar, "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		if p := s.env[name]; p != "" {
			if b, err := os.ReadFile(p); err == nil {
				s.token = string(b)
			}
		}
	}
	f.mu.Lock()
	f.seen = append(f.seen, s)
	f.mu.Unlock()
	_, _ = fmt.Fprintf(out, "file token: %s\n", s.token)
	if f.started != nil {
		f.started <- struct{}{}
	}
	switch f.end {
	case "fail":
		return roundhouse.Result{ExitCode: 2}, nil
	case "error":
		return roundhouse.Result{ExitCode: -1}, errors.New("the tool could not start")
	case "block":
		<-ctx.Done()
		return roundhouse.Result{ExitCode: -1}, ctx.Err()
	default:
		return roundhouse.Result{ExitCode: 0}, nil
	}
}

// last returns the most recent observation.
func (f *federatedRunner) last(t *testing.T) fedSeen {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("the runner never executed")
	}
	return f.seen[len(f.seen)-1]
}

// fedFixture is a dispatcher wired for federation with its stores.
type fedFixture struct {
	// d is the dispatcher.
	d *Dispatcher
	// store holds the runs.
	store run.Store
	// issuer mints the tokens.
	issuer *federation.Issuer
	// creds holds the credentials.
	creds credential.Store
	// audits is the chain decisions are recorded in.
	audits audit.Store
}

// newFedFixture builds a dispatcher over runner with federation on and the given credentials
// stored.
func newFedFixture(t *testing.T, runner roundhouse.Runner, creds ...*credential.Credential) *fedFixture {
	t.Helper()
	audits := audit.NewMemStore()
	return newFedFixtureRecording(t, runner, audits, TokenEvidence(audits), creds...)
}

// newFedFixtureRecording is newFedFixture with the issuance recorder given, so a case can stand in
// a recorder that fails.
func newFedFixtureRecording(t *testing.T, runner roundhouse.Runner, audits audit.Store,
	recorder federation.IssuanceRecorder, creds ...*credential.Credential) *fedFixture {
	t.Helper()
	issuer, err := federation.NewIssuer("https://st.example.com", federation.NewMemKeyStore(),
		fedSealer, federation.WithIssuanceRecorder(recorder))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	credStore := credential.NewMemStore()
	for _, c := range creds {
		if err := credStore.Save(context.Background(), c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	store := run.NewMemStore()
	d := New(store, runner, zap.NewNop(), WithNoJanitor(), WithClaimInterval(time.Millisecond),
		WithCredentials(credStore, fedSealer), WithFederation(issuer), WithAudits(audits),
		WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	return &fedFixture{d: d, store: store, issuer: issuer, creds: credStore, audits: audits}
}

// tokenClaims verifies token against the issuer's published keys and returns its claims.
func tokenClaims(t *testing.T, issuer *federation.Issuer, token string) federation.Claims {
	t.Helper()
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("the run's token does not parse: %v", err)
	}
	set, err := issuer.JWKS(context.Background())
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	keys := set.Key(jws.Signatures[0].Header.KeyID)
	if len(keys) != 1 {
		t.Fatal("the run's token is signed by a key the issuer does not publish")
	}
	payload, err := jws.Verify(keys[0].Key)
	if err != nil {
		t.Fatalf("the run's token does not verify: %v", err)
	}
	var c federation.Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return c
}

// waitGone polls until path no longer exists, failing at the deadline. Cleanup runs as the run's
// execution returns, a moment after the terminal status lands.
func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists after the run ended", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// genericCred is an oidc_token credential.
func genericCred() *credential.Credential {
	return &credential.Credential{
		ID: "cred_oidc", Name: "vault-login", Kind: credential.KindOIDCToken,
		Settings: map[string]string{"audience": "https://vault.example.com", "environment": "prod"},
	}
}

// TestFederatedRunMintsAndLeaksNothing runs a template run with a federated credential through the
// dispatcher and pins the whole contract: the token reaches the tool through a private file and a
// variable, it names the run precisely, every printed copy is masked, nothing durable records it,
// and the files are gone once the run ends.
func TestFederatedRunMintsAndLeaksNothing(t *testing.T) {
	t.Parallel()
	runner := &federatedRunner{}
	fx := newFedFixture(t, runner, genericCred())
	r, err := fx.d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolBash), run.WithCommand("vault login"),
		run.WithCredentialIDs([]string{"cred_oidc"}), run.WithTemplate("tpl_deploy"),
		run.WithOrgID("org_acme"), run.WithActor("operator-one"), run.WithActorType("session"),
		run.WithSource("template", "tpl_deploy"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	final := waitTerminal(t, fx.store, r.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("run status = %s (%s), want succeeded", final.Status, final.Error)
	}

	seen := runner.last(t)
	tokenPath := seen.env[federation.TokenFileEnvVar]
	if tokenPath == "" || seen.token == "" {
		t.Fatal("the run received no token file")
	}
	for name, value := range seen.env {
		if strings.Contains(value, seen.token) {
			t.Errorf("%s carries the token itself, where every process the run starts inherits it",
				name)
		}
	}
	if seen.files[tokenPath] != 0o600 || seen.dirMode != 0o700 {
		t.Errorf("token file mode %o in a directory of mode %o, want 0600 in 0700",
			seen.files[tokenPath], seen.dirMode)
	}

	claims := tokenClaims(t, fx.issuer, seen.token)
	want := map[string]string{
		"run_id": r.ID, "template_id": "tpl_deploy", "org_id": "org_acme", "environment": "prod",
		"credential_id": "cred_oidc", "tool": run.ToolBash, "run_type": federation.RunTypeApply,
		"launcher_type": federation.LauncherPerson, "actor": "operator-one", "actor_type": "session",
		"source": "template", "aud": "https://vault.example.com", "purpose": federation.PurposeRun,
		"sub": "org:org_acme:project:none:template:tpl_deploy:env:prod:run_type:apply:approved:false",
	}
	got := map[string]string{
		"run_id": claims.RunID, "template_id": claims.TemplateID, "org_id": claims.OrgID,
		"environment": claims.Environment, "credential_id": claims.CredentialID, "tool": claims.Tool,
		"run_type": claims.RunType, "launcher_type": claims.LauncherType, "actor": claims.Actor,
		"actor_type": claims.ActorType, "source": claims.Source, "aud": claims.Audience,
		"sub": claims.Subject, "purpose": claims.Purpose,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("token claims mismatch (-want +got):\n%s", diff)
	}
	if claims.Approved {
		t.Error("a run nobody approved carries approved true")
	}

	waitGone(t, tokenPath)
	waitGone(t, filepath.Dir(tokenPath))

	logs, err := fx.store.Log(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if strings.Contains(string(logs), seen.token) {
		t.Error("the run's stored log holds the token")
	}
	if !strings.Contains(string(logs), "file token: ***") {
		t.Errorf("the printed token was not masked: %q", logs)
	}
	record, err := json.Marshal(final)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if strings.Contains(string(record), seen.token) {
		t.Error("the run record holds the token")
	}
	chain, err := fx.audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		blob, _ := json.Marshal(e)
		if strings.Contains(string(blob), seen.token) {
			t.Errorf("audit entry %s holds the token", e.ID)
		}
	}
}

// TestFederatedFilesGoOnEveryExit pins that the token files are removed however the run ends: it
// succeeds, the tool fails, the tool cannot start, a person cancels it mid-run, or its timeout
// fires.
func TestFederatedFilesGoOnEveryExit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		End        string
		Cancel     bool
		Timeout    int
		WantStatus run.Status
	}{{ // Test 0: The run succeeds.
		End: "ok", WantStatus: run.StatusSucceeded,
	}, { // Test 1: The tool exits non-zero.
		End: "fail", WantStatus: run.StatusFailed,
	}, { // Test 2: The tool cannot start.
		End: "error", WantStatus: run.StatusFailed,
	}, { // Test 3: A person cancels the run while the tool holds the token.
		End: "block", Cancel: true, WantStatus: run.StatusCanceled,
	}, { // Test 4: The run's timeout fires while the tool holds the token.
		End: "block", Timeout: 1, WantStatus: run.StatusFailed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runner := &federatedRunner{end: test.End, started: make(chan struct{}, 1)}
			fx := newFedFixture(t, runner, genericCred())
			opts := []run.SubmitOption{run.WithTool(run.ToolBash), run.WithCommand("work"),
				run.WithCredentialIDs([]string{"cred_oidc"})}
			if test.Timeout > 0 {
				opts = append(opts, run.WithTimeout(test.Timeout))
			}
			r, err := fx.d.Submit(context.Background(), "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			select {
			case <-runner.started:
			case <-time.After(waitBudget):
				t.Fatal("the run never started")
			}
			// The runner looked at the file while it executed. Checking here instead would race a run
			// that ends the moment it starts.
			seen := runner.last(t)
			tokenPath := seen.env[federation.TokenFileEnvVar]
			if _, ok := seen.files[tokenPath]; !ok || tokenPath == "" {
				t.Fatal("the token file was missing while the run executed")
			}
			if test.Cancel {
				fx.d.Cancel(r.ID)
			}
			final := waitTerminal(t, fx.store, r.ID)
			if final.Status != test.WantStatus {
				t.Errorf("status = %s, want %s", final.Status, test.WantStatus)
			}
			waitGone(t, filepath.Dir(tokenPath))
		})
	}
}

// TestFederatedExchangeRefusalFailsCleanly runs the exchange delivery against a fake STS that
// refuses, and pins that the run fails with the service's reason, never with the token, and leaves
// no file.
func TestFederatedExchangeRefusalFailsCleanly(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		sent string
	)
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		sent = r.PostForm.Get("WebIdentityToken")
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, "<ErrorResponse><Error><Code>AccessDenied</Code><Message>Not authorized "+
			"to perform sts:AssumeRoleWithWebIdentity with %s</Message></Error></ErrorResponse>",
			r.PostForm.Get("WebIdentityToken"))
	}))
	t.Cleanup(sts.Close)
	runner := &federatedRunner{}
	fx := newFedFixture(t, runner, &credential.Credential{
		ID: "cred_aws", Name: "aws-deploy", Kind: credential.KindAWSOIDC,
		Settings: map[string]string{"role_arn": "arn:aws:iam::123456789012:role/deploy",
			"delivery": "exchange", "sts_endpoint": sts.URL},
	})
	scratch := t.TempDir()
	fx.d.runFilesRoot = scratch
	r, err := fx.d.Submit(context.Background(), "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand("infra"), run.WithCredentialIDs([]string{"cred_aws"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	final := waitTerminal(t, fx.store, r.ID)
	if final.Status != run.StatusFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	mu.Lock()
	token := sent
	mu.Unlock()
	if token == "" {
		t.Fatal("the fake STS never received a token")
	}
	if !strings.Contains(final.Error, "AccessDenied") {
		t.Errorf("run error %q does not say STS refused", final.Error)
	}
	// The prefix is checked because the detail is capped in length, so a whole token never fits.
	if strings.Contains(final.Error, token[:32]) {
		t.Error("the run's recorded error holds the token")
	}
	runner.mu.Lock()
	ran := len(runner.seen)
	runner.mu.Unlock()
	if ran != 0 {
		t.Error("the tool ran although the exchange was refused")
	}
	// The run directory was created before the exchange was tried, so its absence now is the
	// cleanup on the failure path, not a directory that never existed.
	deadline := time.Now().Add(waitBudget)
	for {
		left, err := os.ReadDir(scratch)
		if err != nil {
			t.Fatalf("read scratch: %v", err)
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a token directory outlived the failed run: %s", left[0].Name())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMaterializeCleanupCoversALaterFailure pins that when a credential after a federated one fails
// to open, the cleanup handed back with the error still removes the token directory, which is the
// cleanup the executor runs on that path.
func TestMaterializeCleanupCoversALaterFailure(t *testing.T) {
	t.Parallel()
	other := credential.NewSealer("another-pass", "another-salt")
	sealed, err := other.Seal("API_TOKEN=x")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	fx := newFedFixture(t, &federatedRunner{}, genericCred(), &credential.Credential{
		ID: "cred_bad", Name: "sealed-elsewhere", Kind: credential.KindEnv, Secret: sealed,
	})
	spec := &roundhouse.Spec{}
	cleanup, secrets, err := fx.d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_mat", Tool: run.ToolBash,
			CredentialIDs: []string{"cred_oidc", "cred_bad"}}, spec)
	if !errors.Is(err, credential.ErrUnreadable) {
		t.Fatalf("materializeCredentials() error = %v, want %v", err, credential.ErrUnreadable)
	}
	if len(spec.CredentialFiles) != 1 {
		t.Fatalf("CredentialFiles = %v, want the token file", spec.CredentialFiles)
	}
	if len(secrets) == 0 {
		t.Error("the minted token is not in the mask list returned with the error")
	}
	dir := filepath.Dir(spec.CredentialFiles[0])
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the token directory is missing before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the token directory survived cleanup: %v", err)
	}
}

// TestFederatedTokenSharesTheRunDirectory pins that a federated token file lands in the run's one
// private credential directory beside every other credential file, so it carries that directory's
// lock, is spared by a sweep while the run holds it, and goes with it on cleanup.
func TestFederatedTokenSharesTheRunDirectory(t *testing.T) {
	t.Parallel()
	sealed, err := fedSealer.Seal("vault-pass")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	fx := newFedFixture(t, &federatedRunner{}, genericCred(), &credential.Credential{
		ID: "cred_vault", Name: "vault", Kind: credential.KindVaultPassword, Secret: sealed,
	})
	root := t.TempDir()
	fx.d.runFilesRoot = root
	spec := &roundhouse.Spec{}
	cleanup, _, err := fx.d.materializeCredentials(context.Background(),
		&run.Run{ID: "run_shared", Tool: run.ToolAnsible,
			CredentialIDs: []string{"cred_oidc", "cred_vault"}}, spec)
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("materializeCredentials() error = %v", err)
	}
	if len(spec.CredentialFiles) != 1 || len(spec.VaultPasswords) != 1 {
		t.Fatalf("CredentialFiles = %v, VaultPasswords = %v, want one of each", spec.CredentialFiles,
			spec.VaultPasswords)
	}
	tokenDir := filepath.Dir(spec.CredentialFiles[0])
	if diff := cmp.Diff(tokenDir, filepath.Dir(spec.VaultPasswords[0].Path)); diff != "" {
		t.Errorf("the token and the vault password live apart (-token +vault):\n%s", diff)
	}
	if diff := cmp.Diff(root, filepath.Dir(tokenDir)); diff != "" {
		t.Errorf("the token directory is not under the run files root (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(filepath.Join(tokenDir, ".lock")); err != nil {
		t.Errorf("the token directory carries no run lock: %v", err)
	}
	if n, err := runfiles.Sweep(root); err != nil || n != 0 {
		t.Errorf("Sweep() = %d, %v, want a live run's directory left alone", n, err)
	}
	cleanup()
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Errorf("the token directory survived cleanup: %v", err)
	}
}

// TestApprovedRunNamesItsApprover pins that a run released by an approval carries approved true,
// the approver the chain recorded, and the approved segment of its subject, so a trust policy can
// require a second person before a role is assumed.
func TestApprovedRunNamesItsApprover(t *testing.T) {
	t.Parallel()
	runner := &federatedRunner{}
	fx := newFedFixture(t, runner, genericCred())
	ctx := context.Background()
	request := &audit.Entry{ID: audit.NewID(), Actor: "operator-one", Method: http.MethodPost,
		Path: "/v1/runs"}
	if err := fx.audits.Append(ctx, request); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	reqCtx := run.WithAuditReceipt(ctx, audit.Receipt(request))
	r, err := fx.d.Submit(reqCtx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithCredentialIDs([]string{"cred_oidc"}), run.WithRequireApproval(true),
		run.WithActor("operator-one"), run.WithActorType("session"),
		run.WithHeldByPolicy("prod gate"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if r.Status != run.StatusPendingApproval {
		t.Fatalf("status = %s, want pending approval", r.Status)
	}
	if _, err := fx.d.Approve(ctx, r.ID,
		outcome.Decider{Name: "approver-two", Type: "session"}); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	final := waitTerminal(t, fx.store, r.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", final.Status, final.Error)
	}
	claims := tokenClaims(t, fx.issuer, runner.last(t).token)
	if !claims.Approved || claims.ApprovedBy != "approver-two" || claims.ApprovedByType != "session" ||
		claims.ApprovalPolicy != "prod gate" {
		t.Errorf("approval claims = approved %v by %q (%q) under %q", claims.Approved,
			claims.ApprovedBy, claims.ApprovedByType, claims.ApprovalPolicy)
	}
	if !strings.HasSuffix(claims.Subject, ":approved:true") {
		t.Errorf("sub = %q, want it to end approved:true", claims.Subject)
	}
}

// TestValidateFederatedCredentials pins the submit-time refusals: no issuer on this process, two
// credentials of one kind, settings that will not mint, and a source on a kind that reads none.
func TestValidateFederatedCredentials(t *testing.T) {
	t.Parallel()
	issuer, err := federation.NewIssuer("https://st.example.com", federation.NewMemKeyStore(),
		fedSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	store := credential.NewMemStore()
	for _, c := range []*credential.Credential{
		genericCred(),
		{ID: "cred_oidc2", Name: "second", Kind: credential.KindOIDCToken,
			Settings: map[string]string{"audience": "other"}},
		{ID: "cred_broken", Name: "broken", Kind: credential.KindAWSOIDC,
			Settings: map[string]string{"role_arn": "not-an-arn"}},
		{ID: "cred_sourced", Name: "sourced", Kind: credential.KindOIDCToken,
			Source: credential.SourceCommand, Settings: map[string]string{"audience": "x"}},
		{ID: "cred_aws", Name: "aws", Kind: credential.KindAWSOIDC,
			Settings: map[string]string{"role_arn": "arn:aws:iam::123456789012:role/deploy"}},
	} {
		if err := store.Save(context.Background(), c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	with := &Dispatcher{credentials: store, sealer: fedSealer, federation: issuer, log: zap.NewNop()}
	without := &Dispatcher{credentials: store, sealer: fedSealer, log: zap.NewNop()}
	tests := []struct {
		D    *Dispatcher
		IDs  []string
		Want error
	}{{ // Test 0: One of each kind is fine.
		D: with, IDs: []string{"cred_oidc", "cred_aws"}, Want: nil,
	}, { // Test 1: No issuer on this process.
		D: without, IDs: []string{"cred_oidc"}, Want: federation.ErrNotConfigured,
	}, { // Test 2: Two of one kind would overwrite each other's variables.
		D: with, IDs: []string{"cred_oidc", "cred_oidc2"}, Want: federation.ErrDuplicate,
	}, { // Test 3: Settings that will not mint.
		D: with, IDs: []string{"cred_broken"}, Want: federation.ErrSetting,
	}, { // Test 4: A source on a kind that stores nothing to resolve.
		D: with, IDs: []string{"cred_sourced"}, Want: federation.ErrSetting,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.D.validateCredentials(context.Background(), run.ToolTerraform, test.IDs, true)
			if !errors.Is(err, test.Want) {
				t.Errorf("validateCredentials() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestFederatedCredentialIsNotAStoredSecret pins that a federated credential named where a stored
// secret is read, a registry login, a project key, or an inventory source, is refused rather than
// handed over as an empty value.
func TestFederatedCredentialIsNotAStoredSecret(t *testing.T) {
	t.Parallel()
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), genericCred()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := &Dispatcher{credentials: store, sealer: fedSealer, log: zap.NewNop()}
	spec := &roundhouse.Spec{}
	_, err := d.resolvePullCredential(context.Background(), "cred_oidc", spec)
	if !errors.Is(err, credential.ErrBadKind) {
		t.Errorf("resolvePullCredential() error = %v, want %v", err, credential.ErrBadKind)
	}
	if spec.RegistryUsername != "" || spec.RegistryPassword != "" {
		t.Error("a federated credential reached the registry login")
	}
}

// TestLauncherType pins how the launcher_type claim names who or what started a run.
func TestLauncherType(t *testing.T) {
	t.Parallel()
	step := 1
	tests := []struct {
		Run      run.Run
		Root     run.Run
		WantType string
	}{{ // Test 0: A signed-in person.
		Run: run.Run{}, Root: run.Run{Actor: "operator-one", ActorType: "session"},
		WantType: federation.LauncherPerson,
	}, { // Test 1: An agent's token.
		Run: run.Run{}, Root: run.Run{Actor: "agent-bot", ActorType: "agent"},
		WantType: federation.LauncherAgent,
	}, { // Test 2: A schedule.
		Run: run.Run{}, Root: run.Run{Source: "schedule"}, WantType: federation.LauncherSchedule,
	}, { // Test 3: A webhook trigger.
		Run: run.Run{}, Root: run.Run{Source: "trigger", ActorType: "webhook"},
		WantType: federation.LauncherTrigger,
	}, { // Test 4: A pipeline step, whatever launched the pipeline.
		Run: run.Run{StepIndex: &step}, Root: run.Run{Actor: "operator-one", ActorType: "session"},
		WantType: federation.LauncherPipeline,
	}, { // Test 5: Nothing attributable.
		Run: run.Run{}, Root: run.Run{}, WantType: federation.LauncherSystem,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := launcherType(&test.Run, &test.Root); got != test.WantType {
				t.Errorf("launcherType() = %q, want %q", got, test.WantType)
			}
		})
	}
}

// TestAShardNamesItsParentsLaunch pins that a child run's token reads who launched and who approved
// from the run that was launched, its parent, and names the parent.
func TestAShardNamesItsParentsLaunch(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	parent := &run.Run{ID: "run_parent", Status: run.StatusRunning, CreatedAt: time.Now(),
		Actor: "operator-one", ActorType: "session", Source: "template", TemplateID: "tpl_deploy"}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	parentID := parent.ID
	child := &run.Run{ID: "run_child", ParentID: &parentID, TemplateID: "tpl_deploy", Tool: "bash"}
	d := &Dispatcher{store: store, log: zap.NewNop()}
	c := d.runClaims(ctx, child, "cred_oidc", true)
	if c.ParentRunID != "run_parent" || c.Actor != "operator-one" || c.Source != "template" ||
		c.LauncherType != federation.LauncherPerson || c.RunType != federation.RunTypeDryRun {
		t.Errorf("child claims = %+v", c)
	}
}

// TestAFederatedTokenSaysWhyItWasMinted covers the tokens minted for a module download, which
// happen before a run is recorded or when none will be. The gate's download names the run it is
// judging and says it was the gate's download, so a run id no run was recorded under, because the
// gate refused it, comes with why. A pull request review's pre-check names no run at all: it names
// the pull request and the commit it read.
func TestAFederatedTokenSaysWhyItWasMinted(t *testing.T) {
	t.Parallel()
	issuer, err := federation.NewIssuer("https://st.example.com", federation.NewMemKeyStore(),
		fedSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), genericCred()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	var mu sync.Mutex
	var minted []federation.Claims
	fetch := roundhouse.ModuleFetcherFunc(func(_ context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		for _, e := range spec.Env {
			if path, ok := strings.CutPrefix(e, federation.TokenFileEnvVar+"="); ok {
				token, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("read the token file: %v", err)
					continue
				}
				c := tokenClaims(t, issuer, string(token))
				mu.Lock()
				minted = append(minted, c)
				mu.Unlock()
			}
		}
		return roundhouse.Result{ExitCode: 0}, nil
	})
	d := New(run.NewMemStore(), fetchingRunner{Runner: okRunner(), fetch: fetch}, zap.NewNop(),
		WithNoJanitor(), WithCredentials(creds, fedSealer), WithFederation(issuer),
		WithPolicies(rulesHolding(t, excludePlans(run.ToolTerraform))),
		WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": registryCall("clean")})
	ctx := context.Background()
	d.DryRunScansAt(ctx, &run.Run{Tool: run.ToolTerraform, Command: dir, DryRun: true,
		CredentialIDs: []string{"cred_oidc"}, PinnedCommit: "0123456789abcdef",
		Labels: map[string]string{run.LabelPullRequest: "7"}})
	// Another configuration, so the gate downloads for the submission rather than reusing the
	// pre-check's download.
	writeFiles(t, dir, map[string]string{"main.tf": registryCall("clean") + "\n# next push\n"})
	submitted, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand(dir), run.WithDryRun(true), run.WithCredentialIDs([]string{"cred_oidc"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(minted) != 2 {
		t.Fatalf("minted %d tokens, want one for the pre-check and one for the gate", len(minted))
	}
	type why struct {
		// Purpose is the purpose claim.
		Purpose string
		// RunID is the run_id claim.
		RunID string
		// PullRequest is the pull_request claim.
		PullRequest string
		// CommitSHA is the commit_sha claim.
		CommitSHA string
	}
	want := []why{
		{Purpose: federation.PurposeReviewPrecheck, PullRequest: "7", CommitSHA: "0123456789abcdef"},
		{Purpose: federation.PurposeGateDownload, RunID: submitted.ID},
	}
	var got []why
	for _, c := range minted {
		got = append(got, why{Purpose: c.Purpose, RunID: c.RunID, PullRequest: c.PullRequest,
			CommitSHA: c.CommitSHA})
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("token purposes mismatch (-want +got):\n%s", diff)
	}
}
