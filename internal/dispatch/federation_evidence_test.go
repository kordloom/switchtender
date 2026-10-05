package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// tokenEntries returns the token issuance entries in audits, oldest first.
func tokenEntries(t *testing.T, audits audit.Store) []*audit.Entry {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []*audit.Entry
	for _, e := range chain {
		if e.Method == audit.MethodToken {
			out = append(out, e)
		}
	}
	return out
}

// headerKID returns the key id a token's header names.
func headerKID(t *testing.T, token string) string {
	t.Helper()
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("the token does not parse: %v", err)
	}
	return jws.Signatures[0].Header.KeyID
}

// TestFederatedRunRecordsItsSigningKey pins the evidence a federated run leaves: one chain entry,
// before the run's outcome, naming the run, the credential, the id of the key that signed the token
// the run received, the token's id, and its expiry, on behalf of whoever launched it, and holding
// neither the token nor any key material.
func TestFederatedRunRecordsItsSigningKey(t *testing.T) {
	t.Parallel()
	runner := &federatedRunner{}
	fx := newFedFixture(t, runner, genericCred())
	r, err := fx.d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolBash), run.WithCommand("vault login"),
		run.WithCredentialIDs([]string{"cred_oidc"}), run.WithActor("operator-one"),
		run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	final := waitTerminal(t, fx.store, r.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("run status = %s (%s), want succeeded", final.Status, final.Error)
	}
	token := runner.last(t).token
	claims := tokenClaims(t, fx.issuer, token)

	entries := tokenEntries(t, fx.audits)
	if len(entries) != 1 {
		t.Fatalf("token issuance entries = %d, want one for the one token", len(entries))
	}
	got, ok := outcome.ParseTokenPath(entries[0].Path)
	if !ok {
		t.Fatalf("the issuance entry's path %q does not read as one", entries[0].Path)
	}
	want := outcome.TokenIssuance{
		RunID: r.ID, CredentialID: "cred_oidc", KeyID: headerKID(t, token), TokenID: claims.ID,
		ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
	}
	if got != want {
		t.Errorf("recorded issuance = %+v, want %+v", got, want)
	}
	if entries[0].OnBehalfOf != "operator-one" || entries[0].ActorType != "system" {
		t.Errorf("the entry names %q (%s) on behalf of %q, want the issuer for operator-one",
			entries[0].Actor, entries[0].ActorType, entries[0].OnBehalfOf)
	}

	// The outcome is appended after the run's terminal write, so a run reading as terminal may not
	// have it on the chain yet, while the issuance was appended before the tool started. Waiting for
	// the outcome reads the order the chain settles on rather than the instant the status flipped.
	out := waitOutcomeEntry(t, fx.audits, r.ID)
	if !strings.HasPrefix(out.Path, "/runs/"+r.ID+"/outcome/") {
		t.Fatalf("the run's chain entry %q is not its outcome", out.Path)
	}
	if entries[0].Seq > out.Seq {
		t.Errorf("the issuance is at %d and the outcome at %d, want the issuance first",
			entries[0].Seq, out.Seq)
	}
	keys, err := fx.issuer.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	if len(keys) != 1 || keys[0].ID != got.KeyID {
		t.Errorf("the recorded key %s is not the issuer's key %+v", got.KeyID, keys)
	}
	blob, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if strings.Contains(string(blob), token) || strings.Contains(string(blob), "PRIVATE") {
		t.Error("the issuance entry carries the token or key material")
	}
}

