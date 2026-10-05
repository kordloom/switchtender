package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
)

// envMap splits KEY=VALUE entries into a map.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out[k] = v
	}
	return out
}

// keysOf returns a map's keys, sorted.
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readPrivate reads a delivered file, failing the test unless it is mode 0600.
func readPrivate(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s has mode %o, want 0600", path, info.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// mustParse parses settings for kind, failing the test on an error.
func mustParse(t *testing.T, kind credential.Kind, settings map[string]string) Config {
	t.Helper()
	cfg, err := ParseSettings(kind, settings)
	if err != nil {
		t.Fatalf("ParseSettings() error = %v", err)
	}
	return cfg
}

// TestDeliverByFile pins what each kind hands a run with the file delivery: the variables its tools
// read, a token file only the run's user can read, and nothing that leaves this process.
func TestDeliverByFile(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	tests := []struct {
		Kind     credential.Kind
		Settings map[string]string
		WantEnv  []string
		WantAud  string
	}{{ // Test 0: AWS sets the web identity variables every AWS SDK reads.
		Kind:     credential.KindAWSOIDC,
		Settings: map[string]string{"role_arn": testRoleARN, "region": "eu-west-1"},
		WantEnv: []string{"AWS_DEFAULT_REGION", "AWS_REGION", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
			"AWS_WEB_IDENTITY_TOKEN_FILE"},
		WantAud: DefaultAWSAudience,
	}, { // Test 1: Google binds an external_account file to the application default credentials.
		Kind:     credential.KindGCPOIDC,
		Settings: map[string]string{"provider": testProvider},
		WantEnv: []string{"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE", "GCP_AUTH_KIND",
			"GOOGLE_APPLICATION_CREDENTIALS"},
		WantAud: "https://iam.googleapis.com/" + testProvider,
	}, { // Test 2: Azure sets the workload identity convention and the azurerm OIDC variables.
		Kind: credential.KindAzureOIDC,
		Settings: map[string]string{"client_id": "app-1", "tenant_id": "tenant-1",
			"subscription_id": "sub-1"},
		WantEnv: []string{"ARM_CLIENT_ID", "ARM_OIDC_TOKEN_FILE_PATH", "ARM_SUBSCRIPTION_ID",
			"ARM_TENANT_ID", "ARM_USE_OIDC", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE",
			"AZURE_SUBSCRIPTION_ID", "AZURE_TENANT_ID"},
		WantAud: DefaultAzureAudience,
	}, { // Test 3: The generic kind hands over the token by file alone.
		Kind:     credential.KindOIDCToken,
		Settings: map[string]string{"audience": "https://vault.example.com"},
		WantEnv:  []string{TokenFileEnvVar},
		WantAud:  "https://vault.example.com",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cfg := mustParse(t, test.Kind, test.Settings)
			d, err := iss.Deliver(context.Background(), cfg, sampleClaims(), dir)
			if err != nil {
				t.Fatalf("Deliver() error = %v", err)
			}
			env := envMap(d.Env)
			if diff := cmp.Diff(test.WantEnv, keysOf(env)); diff != "" {
				t.Errorf("environment variables mismatch (-want +got):\n%s", diff)
			}
			if len(d.Secrets) != 1 {
				t.Fatalf("Secrets = %d values, want the token alone", len(d.Secrets))
			}
			token := d.Secrets[0]
			// The token reaches the tool through a file, never through a variable every process
			// the run starts would inherit.
			for name, value := range env {
				if strings.Contains(value, token) {
					t.Errorf("%s carries the token itself", name)
				}
			}
			claims, _ := verifyToken(t, iss, token)
			if claims.Audience != test.WantAud {
				t.Errorf("aud = %q, want %q", claims.Audience, test.WantAud)
			}
			for _, f := range d.Files {
				if filepath.Dir(f) != dir {
					t.Errorf("%s was written outside the run's directory", f)
				}
			}
			var tokenFile string
			for _, name := range []string{"AWS_WEB_IDENTITY_TOKEN_FILE", "AZURE_FEDERATED_TOKEN_FILE",
				TokenFileEnvVar} {
				if p, ok := env[name]; ok {
					tokenFile = p
				}
			}
			if creds, ok := env["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
				var account gcpExternalAccount
				if err := json.Unmarshal([]byte(readPrivate(t, creds)), &account); err != nil {
					t.Fatalf("external_account file is not JSON: %v", err)
				}
				if account.Type != "external_account" ||
					account.Audience != "//iam.googleapis.com/"+testProvider ||
					account.SubjectTokenType != gcpJWTTokenType || account.TokenURL != defaultGCPSTSURL {
					t.Errorf("external_account file = %+v", account)
				}
				tokenFile = account.CredentialSource.File
			}
			if tokenFile == "" {
				t.Fatal("no variable names the token file")
			}
			if got := readPrivate(t, tokenFile); got != token {
				t.Error("the token file does not hold the minted token")
			}
		})
	}
}

