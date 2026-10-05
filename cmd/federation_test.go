package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/outcome"
)

// TestNewFederationIssuer pins that federation stays off with no URL, and that a process asked to
// federate refuses to start when it cannot: an unusable URL, no encryption key to seal the signing
// key under, or no audit chain to record which key signed each token in.
func TestNewFederationIssuer(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("cmd-federation-pass", "cmd-federation-salt")
	tests := []struct {
		URL        string
		Sealer     *credential.Sealer
		Audits     audit.Store
		WantIssuer bool
		Want       error
	}{{ // Test 0: No URL leaves federation off.
		URL: "", Sealer: sealer, Audits: audit.NewMemStore(), WantIssuer: false, Want: nil,
	}, { // Test 1: A usable URL, a sealer, and a chain make an issuer.
		URL: "https://st.example.com", Sealer: sealer, Audits: audit.NewMemStore(), WantIssuer: true,
		Want: nil,
	}, { // Test 2: Plain http to a remote host is refused.
		URL: "http://st.example.com", Sealer: sealer, Audits: audit.NewMemStore(),
		Want: federation.ErrIssuer,
	}, { // Test 3: No encryption key is refused rather than storing the key in the clear.
		URL: "https://st.example.com", Sealer: credential.NewSealer("", ""), Audits: audit.NewMemStore(),
		Want: credential.ErrNoKey,
	}, { // Test 4: No audit chain is refused rather than minting tokens no record names the key of.
		URL: "https://st.example.com", Sealer: sealer, Audits: nil, Want: federation.ErrIssuer,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := newFederationIssuer(test.URL, federation.NewMemKeyStore(), test.Sealer,
				test.Audits)
			if !errors.Is(err, test.Want) {
				t.Fatalf("newFederationIssuer() error = %v, want %v", err, test.Want)
			}
			if (got != nil) != test.WantIssuer {
				t.Errorf("newFederationIssuer() issuer = %v, want one: %v", got, test.WantIssuer)
			}
		})
	}
}

// TestTheIssuerServeAndWorkerBuildRecordsEveryToken pins the wiring both executors use: a token the
// issuer they build mints is preceded in the audit chain by an entry naming the run, the
// credential, the key that signed it, and the token's id, and carrying neither the token nor key
// material.
func TestTheIssuerServeAndWorkerBuildRecordsEveryToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("cmd-federation-pass", "cmd-federation-salt")
	audits := audit.NewMemStore()
	keys := federation.NewMemKeyStore()
	issuer, err := newFederationIssuer("https://st.example.com", keys, sealer, audits)
	if err != nil {
		t.Fatalf("newFederationIssuer() error = %v", err)
	}
	token, expires, err := issuer.Mint(ctx, federation.Claims{
		Audience: "sts.amazonaws.com", RunID: "run_wired", CredentialID: "cred_aws",
		RunType: federation.RunTypeApply, Actor: "operator-one",
	}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 || chain[0].Method != audit.MethodToken {
		t.Fatalf("chain = %+v, want one token issuance entry", chain)
	}
	got, ok := outcome.ParseTokenPath(chain[0].Path)
	if !ok {
		t.Fatalf("the entry's path %q does not read as an issuance", chain[0].Path)
	}
	if got.RunID != "run_wired" || got.CredentialID != "cred_aws" ||
		got.KeyID != jws.Signatures[0].Header.KeyID || !got.ExpiresAt.Equal(expires) {
		t.Errorf("recorded issuance = %+v, want run_wired, cred_aws, the token's kid, and its expiry",
			got)
	}
	if chain[0].OnBehalfOf != "operator-one" || chain[0].ActorType != "system" {
		t.Errorf("entry actor = %q (%s) on behalf of %q", chain[0].Actor, chain[0].ActorType,
			chain[0].OnBehalfOf)
	}
	stored, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	pem, err := sealer.Open(stored[0].Sealed)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	entry := fmt.Sprintf("%+v", *chain[0])
	for _, secret := range []string{token, stored[0].Sealed, pem} {
		if strings.Contains(entry, secret) {
			t.Error("the issuance entry carries the token or key material")
		}
	}
}

// TestFederationFlagOnEveryExecutor pins that serve and worker both take --federation-issuer. A
// worker without it would refuse every federated run it claimed, since it signs the tokens of the
// runs it executes.
func TestFederationFlagOnEveryExecutor(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"serve", "worker"} {
		if findCommand(t, c).Flags().Lookup("federation-issuer") == nil {
			t.Errorf("%s has no --federation-issuer flag", c)
		}
	}
}
