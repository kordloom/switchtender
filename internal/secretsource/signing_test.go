package secretsource

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// signedAt is a fixed instant, so a signature is reproducible across runs.
var signedAt = time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

// signFixture signs a request built from the given parts and returns the request, so a test can
// compare the signatures of two requests that differ in exactly one way.
func signFixture(t *testing.T, method, rawURL, contentType string, body []byte,
	creds awsCredentials, region, service string, extra ...string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	if len(extra) > 0 {
		req.Header.Set("X-Amz-Target", awsTarget)
	}
	signAWSV4(req, body, creds, region, service, signedAt, extra...)
	return req
}

// baseCreds are long-lived signing credentials with no session token.
var baseCreds = awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI-EXAMPLE"}

// TestSignAWSV4ProducesAWellFormedAuthorization pins the shape of the header AWS reads. The whole
// point of signing is that Secrets Manager and STS accept the call, and a header that is subtly
// malformed fails at the far end with a signature mismatch that says nothing about which piece was
// wrong.
func TestSignAWSV4ProducesAWellFormedAuthorization(t *testing.T) {
	t.Parallel()
	req := signFixture(t, http.MethodPost, "https://secretsmanager.us-east-1.amazonaws.com/",
		awsContentType, []byte(`{"SecretId":"db/password"}`), baseCreds, "us-east-1", awsService,
		"x-amz-target")

	auth := req.Header.Get("Authorization")
	wantPrefix := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260314/us-east-1/" +
		"secretsmanager/aws4_request"
	if !strings.HasPrefix(auth, wantPrefix) {
		t.Errorf("authorization = %q, want it to start with %q", auth, wantPrefix)
	}
	wantSigned := ", SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature="
	if !strings.Contains(auth, wantSigned) {
		t.Errorf("authorization = %q, want the signed headers sorted and every one of them signed", auth)
	}
	_, sig, _ := strings.Cut(auth, "Signature=")
	if len(sig) != 64 {
		t.Errorf("signature = %q, want 64 hex characters of HMAC-SHA256", sig)
	}
	if strings.Trim(sig, "0123456789abcdef") != "" {
		t.Errorf("signature = %q, want lowercase hex", sig)
	}
	if diff := cmp.Diff("20260314T150926Z", req.Header.Get("X-Amz-Date")); diff != "" {
		t.Errorf("X-Amz-Date mismatch (-want +got):\n%s", diff)
	}
	if req.Header.Get("X-Amz-Security-Token") != "" {
		t.Error("a long-lived key was signed with a session token header it does not have")
	}
}

// TestSignAWSV4NeverPutsTheSecretKeyOnTheWire pins the one thing a signer must never do. The secret
// access key is only ever an HMAC input; the access key id is public and belongs in the credential
// scope. A signer that copied the secret key into a header would send the account's long-lived key
// to AWS in the clear on every resolve, and into any proxy log along the way.
func TestSignAWSV4NeverPutsTheSecretKeyOnTheWire(t *testing.T) {
	t.Parallel()
	creds := awsCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "SECRET-KEY-MUST-NEVER-BE-SENT",
		SessionToken:    "SESSION-TOKEN-IS-SENT-BY-DESIGN",
	}
	body := []byte(`{"SecretId":"db/password"}`)
	req := signFixture(t, http.MethodPost, "https://secretsmanager.us-east-1.amazonaws.com/",
		awsContentType, body, creds, "us-east-1", awsService, "x-amz-target")

	for name, values := range req.Header {
		for _, v := range values {
			if strings.Contains(v, creds.SecretAccessKey) {
				t.Errorf("header %s carries the secret access key: %q", name, v)
			}
		}
	}
	if req.URL.RawQuery != "" && strings.Contains(req.URL.RawQuery, creds.SecretAccessKey) {
		t.Error("the query string carries the secret access key")
	}

	// The session token, by contrast, is sent and is part of the signature, as AWS requires.
	if diff := cmp.Diff(creds.SessionToken, req.Header.Get("X-Amz-Security-Token")); diff != "" {
		t.Errorf("X-Amz-Security-Token mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(req.Header.Get("Authorization"),
		"SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-amz-target") {
		t.Errorf("the session token is not in the signed headers, so AWS rejects the call: %q",
			req.Header.Get("Authorization"))
	}
}