// TestDeliverGCPImpersonation pins that naming a service account adds the impersonation URL the
// client libraries follow, pointed at the IAM endpoint the credential names.
func TestDeliverGCPImpersonation(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	cfg := mustParse(t, credential.KindGCPOIDC, map[string]string{
		"provider": testProvider, "service_account": "deployer@my-project.iam.gserviceaccount.com",
	})
	d, err := iss.Deliver(context.Background(), cfg, sampleClaims(), t.TempDir())
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	var account gcpExternalAccount
	if err := json.Unmarshal([]byte(readPrivate(t, envMap(d.Env)["GOOGLE_APPLICATION_CREDENTIALS"])),
		&account); err != nil {
		t.Fatalf("external_account file is not JSON: %v", err)
	}
	want := "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" +
		"deployer@my-project.iam.gserviceaccount.com:generateAccessToken"
	if account.ServiceAccountImpersonationURL != want {
		t.Errorf("impersonation URL = %q, want %q", account.ServiceAccountImpersonationURL, want)
	}
}

// fakeSTS records the last request an exchange made and answers with a canned reply.
type fakeSTS struct {
	// mu guards the recorded request.
	mu sync.Mutex
	// form is the last request's decoded form body.
	form url.Values
	// body is the last request's raw body.
	body string
	// auth is the last request's Authorization header.
	auth string
	// path is the last request's path.
	path string
	// status is the reply status.
	status int
	// reply is the reply body.
	reply string
}

