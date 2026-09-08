package secretsource

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestAWSRefusesAHalfPresentCredentialPair pins that both halves of the signing key are required.
// SigV4 will happily sign with an empty secret access key: the HMAC chain accepts it and produces a
// well-formed Authorization header. So a config that names an access key id but no secret key, or
// the reverse, does not fail locally. It fails at AWS, after the request has already carried the
// account's access key id, the secret id being read, and the caller's address to the far end. The
// refusal has to happen here, before anything is sent.
func TestAWSRefusesAHalfPresentCredentialPair(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "")

	tests := []struct {
		Access string
		Secret string
		Why    string
	}{{ // Test 0: An access key id with no secret key.
		Access: "AKIDEXAMPLE", Secret: "", Why: "access key id without a secret key",
	}, { // Test 1: A secret key with no access key id.
		Access: "", Secret: "wJalrXUtnFEMI-EXAMPLE", Why: "secret key without an access key id",
	}, { // Test 2: Neither half present.
		Access: "", Secret: "", Why: "no credentials at all",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			_, _, err := awsResolveCredentials(awsConfig{
				SecretID: "db/password", Region: "us-east-1",
				AccessKeyID: test.Access, SecretAccessKey: test.Secret,
			})
			if err == nil {
				t.Fatalf("%s was accepted; the request would be signed with an empty half of the "+
					"key pair and sent to AWS carrying the access key id", test.Why)
			}
			if !strings.Contains(err.Error(), "access_key_id") ||
				!strings.Contains(err.Error(), "secret_access_key") {
				t.Errorf("%s: error = %v, want it to name both required fields", test.Why, err)
			}
		})
	}
}

// TestAWSRefusesAHalfPresentPairFromTheEnvironment covers the same hole reached through the
// environment fallback rather than the config, since either source can supply one half.
func TestAWSRefusesAHalfPresentPairFromTheEnvironment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDFROMENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a resolve with half a key pair reached the network")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	redirectCloudEndpoints(t, srv.URL)

	_, err := resolveAWS(context.Background(), `{"secret_id":"db/password","region":"us-east-1"}`)
	if err == nil {
		t.Fatal("resolveAWS accepted an environment access key with no secret key")
	}
}

// TestSignAWSV4SortsTheCanonicalHeadersWhateverOrderTheyArriveIn pins the sort the signer does on the
// signed header names. AWS builds its side of the canonical request from the names in the
// SignedHeaders list and requires them in ascending order; a signer that emitted them in call order
// would produce a signature AWS recomputes differently and rejects. Today's two callers happen to
// pass extras that already sort last, so the sort is invisible until a caller adds a header such as
// x-amz-content-sha256 that sorts before x-amz-date. Pin it on an extra that is deliberately out of
// order, so the guarantee is tested rather than coincidental.
func TestSignAWSV4SortsTheCanonicalHeadersWhateverOrderTheyArriveIn(t *testing.T) {
	t.Parallel()
	const endpoint = "https://secretsmanager.us-east-1.amazonaws.com/"
	body := []byte(`{"SecretId":"db/password"}`)

	signedHeaders := func(extra ...string) string {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", awsContentType)
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
		req.Header.Set("X-Amz-Target", awsTarget)
		signAWSV4(req, body, baseCreds, "us-east-1", awsService, signedAt, extra...)
		_, rest, _ := strings.Cut(req.Header.Get("Authorization"), "SignedHeaders=")
		list, _, _ := strings.Cut(rest, ",")
		return list
	}

	// x-amz-target is passed first but sorts after x-amz-content-sha256, and both sort against the
	// three names the signer always includes.
	want := "content-type;host;x-amz-content-sha256;x-amz-date;x-amz-target"
	if diff := cmp.Diff(want, signedHeaders("x-amz-target", "x-amz-content-sha256")); diff != "" {
		t.Errorf("signed headers are not in ascending order, so AWS rejects the "+
			"signature (-want +got):\n%s", diff)
	}

	// The same set in a different call order must produce the same list, and therefore the same
	// signature, or the signature depends on argument order rather than on the request.
	if diff := cmp.Diff(signedHeaders("x-amz-target", "x-amz-content-sha256"),
		signedHeaders("x-amz-content-sha256", "x-amz-target")); diff != "" {
		t.Errorf("the signed header list changed with the argument order (-first +second):\n%s", diff)
	}
}