// TestARunWhoseTokenCannotBeRecordedNeverReceivesIt pins that evidence comes first: when the chain
// refuses the issuance entry, the run fails with the reason, the tool never starts, and no token
// file is left anywhere.
func TestARunWhoseTokenCannotBeRecordedNeverReceivesIt(t *testing.T) {
	t.Parallel()
	runner := &federatedRunner{}
	refuse := federation.IssuanceRecorderFunc(func(context.Context, federation.Issuance) error {
		return errors.New("the audit chain refused the append")
	})
	fx := newFedFixtureRecording(t, runner, audit.NewMemStore(), refuse, genericCred())
	scratch := t.TempDir()
	fx.d.runFilesRoot = scratch
	r, err := fx.d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("vault login"), run.WithCredentialIDs([]string{"cred_oidc"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	final := waitTerminal(t, fx.store, r.ID)
	if final.Status != run.StatusFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	if !strings.Contains(final.Error, federation.ErrEvidence.Error()) {
		t.Errorf("run error %q does not say the issuance could not be recorded", final.Error)
	}
	runner.mu.Lock()
	ran := len(runner.seen)
	runner.mu.Unlock()
	if ran != 0 {
		t.Error("the tool ran with a token whose issuance was never recorded")
	}
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
			t.Fatalf("a run directory outlived the refused run: %s", left[0].Name())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestARefusedExchangeStillRecordsTheTokenItSent pins that a token which left the process is on
// record even when the run then failed: with the exchange delivery the token reaches the cloud's
// token service before the run starts, and the issuance entry names the key that signed the very
// token the service received.
func TestARefusedExchangeStillRecordsTheTokenItSent(t *testing.T) {
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
		_, _ = fmt.Fprint(w, "<ErrorResponse><Error><Code>AccessDenied</Code>"+
			"<Message>Not authorized</Message></Error></ErrorResponse>")
	}))
	t.Cleanup(sts.Close)
	fx := newFedFixture(t, &federatedRunner{}, &credential.Credential{
		ID: "cred_aws", Name: "aws-deploy", Kind: credential.KindAWSOIDC,
		Settings: map[string]string{"role_arn": "arn:aws:iam::123456789012:role/deploy",
			"delivery": "exchange", "sts_endpoint": sts.URL},
	})
	r, err := fx.d.Submit(context.Background(), "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand("infra"), run.WithCredentialIDs([]string{"cred_aws"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if final := waitTerminal(t, fx.store, r.ID); final.Status != run.StatusFailed {
		t.Fatalf("status = %s, want failed", final.Status)
	}
	mu.Lock()
	token := sent
	mu.Unlock()
	if token == "" {
		t.Fatal("the token service never received a token")
	}
	entries := tokenEntries(t, fx.audits)
	if len(entries) != 1 {
		t.Fatalf("token issuance entries = %d, want one", len(entries))
	}
	got, ok := outcome.ParseTokenPath(entries[0].Path)
	if !ok || got.RunID != r.ID || got.CredentialID != "cred_aws" ||
		got.KeyID != headerKID(t, token) {
		t.Errorf("recorded issuance = %+v (%v), want run %s, cred_aws, and the sent token's key", got,
			ok, r.ID)
	}
}

// TestTokenEvidenceNamesTheParentRun pins that the issuance of a pipeline step's token names the
// pipeline as a whole path segment, which is how the sparse receipt and the dossier of the run that
// was launched find a step's entries.
func TestTokenEvidenceNamesTheParentRun(t *testing.T) {
	t.Parallel()
	audits := audit.NewMemStore()
	issued := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	err := TokenEvidence(audits).RecordIssuance(context.Background(), federation.Issuance{
		RunID: "run_step", ParentRunID: "run_pipeline", CredentialID: "cred_aws", KeyID: "kid_1",
		TokenID: "jti_1", Actor: "operator-one", IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("RecordIssuance() error = %v", err)
	}
	entries := tokenEntries(t, audits)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	segments := strings.Split(entries[0].Path, "/")
	for _, id := range []string{"run_step", "run_pipeline"} {
		found := false
		for _, s := range segments {
			found = found || s == id
		}
		if !found {
			t.Errorf("the issuance path %q does not name %s as a segment", entries[0].Path, id)
		}
	}
	if !entries[0].At.Equal(issued) {
		t.Errorf("the entry records %v, want the issuance time %v", entries[0].At, issued)
	}
}