// ServeHTTP records the request and writes the canned reply.
func (f *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.body, f.auth, f.path = string(b), r.Header.Get("Authorization"), r.URL.Path
	f.form, _ = url.ParseQuery(string(b))
	status, reply := f.status, f.reply
	f.mu.Unlock()
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

// awsAssumeReply is a successful AssumeRoleWithWebIdentity answer.
const awsAssumeReply = `<AssumeRoleWithWebIdentityResponse>
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>ASIAFEDERATED</AccessKeyId>
      <SecretAccessKey>federated-secret-key</SecretAccessKey>
      <SessionToken>federated-session-token</SessionToken>
      <Expiration>2026-10-01T10:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`

// TestDeliverAWSExchange runs the exchange delivery against a fake STS and pins the request it
// makes, that it carries no AWS signature because the token is the authentication, the variables it
// injects, that every returned value is masked, and that no token file is written.
func TestDeliverAWSExchange(t *testing.T) {
	t.Parallel()
	sts := &fakeSTS{status: http.StatusOK, reply: awsAssumeReply}
	srv := httptest.NewServer(sts)
	t.Cleanup(srv.Close)
	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	cfg := mustParse(t, credential.KindAWSOIDC, map[string]string{
		"role_arn": testRoleARN, "delivery": "exchange", "sts_endpoint": srv.URL + "/",
		"session_duration": "1800", "region": "us-east-1",
	})
	dir := t.TempDir()
	d, err := iss.Deliver(context.Background(), cfg, sampleClaims(), dir)
	if err != nil {
		t.Fatalf("Deliver() error = %v", err)
	}
	sts.mu.Lock()
	form, auth := sts.form, sts.auth
	sts.mu.Unlock()
	if auth != "" {
		t.Errorf("the exchange sent an Authorization header %q; it needs none", auth)
	}
	wantForm := map[string]string{
		"Action": "AssumeRoleWithWebIdentity", "Version": "2011-06-15", "RoleArn": testRoleARN,
		"RoleSessionName": "switchtender-run_abc", "DurationSeconds": "1800",
	}
	for k, v := range wantForm {
		if form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
		}
	}
	claims, _ := verifyToken(t, iss, form.Get("WebIdentityToken"))
	if claims.RunID != "run_abc" || claims.Audience != DefaultAWSAudience {
		t.Errorf("the token sent to STS names run %q for %q", claims.RunID, claims.Audience)
	}
	wantEnv := map[string]string{
		"AWS_ACCESS_KEY_ID": "ASIAFEDERATED", "AWS_SECRET_ACCESS_KEY": "federated-secret-key",
		"AWS_SESSION_TOKEN": "federated-session-token", "AWS_REGION": "us-east-1",
		"AWS_DEFAULT_REGION": "us-east-1",
	}
	if diff := cmp.Diff(wantEnv, envMap(d.Env)); diff != "" {
		t.Errorf("environment mismatch (-want +got):\n%s", diff)
	}
	for _, s := range []string{form.Get("WebIdentityToken"), "ASIAFEDERATED", "federated-secret-key",
		"federated-session-token"} {
		found := false
		for _, m := range d.Secrets {
			found = found || m == s
		}
		if !found {
			t.Errorf("%q is not in the mask list", s)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 || len(d.Files) != 0 {
		t.Errorf("the exchange delivery wrote %d files, want none", len(entries))
	}
}

// TestDeliverAWSExchangeRefused pins that a refusal from STS fails the delivery with the service's
// reason and never with the token, even when the service echoes it.
func TestDeliverAWSExchangeRefused(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		sent string
	)
	echo := func() string {
		mu.Lock()
		defer mu.Unlock()
		return sent
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		mu.Lock()
		sent = form.Get("WebIdentityToken")
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>Not authorized `+
			`for token %s</Message></Error></ErrorResponse>`, form.Get("WebIdentityToken"))
	}))
	t.Cleanup(srv.Close)
	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	cfg := mustParse(t, credential.KindAWSOIDC, map[string]string{
		"role_arn": testRoleARN, "delivery": "exchange", "sts_endpoint": srv.URL,
	})
	d, err := iss.Deliver(context.Background(), cfg, sampleClaims(), t.TempDir())
	if !errors.Is(err, ErrExchange) {
		t.Fatalf("Deliver() error = %v, want %v", err, ErrExchange)
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("error %q does not say why STS refused", err)
	}
	// The prefix is what is checked, because the detail is capped in length: a whole token never
	// fits, so a check for all of it would pass while most of the token went into the record.
	if token := echo(); len(token) < 32 || strings.Contains(err.Error(), token[:32]) {
		t.Error("the refusal carried the token into the error")
	}
	if len(d.Secrets) == 0 {
		t.Error("the minted token is not handed back for masking when the exchange fails")
	}
}

// TestDeliverExchangeGarbled pins that a reply with no usable credentials is a refusal, not an
// empty login.
func TestDeliverExchangeGarbled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind  credential.Kind
		Reply string
	}{{ // Test 0: AWS answers with something that is not XML.
		Kind: credential.KindAWSOIDC, Reply: "not xml",
	}, { // Test 1: AWS answers with empty credentials.
		Kind: credential.KindAWSOIDC,
		Reply: `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult>` +
			`<Credentials></Credentials></AssumeRoleWithWebIdentityResult>` +
			`</AssumeRoleWithWebIdentityResponse>`,
	}, { // Test 2: Google answers with no access token.
		Kind: credential.KindGCPOIDC, Reply: `{"token_type":"Bearer"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(&fakeSTS{status: http.StatusOK, reply: test.Reply})
			t.Cleanup(srv.Close)
			iss, _ := newTestIssuer(t, "https://st.example.com", nil)
			settings := map[string]string{"delivery": "exchange", "sts_endpoint": srv.URL}
			if test.Kind == credential.KindAWSOIDC {
				settings["role_arn"] = testRoleARN
			} else {
				settings["provider"] = testProvider
			}
			_, err := iss.Deliver(context.Background(), mustParse(t, test.Kind, settings), sampleClaims(),
				t.TempDir())
			if !errors.Is(err, ErrExchange) {
				t.Errorf("Deliver() error = %v, want %v", err, ErrExchange)
			}
		})
	}
}

