package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestMintAWSSTS exercises the AWS STS minter against a mock AssumeRole endpoint, covering the minted
// env block, the signed request, the defaults, the no-op lease, and the error cases.
func TestMintAWSSTS(t *testing.T) {
	t.Parallel()
	const respXML = `<?xml version="1.0"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>ASIAEXAMPLE</AccessKeyId>
      <SecretAccessKey>secretexample</SecretAccessKey>
      <SessionToken>tokenexample</SessionToken>
      <Expiration>2026-07-21T00:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`

	var gotForm url.Values
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(b))
		switch gotForm.Get("RoleArn") {
		case "arn:aws:iam::123456789012:role/missing":
			w.WriteHeader(http.StatusForbidden)
		case "arn:aws:iam::123456789012:role/garbage":
			_, _ = w.Write([]byte("not xml"))
		case "arn:aws:iam::123456789012:role/empty":
			_, _ = w.Write([]byte(`<AssumeRoleResponse><AssumeRoleResult><Credentials></Credentials></AssumeRoleResult></AssumeRoleResponse>`))
		default:
			_, _ = w.Write([]byte(respXML))
		}
	}))
	defer srv.Close()

	orig := awsSTSEndpoint
	awsSTSEndpoint = srv.URL + "/"
	defer func() { awsSTSEndpoint = orig }()

	// cfg fills base credentials and a region so the shared credential resolution never falls back to
	// the ambient AWS environment, keeping the test independent of the machine it runs on.
	cfg := func(c awsSTSConfig) string {
		if c.Region == "" {
			c.Region = "us-east-1"
		}
		if c.AccessKeyID == "" {
			c.AccessKeyID = "AKIABASE"
		}
		if c.SecretAccessKey == "" {
			c.SecretAccessKey = "basesecret"
		}
		b, _ := json.Marshal(c)
		return string(b)
	}

	// Test 0: A successful assume-role mints an env block with the three AWS variables.
	env, lease, err := mintAWSSTS(context.Background(), cfg(awsSTSConfig{
		RoleARN: "arn:aws:iam::123456789012:role/deploy", DurationSeconds: 900, RoleSessionName: "run-1",
	}))
	if err != nil {
		t.Fatalf("mintAWSSTS: %v", err)
	}
	wantEnv := "AWS_ACCESS_KEY_ID=ASIAEXAMPLE\nAWS_SECRET_ACCESS_KEY=secretexample\nAWS_SESSION_TOKEN=tokenexample"
	if env != wantEnv {
		t.Errorf("env = %q, want %q", env, wantEnv)
	}
	if gotForm.Get("Action") != "AssumeRole" ||
		gotForm.Get("RoleArn") != "arn:aws:iam::123456789012:role/deploy" {
		t.Errorf("form = %v, want AssumeRole for the deploy role", gotForm)
	}
	if gotForm.Get("RoleSessionName") != "run-1" || gotForm.Get("DurationSeconds") != "900" {
		t.Errorf("session/duration = %q/%q, want run-1/900",
			gotForm.Get("RoleSessionName"), gotForm.Get("DurationSeconds"))
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") || !strings.Contains(gotAuth, "/sts/aws4_request") {
		t.Errorf("authorization = %q, want an STS SigV4 signature", gotAuth)
	}
	if lease == nil || lease.Kind() != KindAWSSTS {
		t.Errorf("lease = %v, want one naming aws_sts", lease)
	}
	if err := lease.Revoke(context.Background()); err != nil {
		t.Errorf("Revoke() = %v, want nil no-op", err)
	}

	// Test 1: Defaults fill the session name and duration when the config omits them.
	if _, _, err := mintAWSSTS(context.Background(), cfg(awsSTSConfig{
		RoleARN: "arn:aws:iam::123456789012:role/deploy",
	})); err != nil {
		t.Fatalf("mintAWSSTS defaults: %v", err)
	}
	if gotForm.Get("RoleSessionName") != defaultAWSSTSSessionName || gotForm.Get("DurationSeconds") != "3600" {
		t.Errorf("defaults = %q/%q, want switchtender/3600",
			gotForm.Get("RoleSessionName"), gotForm.Get("DurationSeconds"))
	}

	// Test 2: Bad config and a missing role error before any request; a forbidden response, bad XML,
	// and empty credentials error after it.
	if _, _, err := mintAWSSTS(context.Background(), "{bad"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}
	if _, _, err := mintAWSSTS(context.Background(), cfg(awsSTSConfig{})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing role error = %v, want ErrResolve", err)
	}
	for name, arn := range map[string]string{
		"forbidden": "arn:aws:iam::123456789012:role/missing",
		"bad xml":   "arn:aws:iam::123456789012:role/garbage",
		"no creds":  "arn:aws:iam::123456789012:role/empty",
	} {
		if _, _, err := mintAWSSTS(context.Background(), cfg(awsSTSConfig{RoleARN: arn})); !errors.Is(err, ErrResolve) {
			t.Errorf("%s error = %v, want ErrResolve", name, err)
		}
	}
}