// TestSignAWSV4CoversEveryPartOfTheRequest proves the signature is bound to the request rather than
// merely attached to it. If any of the method, path, query, body, host, or signed header values
// could change without changing the signature, a proxy or a rewritten endpoint could alter the call
// while keeping the account's authorization intact.
//
//nolint:funlen // Test function.
func TestSignAWSV4CoversEveryPartOfTheRequest(t *testing.T) {
	t.Parallel()
	const endpoint = "https://secretsmanager.us-east-1.amazonaws.com/"
	body := []byte(`{"SecretId":"db/password"}`)
	signature := func(r *http.Request) string {
		_, sig, _ := strings.Cut(r.Header.Get("Authorization"), "Signature=")
		return sig
	}
	base := signature(signFixture(t, http.MethodPost, endpoint, awsContentType, body,
		baseCreds, "us-east-1", awsService, "x-amz-target"))

	tests := []struct {
		// Sign produces a request that differs from the base in exactly one way.
		Sign func() *http.Request
		// Why names the difference.
		Why string
	}{{ // Test 0: A different body must not carry the same signature.
		Why: "the request body",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint, awsContentType,
				[]byte(`{"SecretId":"other/secret"}`), baseCreds, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 1: A different method must not carry the same signature.
		Why: "the HTTP method",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodGet, endpoint, awsContentType, body,
				baseCreds, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 2: A different path must not carry the same signature.
		Why: "the request path",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint+"other", awsContentType, body,
				baseCreds, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 3: A different query must not carry the same signature.
		Why: "the query string",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint+"?Action=Other", awsContentType, body,
				baseCreds, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 4: A different host must not carry the same signature.
		Why: "the host",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, "https://secretsmanager.eu-west-1.amazonaws.com/",
				awsContentType, body, baseCreds, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 5: A different content type must not carry the same signature.
		Why: "the content type",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint, "application/json", body,
				baseCreds, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 6: A different region must not carry the same signature.
		Why: "the credential scope region",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint, awsContentType, body,
				baseCreds, "eu-west-1", awsService, "x-amz-target")
		},
	}, { // Test 7: A different service must not carry the same signature.
		Why: "the credential scope service",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint, awsContentType, body,
				baseCreds, "us-east-1", awsSTSService, "x-amz-target")
		},
	}, { // Test 8: A different secret key must not carry the same signature.
		Why: "the signing key",
		Sign: func() *http.Request {
			other := awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "a-different-key"}
			return signFixture(t, http.MethodPost, endpoint, awsContentType, body,
				other, "us-east-1", awsService, "x-amz-target")
		},
	}, { // Test 9: Dropping the extra signed header changes the signature and the signed header list.
		Why: "the extra signed headers",
		Sign: func() *http.Request {
			return signFixture(t, http.MethodPost, endpoint, awsContentType, body,
				baseCreds, "us-east-1", awsService)
		},
	}, { // Test 10: A session token changes the signature, since it joins the signed headers.
		Why: "the session token",
		Sign: func() *http.Request {
			creds := baseCreds
			creds.SessionToken = "session"
			return signFixture(t, http.MethodPost, endpoint, awsContentType, body,
				creds, "us-east-1", awsService, "x-amz-target")
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := signature(test.Sign()); got == base {
				t.Errorf("changing %s left the signature unchanged, so the signature does not "+
					"cover it and the call could be altered in flight", test.Why)
			}
		})
	}
}

// TestSignAWSV4IsDeterministicAndNormalizesAnEmptyPath pins two properties AWS relies on. The same
// request signed twice at the same instant has to produce the same signature, or the signer is
// reading something outside its inputs. And an endpoint written without a trailing slash has to
// sign the same as one with it, since AWS canonicalizes an empty path to "/" and would otherwise
// reject every call made against the bare host form.
func TestSignAWSV4IsDeterministicAndNormalizesAnEmptyPath(t *testing.T) {
	t.Parallel()
	body := []byte("Action=AssumeRole&Version=2011-06-15")
	signature := func(rawURL string) string {
		req := signFixture(t, http.MethodPost, rawURL, "application/x-www-form-urlencoded", body,
			baseCreds, "us-east-1", awsSTSService)
		_, sig, _ := strings.Cut(req.Header.Get("Authorization"), "Signature=")
		return sig
	}

	withSlash := signature("https://sts.us-east-1.amazonaws.com/")
	if diff := cmp.Diff(withSlash, signature("https://sts.us-east-1.amazonaws.com/")); diff != "" {
		t.Errorf("signing the same request twice differed (-first +second):\n%s", diff)
	}
	if diff := cmp.Diff(withSlash, signature("https://sts.us-east-1.amazonaws.com")); diff != "" {
		t.Errorf("a bare host endpoint signed differently from the same endpoint with a trailing "+
			"slash, so AWS rejects it (-with slash +bare):\n%s", diff)
	}
}