// TestDeliverGCPExchange runs the Google exchange against fake STS and IAM credentials services and
// pins both legs: the token exchange request, the impersonation call made with the federated token,
// and the service account token delivered to the run.
func TestDeliverGCPExchange(t *testing.T) {
	t.Parallel()
	sts := &fakeSTS{status: http.StatusOK,
		reply: `{"access_token":"federated-access","token_type":"Bearer","expires_in":3600}`}
	stsSrv := httptest.NewServer(sts)
	t.Cleanup(stsSrv.Close)
	iam := &fakeSTS{status: http.StatusOK,
		reply: `{"accessToken":"impersonated-access","expireTime":"2026-10-01T10:00:00Z"}`}
	iamSrv := httptest.NewServer(iam)
	t.Cleanup(iamSrv.Close)

	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	tests := []struct {
		ServiceAccount string
		WantAccess     string
	}{{ // Test 0: With no service account the federated token is delivered.
		ServiceAccount: "", WantAccess: "federated-access",
	}, { // Test 1: With one, the impersonated token is delivered.
		ServiceAccount: "deployer@my-project.iam.gserviceaccount.com", WantAccess: "impersonated-access",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			settings := map[string]string{
				"provider": testProvider, "delivery": "exchange", "sts_endpoint": stsSrv.URL + "/v1/token",
				"iam_endpoint": iamSrv.URL,
			}
			if test.ServiceAccount != "" {
				settings["service_account"] = test.ServiceAccount
			}
			d, err := iss.Deliver(context.Background(), mustParse(t, credential.KindGCPOIDC, settings),
				sampleClaims(), t.TempDir())
			if err != nil {
				t.Fatalf("Deliver() error = %v", err)
			}
			wantEnv := map[string]string{
				"GOOGLE_OAUTH_ACCESS_TOKEN": test.WantAccess, "GCP_ACCESS_TOKEN": test.WantAccess,
				"GCP_AUTH_KIND": "accesstoken",
			}
			if diff := cmp.Diff(wantEnv, envMap(d.Env)); diff != "" {
				t.Errorf("environment mismatch (-want +got):\n%s", diff)
			}
			sts.mu.Lock()
			form := sts.form
			sts.mu.Unlock()
			wantForm := map[string]string{
				"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
				"audience":             "//iam.googleapis.com/" + testProvider,
				"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
				"subject_token_type":   gcpJWTTokenType,
				"scope":                gcpCloudPlatformScope,
			}
			for k, v := range wantForm {
				if form.Get(k) != v {
					t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
				}
			}
			verifyToken(t, iss, form.Get("subject_token"))
			if test.ServiceAccount == "" {
				return
			}
			iam.mu.Lock()
			auth, path, body := iam.auth, iam.path, iam.body
			iam.mu.Unlock()
			if auth != "Bearer federated-access" {
				t.Errorf("impersonation Authorization = %q, want the federated token", auth)
			}
			if path != "/v1/projects/-/serviceAccounts/"+test.ServiceAccount+":generateAccessToken" {
				t.Errorf("impersonation path = %q", path)
			}
			if !strings.Contains(body, gcpCloudPlatformScope) {
				t.Errorf("impersonation body %q does not ask for the cloud-platform scope", body)
			}
		})
	}
}

