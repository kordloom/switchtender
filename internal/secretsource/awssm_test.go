package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestAWSResolveCredentialsRegion checks that a config-controlled region is validated before it can be
// spliced into the request authority. A region like "evil.example.com#" would otherwise build
// https://secretsmanager.evil.example.com#.amazonaws.com/, whose host the SSRF guard reads as a benign
// public name, sending the SigV4-signed request and any ambient AWS session token to the attacker. The
// same shared credential resolver guards the STS minter. It sets AWS_* environment, so it is serial.
func TestAWSResolveCredentialsRegion(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	// Precondition: the raw interpolation of an escaping region yields an attacker host that the SSRF
	// guard accepts, which is exactly what the region check must prevent.
	escaped := fmt.Sprintf("https://%s.%s.amazonaws.com/", awsService, "evil.example.com#")
	u, err := url.Parse(escaped)
	if err != nil || u.Hostname() != "secretsmanager.evil.example.com" {
		t.Fatalf("precondition: interpolated host = %q, %v; want secretsmanager.evil.example.com", u.Hostname(), err)
	}
	if err := checkResolveURL(escaped); err != nil {
		t.Fatalf("precondition: checkResolveURL(%q) = %v, want nil", escaped, err)
	}

	base := awsConfig{AccessKeyID: "AKID", SecretAccessKey: "secret"}
	// Test 0: Regions that would escape the amazonaws.com authority are rejected.
	bad := []string{
		"evil.example.com#",
		"evil.example.com/",
		"evil.example.com?",
		"us-east-1@evil.example.com",
		"us east 1",
		"US-EAST-1",
		"pct%23",
	}
	for _, region := range bad {
		c := base
		c.Region = region
		if _, _, err := awsResolveCredentials(c); !errors.Is(err, ErrResolve) {
			t.Errorf("region %q = %v, want ErrResolve", region, err)
		}
	}

	// Test 1: A valid region resolves and builds the expected regional authority.
	c := base
	c.Region = "us-east-1"
	_, region, err := awsResolveCredentials(c)
	if err != nil || region != "us-east-1" {
		t.Fatalf("valid region = %q, %v; want us-east-1", region, err)
	}
	got, err := url.Parse(fmt.Sprintf("https://%s.%s.amazonaws.com/", awsService, region))
	if err != nil || got.Hostname() != "secretsmanager.us-east-1.amazonaws.com" {
		t.Errorf("endpoint host = %q, %v; want secretsmanager.us-east-1.amazonaws.com", got.Hostname(), err)
	}

	// Test 2: The STS minter shares the credential resolver, so an escaping region is rejected there
	// too, before any request that would carry ambient AWS credentials.
	stsCfg := `{"role_arn":"arn:aws:iam::123456789012:role/x","region":"evil.example.com#",` +
		`"access_key_id":"a","secret_access_key":"b"}`
	if _, _, err := mintAWSSTS(context.Background(), stsCfg); !errors.Is(err, ErrResolve) {
		t.Errorf("sts escaping region = %v, want ErrResolve", err)
	}
}
