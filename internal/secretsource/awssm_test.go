package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAWSSigningKey checks the SigV4 signing-key derivation against the worked example AWS publishes
// in its documentation, so the crypto core is verified against an independent vector rather than the
// resolver's own round trip.
func TestAWSSigningKey(t *testing.T) {
	t.Parallel()
	// AWS documents this exact key, date, region, and service producing this signing key.
	got := hex.EncodeToString(awsSigningKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam"))
	want := "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"
	if got != want {
		t.Errorf("signing key mismatch:\n got %s\nwant %s", got, want)
	}
}

// TestResolveAWS exercises the resolver end to end against a mock Secrets Manager, checking the
// happy path, the binary path, and that the request carries a well-formed SigV4 authorization.
func TestResolveAWS(t *testing.T) {
	t.Parallel()

	var gotAuth, gotTarget, gotSecretID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTarget = r.Header.Get("X-Amz-Target")
		var in struct{ SecretId string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotSecretID = in.SecretId
		switch in.SecretId {
		case "db/password":
			_ = json.NewEncoder(w).Encode(map[string]any{"SecretString": "hunter2"})
		case "db/binary":
			enc := base64.StdEncoding.EncodeToString([]byte("binary-value"))
			_ = json.NewEncoder(w).Encode(map[string]any{"SecretBinary": enc})
		case "db/missing":
			w.WriteHeader(http.StatusBadRequest)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()

	orig := awsEndpoint
	awsEndpoint = srv.URL + "/"
	defer func() { awsEndpoint = orig }()

	cfg := func(secretID string) string {
		b, _ := json.Marshal(awsConfig{
			SecretID: secretID, Region: "us-east-1",
			AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret",
		})
		return string(b)
	}

	// Happy path: a string secret comes back and the request was signed.
	got, err := resolveAWS(context.Background(), cfg("db/password"))
	if err != nil {
		t.Fatalf("resolveAWS: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("value = %q, want hunter2", got)
	}
	if gotTarget != awsTarget {
		t.Errorf("target = %q, want %q", gotTarget, awsTarget)
	}
	if gotSecretID != "db/password" {
		t.Errorf("secret id = %q, want db/password", gotSecretID)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Errorf("authorization not a SigV4 header: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "/us-east-1/secretsmanager/aws4_request") ||
		!strings.Contains(gotAuth, "SignedHeaders=content-type;host;x-amz-date;x-amz-target") {
		t.Errorf("authorization scope or signed headers wrong: %q", gotAuth)
	}

	// Binary secrets are base64-decoded.
	got, err = resolveAWS(context.Background(), cfg("db/binary"))
	if err != nil {
		t.Fatalf("resolveAWS binary: %v", err)
	}
	if got != "binary-value" {
		t.Errorf("binary value = %q, want binary-value", got)
	}

	// A non-200 response surfaces ErrResolve.
	if _, err := resolveAWS(context.Background(), cfg("db/missing")); !errors.Is(err, ErrResolve) {
		t.Errorf("missing secret error = %v, want ErrResolve", err)
	}

	// An empty response body is a resolve failure, not an empty value.
	if _, err := resolveAWS(context.Background(), cfg("db/empty")); !errors.Is(err, ErrResolve) {
		t.Errorf("empty secret error = %v, want ErrResolve", err)
	}
}

// TestResolveAWSConfigErrors covers configs that cannot produce a value before any request is made.
// It sets AWS environment variables to isolate from the host, so it does not run in parallel.
func TestResolveAWSConfigErrors(t *testing.T) {
	tests := []struct {
		Name   string
		Config string
	}{
		{Name: "not json", Config: "{"},
		{Name: "no secret id", Config: `{"region":"us-east-1","access_key_id":"a","secret_access_key":"b"}`},
		{Name: "no credentials", Config: `{"secret_id":"x","region":"us-east-1"}`},
		{Name: "no region", Config: `{"secret_id":"x","access_key_id":"a","secret_access_key":"b"}`},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			// Isolate from any AWS_* environment on the host.
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			t.Setenv("AWS_REGION", "")
			t.Setenv("AWS_DEFAULT_REGION", "")
			if _, err := resolveAWS(context.Background(), test.Config); !errors.Is(err, ErrResolve) {
				t.Errorf("error = %v, want ErrResolve", err)
			}
		})
	}
}