// TestDeliverGCPExchangeRefused pins that a refusal at either leg fails the delivery with the
// reason and without the tokens.
func TestDeliverGCPExchangeRefused(t *testing.T) {
	t.Parallel()
	stsDenied := &fakeSTS{status: http.StatusBadRequest,
		reply: `{"error":"invalid_grant","error_description":"The audience in ID Token does not match"}`}
	stsOK := &fakeSTS{status: http.StatusOK, reply: `{"access_token":"federated-access"}`}
	iamDenied := &fakeSTS{status: http.StatusForbidden,
		reply: `{"error":{"status":"PERMISSION_DENIED",` +
			`"message":"caller federated-access lacks permission"}}`}
	tests := []struct {
		STS        *fakeSTS
		WantReason string
	}{{ // Test 0: The token exchange is refused.
		STS: stsDenied, WantReason: "invalid_grant",
	}, { // Test 1: The exchange succeeds and the impersonation is refused.
		STS: stsOK, WantReason: "PERMISSION_DENIED",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			stsSrv := httptest.NewServer(test.STS)
			t.Cleanup(stsSrv.Close)
			iamSrv := httptest.NewServer(iamDenied)
			t.Cleanup(iamSrv.Close)
			iss, _ := newTestIssuer(t, "https://st.example.com", nil)
			cfg := mustParse(t, credential.KindGCPOIDC, map[string]string{
				"provider": testProvider, "delivery": "exchange", "sts_endpoint": stsSrv.URL,
				"iam_endpoint": iamSrv.URL, "service_account": "deployer@my-project.iam.gserviceaccount.com",
			})
			_, err := iss.Deliver(context.Background(), cfg, sampleClaims(), t.TempDir())
			if !errors.Is(err, ErrExchange) {
				t.Fatalf("Deliver() error = %v, want %v", err, ErrExchange)
			}
			if !strings.Contains(err.Error(), test.WantReason) {
				t.Errorf("error %q does not carry %q", err, test.WantReason)
			}
			if strings.Contains(err.Error(), "federated-access") {
				t.Errorf("error %q carries the federated token", err)
			}
		})
	}
}

// TestDeliverRefusesAnOccupiedPath pins that a token file is never written through something
// already at its path, such as a link planted in the directory before the run.
func TestDeliverRefusesAnOccupiedPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(elsewhere, filepath.Join(dir, "oidc-token")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	cfg := mustParse(t, credential.KindOIDCToken, map[string]string{"audience": "vault"})
	if _, err := iss.Deliver(context.Background(), cfg, sampleClaims(), dir); err == nil {
		t.Fatal("Deliver() wrote through a link already at the token path")
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Error("the token reached the link's target")
	}
}

// TestAnAWSSessionSaysWhyItWasMinted pins the role session name CloudTrail records for a token
// SwitchTender minted. A run's session names the run. The gate's module download names the run it
// was judging and that it was the gate's, and a pull request review's pre-check, which no run
// exists for, names the pull request.
func TestAnAWSSessionSaysWhyItWasMinted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Claims are the token's claims.
		Claims Claims
		// WantName is the session name.
		WantName string
	}{{ // Test 0: A run.
		Claims: Claims{RunID: "run_abc", Purpose: PurposeRun}, WantName: "switchtender-run_abc",
	}, { // Test 1: The gate's download before the run is recorded.
		Claims:   Claims{RunID: "run_abc", Purpose: PurposeGateDownload},
		WantName: "switchtender-gate-run_abc",
	}, { // Test 2: A review's pre-check, which names no run.
		Claims:   Claims{Purpose: PurposeReviewPrecheck, PullRequest: "7"},
		WantName: "switchtender-review-7",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantName, defaultSessionName(test.Claims)); diff != "" {
				t.Errorf("session name mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