// TestResolveAWSRefusesANonOKStatusEvenWhenTheBodyLooksLikeASecret pins the status check on the
// Secrets Manager response. The success body and the error body are both JSON, and nothing but the
// status separates them. A resolver that parsed the body regardless would treat whatever a failing
// endpoint, a captive portal, or an interception proxy returned as the credential and hand it to the
// run, which is worse than failing: the run proceeds with an attacker-chosen secret.
func TestResolveAWSRefusesANonOKStatusEvenWhenTheBodyLooksLikeASecret(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI-EXAMPLE")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "")

	const planted = "PLANTED-NOT-THE-REAL-SECRET"
	for _, status := range []int{
		http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusOK,
	} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"SecretString":"` + planted + `"}`))
			}))
			defer srv.Close()
			redirectCloudEndpoints(t, srv.URL)

			got, err := resolveAWS(context.Background(),
				`{"secret_id":"db/password","region":"us-east-1"}`)
			if status == http.StatusOK {
				if err != nil || got != planted {
					t.Fatalf("a 200 resolve = %q, %v; want the secret", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("aws returned %d and the resolver still produced a value: %q", status, got)
			}
			if got != "" {
				t.Errorf("aws returned %d and the resolver produced %q; a failed resolve must "+
					"return nothing, not a body an attacker chose", status, got)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", status)) {
				t.Errorf("error = %v, want it to name the %d status so the operator can tell a "+
					"denied read from an empty secret", err, status)
			}
		})
	}
}

// TestTheSecretFetchStageNeverPutsItsResponseInTheError closes the gap the general redaction sweep
// leaves. That sweep gives every kind a config whose first network call already fails, so for the
// resolvers that authenticate before reading, azure among them, only the token stage is ever
// exercised and the secret-stage error line is never reached. The secret stage is the more dangerous
// half: its response body is the one that carries an actual credential, and its error is stored on
// the run and served back. Drive each resolver to a successful auth and a failing read.
func TestTheSecretFetchStageNeverPutsItsResponseInTheError(t *testing.T) {
	const bodySecret = "SECRET-STAGE-BODY-7b2e"

	tests := []struct {
		Kind   string
		Config string
	}{{ // Test 0: Azure with a config bearer token, so no grant runs and the read fails directly.
		Kind: KindAzure, Config: `{"vault":"kv","secret":"ci","token":"` + canary + `"}`,
	}, { // Test 1: Azure through a service principal whose token call succeeds.
		Kind: KindAzure,
		Config: `{"vault":"kv","secret":"ci","tenant_id":"t","client_id":"c",` +
			`"client_secret":"` + canary + `"}`,
	}, { // Test 2: Vault, whose read is a single authenticated call.
		Kind:   KindVault,
		Config: `{"addr":"%s","path":"secret/data/ci","field":"token","token":"` + canary + `"}`,
	}, { // Test 3: AWS Secrets Manager, signed and then refused.
		Kind: KindAWS,
		Config: `{"secret_id":"db/password","region":"us-east-1","access_key_id":"AKIDEXAMPLE",` +
			`"secret_access_key":"` + canary + `"}`,
	}, { // Test 4: 1Password Connect, whose item read is a single authenticated call.
		Kind:   KindOnePassword,
		Config: `{"url":"%s","token":"` + canary + `","vault":"Prod","item":"DB"}`,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Kind), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Any token or auth exchange succeeds, so the resolver reaches its read.
				if strings.Contains(r.URL.Path, "token") || strings.Contains(r.URL.Path, "oauth2") {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"access_token":"granted","token_type":"Bearer"}`))
					return
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"denied","value":"` + bodySecret + `",` +
					`"message":"the value is ` + bodySecret + `"}`))
			}))
			defer srv.Close()
			redirectCloudEndpoints(t, srv.URL)

			config := test.Config
			if strings.Contains(config, "%s") {
				config = fmt.Sprintf(config, srv.URL)
			}
			_, _, err := ResolveLeased(context.Background(), test.Kind, config)
			if err == nil {
				t.Fatalf("%s resolved against a refusing store, want a refusal", test.Kind)
			}
			if strings.Contains(err.Error(), bodySecret) {
				t.Errorf("the %s resolver copied the secret stage's response body into its error, "+
					"which is stored on the run and served back:\n%v", test.Kind, err)
			}
			assertNoCanary(t, test.Kind, err)
		})
	}
}
