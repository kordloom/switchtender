package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestResolveAWSSendsTheVersionTheSourceNames pins that a source naming a specific version or
// staging label reads that one. Secrets Manager answers a request with neither by returning
// AWSCURRENT, so a resolver that dropped the fields would silently read the current secret while
// the source says otherwise, which is how a rotation rollback quietly stops working.
//
// It redirects the package-level endpoint, so it does not run in parallel.
func TestResolveAWSSendsTheVersionTheSourceNames(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		switch got["SecretId"] {
		case "db/badbinary":
			_, _ = w.Write([]byte(`{"SecretBinary":"not base64 at all!!"}`))
		case "db/garbage":
			_, _ = w.Write([]byte(`<html>not json</html>`))
		default:
			_, _ = w.Write([]byte(`{"SecretString":"value"}`))
		}
	}))
	defer srv.Close()

	orig := awsEndpoint
	awsEndpoint = srv.URL + "/"
	defer func() { awsEndpoint = orig }()

	cfg := func(c awsConfig) string {
		c.Region, c.AccessKeyID, c.SecretAccessKey = "us-east-1", "AKID", "secret"
		b, _ := json.Marshal(c)
		return string(b)
	}

	// Test 0: A version id is sent as VersionId.
	if _, err := resolveAWS(context.Background(),
		cfg(awsConfig{SecretID: "db/password", VersionID: "v-123"})); err != nil {
		t.Fatalf("version id resolve: %v", err)
	}
	if got["VersionId"] != "v-123" {
		t.Errorf("VersionId = %q, want v-123; the source's version was dropped", got["VersionId"])
	}

	// Test 1: A staging label is sent as VersionStage.
	if _, err := resolveAWS(context.Background(),
		cfg(awsConfig{SecretID: "db/password", VersionStage: "AWSPREVIOUS"})); err != nil {
		t.Fatalf("version stage resolve: %v", err)
	}
	if got["VersionStage"] != "AWSPREVIOUS" {
		t.Errorf("VersionStage = %q, want AWSPREVIOUS", got["VersionStage"])
	}

	// Test 2: Neither field is sent when the source names no version, so AWS returns the current one.
	if _, err := resolveAWS(context.Background(),
		cfg(awsConfig{SecretID: "db/password"})); err != nil {
		t.Fatalf("plain resolve: %v", err)
	}
	if _, ok := got["VersionId"]; ok {
		t.Error("VersionId was sent for a source that names no version")
	}
	if _, ok := got["VersionStage"]; ok {
		t.Error("VersionStage was sent for a source that names no staging label")
	}

	// Test 3: A binary secret that is not valid base64 is a refusal, not a mangled value.
	if v, err := resolveAWS(context.Background(),
		cfg(awsConfig{SecretID: "db/badbinary"})); !errors.Is(err, ErrResolve) || v != "" {
		t.Errorf("undecodable binary secret = %q, %v; want ErrResolve", v, err)
	}

	// Test 4: A response that is not JSON is a refusal.
	if v, err := resolveAWS(context.Background(),
		cfg(awsConfig{SecretID: "db/garbage"})); !errors.Is(err, ErrResolve) || v != "" {
		t.Errorf("non-JSON response = %q, %v; want ErrResolve", v, err)
	}
}

// TestMintAWSSTSSendsTheExternalIDACrossAccountTrustNeeds pins that an external id reaches STS. A
// cross-account role trust that requires one refuses the assume without it, so dropping the field
// turns a working role into a permanent access-denied that looks like a permissions problem at the
// other account.
//
// It redirects the package-level endpoint, so it does not run in parallel.
func TestMintAWSSTSSendsTheExternalIDACrossAccountTrustNeeds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	const respXML = `<AssumeRoleResponse><AssumeRoleResult><Credentials>` +
		`<AccessKeyId>ASIA</AccessKeyId><SecretAccessKey>sk</SecretAccessKey>` +
		`<SessionToken>st</SessionToken></Credentials></AssumeRoleResult></AssumeRoleResponse>`
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(respXML))
	}))
	defer srv.Close()

	orig := awsSTSEndpoint
	awsSTSEndpoint = srv.URL + "/"
	defer func() { awsSTSEndpoint = orig }()

	cfg := `{"role_arn":"arn:aws:iam::123456789012:role/x","region":"us-east-1",` +
		`"access_key_id":"AKID","secret_access_key":"secret","external_id":"ext-42",` +
		`"duration_seconds":-5}`
	if _, _, err := mintAWSSTS(context.Background(), cfg); err != nil {
		t.Fatalf("mintAWSSTS: %v", err)
	}
	if form.Get("ExternalId") != "ext-42" {
		t.Errorf("ExternalId = %q, want ext-42; a cross-account trust requiring it would refuse",
			form.Get("ExternalId"))
	}
	if form.Get("DurationSeconds") != "3600" {
		t.Errorf("DurationSeconds = %q, want the one hour default for a non-positive duration",
			form.Get("DurationSeconds"))
	}
}

// TestResolveAzureFailsClosedOnEveryTokenAndSecretFailure pins the refusals along the Key Vault
// path. The managed-identity route reaches an address the ordinary guard refuses, so it is the one
// place a resolver deliberately talks to the metadata service, and every way that conversation can
// go wrong has to end in a refusal rather than an empty bearer token or an empty secret.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
//
//nolint:funlen // Test function.
func TestResolveAzureFailsClosedOnEveryTokenAndSecretFailure(t *testing.T) {
	// mode selects how each mock misbehaves.
	var mode string
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "vault garbage":
			_, _ = w.Write([]byte(`<html>`))
		case "vault empty":
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"value":"azure-value"}`))
		}
	}))
	defer vaultSrv.Close()

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "auth refused":
			w.WriteHeader(http.StatusUnauthorized)
		case "auth no token":
			_, _ = w.Write([]byte(`{"token_type":"Bearer"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"sp-token"}`))
		}
	}))
	defer authSrv.Close()

	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "imds refused":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "imds no token":
			_, _ = w.Write([]byte(`{"expires_in":"3600"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"mi-token"}`))
		}
	}))
	defer imdsSrv.Close()

	origEP, origAuth, origIMDS := azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint
	azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = vaultSrv.URL, authSrv.URL, imdsSrv.URL
	defer func() {
		azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = origEP, origAuth, origIMDS
	}()

	sp := `{"vault":"kv","secret":"ci","tenant_id":"t","client_id":"c","client_secret":"s"}`
	mi := `{"vault":"kv","secret":"ci"}`
	tests := []struct {
		// Mode selects the failure the mocks produce.
		Mode string
		// Config is the source config.
		Config string
		// Why explains the case.
		Why string
	}{{ // Test 0: A Key Vault response that is not JSON is a refusal.
		Mode: "vault garbage", Config: `{"vault":"kv","secret":"ci","token":"t"}`,
		Why: "a non-JSON Key Vault response",
	}, { // Test 1: A Key Vault response with no value is a refusal, not an empty secret.
		Mode: "vault empty", Config: `{"vault":"kv","secret":"ci","token":"t"}`,
		Why: "a Key Vault response with no value",
	}, { // Test 2: An Entra ID grant that is refused is a refusal.
		Mode: "auth refused", Config: sp, Why: "a refused client-credentials grant",
	}, { // Test 3: A grant response with no access token is a refusal, not an empty bearer.
		Mode: "auth no token", Config: sp, Why: "a grant response with no access token",
	}, { // Test 4: A metadata service that refuses is a refusal.
		Mode: "imds refused", Config: mi, Why: "a refusing metadata service",
	}, { // Test 5: A metadata response with no access token is a refusal, not an empty bearer.
		Mode: "imds no token", Config: mi, Why: "a metadata response with no access token",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			mode = test.Mode
			value, err := resolveAzure(context.Background(), test.Config)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("%s = %q, %v; want ErrResolve", test.Why, value, err)
			}
			if value != "" {
				t.Errorf("%s returned the value %q alongside its error", test.Why, value)
			}
		})
	}
}

// TestAzureConfigCannotBuildAnUncheckableURL pins that the secret name and tenant id, both spliced
// into a URL without escaping, are still caught by the address check when they make the URL
// unparseable. Without the check the request would be built from a string nobody validated.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
func TestAzureConfigCannotBuildAnUncheckableURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"t","value":"v"}`))
	}))
	defer srv.Close()

	origEP, origAuth := azureEndpoint, azureAuthEndpoint
	azureEndpoint, azureAuthEndpoint = srv.URL, srv.URL
	defer func() { azureEndpoint, azureAuthEndpoint = origEP, origAuth }()

	tests := []struct {
		// Config is the source config.
		Config string
		// Why explains the case.
		Why string
	}{{ // Test 0: A secret name with a bad percent escape makes the secret URL unparseable.
		Config: `{"vault":"kv","secret":"ci%zz","token":"t"}`, Why: "a bad escape in the secret name",
	}, { // Test 1: A tenant id with a bad percent escape makes the token URL unparseable.
		Config: `{"vault":"kv","secret":"ci","tenant_id":"t%zz","client_id":"c","client_secret":"s"}`,
		Why:    "a bad escape in the tenant id",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if _, err := resolveAzure(context.Background(), test.Config); !errors.Is(err, ErrResolve) {
				t.Errorf("%s = %v, want ErrResolve", test.Why, err)
			}
		})
	}
}

// TestAzureIMDSDoesNotFollowARedirect pins the redirect refusal on the metadata client. That client
// exists to reach the link-local address the ordinary guard refuses, so it is the one client with
// no dial guard behind it, and following a redirect from a spoofed metadata response would send the
// managed-identity request wherever the response pointed.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
func TestAzureIMDSDoesNotFollowARedirect(t *testing.T) {
	var destReached bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destReached = true
		_, _ = w.Write([]byte(`{"access_token":"token-from-the-redirect-target"}`))
	}))
	defer dest.Close()

	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+"/token", http.StatusFound)
	}))
	defer imds.Close()

	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"value":"azure-value"}`))
	}))
	defer vaultSrv.Close()

	origEP, origIMDS := azureEndpoint, azureIMDSEndpoint
	azureEndpoint, azureIMDSEndpoint = vaultSrv.URL, imds.URL
	defer func() { azureEndpoint, azureIMDSEndpoint = origEP, origIMDS }()

	if _, err := resolveAzure(context.Background(),
		`{"vault":"kv","secret":"ci"}`); !errors.Is(err, ErrResolve) {
		t.Errorf("a redirecting metadata service = %v, want ErrResolve", err)
	}
	if destReached {
		t.Error("the managed-identity token request followed a redirect off the metadata service")
	}
}

// TestResolveGSMFailsClosedOnEveryTokenAndPayloadFailure pins the refusals along the Secret Manager
// path. Like Azure, it takes its token from a metadata service, and every failure of that exchange
// has to refuse rather than send an empty bearer token and read whatever comes back.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
func TestResolveGSMFailsClosedOnEveryTokenAndPayloadFailure(t *testing.T) {
	var mode string
	secretSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "garbage":
			_, _ = w.Write([]byte(`<html>`))
		case "bad base64":
			_, _ = w.Write([]byte(`{"payload":{"data":"not base64 !!"}}`))
		default:
			payload := base64.StdEncoding.EncodeToString([]byte("gsm-value"))
			_, _ = w.Write([]byte(`{"payload":{"data":"` + payload + `"}}`))
		}
	}))
	defer secretSrv.Close()

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "meta refused":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "meta no token":
			_, _ = w.Write([]byte(`{"expires_in":3600}`))
		case "meta garbage":
			_, _ = w.Write([]byte(`<html>`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"access-tok"}`))
		}
	}))
	defer metaSrv.Close()

	origEP, origMeta := gsmEndpoint, gsmMetadataEndpoint
	gsmEndpoint, gsmMetadataEndpoint = secretSrv.URL, metaSrv.URL
	defer func() { gsmEndpoint, gsmMetadataEndpoint = origEP, origMeta }()

	tests := []struct {
		// Mode selects the failure the mocks produce.
		Mode string
		// Config is the source config.
		Config string
		// Why explains the case.
		Why string
	}{{ // Test 0: A Secret Manager response that is not JSON is a refusal.
		Mode: "garbage", Config: `{"project":"p","secret":"s","token":"t"}`,
		Why: "a non-JSON Secret Manager response",
	}, { // Test 1: A payload that is not base64 is a refusal, not a mangled value.
		Mode: "bad base64", Config: `{"project":"p","secret":"s","token":"t"}`,
		Why: "a payload that is not base64",
	}, { // Test 2: A metadata server that refuses is a refusal.
		Mode: "meta refused", Config: `{"project":"p","secret":"s"}`,
		Why: "a refusing metadata server",
	}, { // Test 3: A metadata response with no access token is a refusal, not an empty bearer.
		Mode: "meta no token", Config: `{"project":"p","secret":"s"}`,
		Why: "a metadata response with no access token",
	}, { // Test 4: A metadata response that is not JSON is a refusal.
		Mode: "meta garbage", Config: `{"project":"p","secret":"s"}`,
		Why: "a non-JSON metadata response",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			mode = test.Mode
			value, err := resolveGSM(context.Background(), test.Config)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("%s = %q, %v; want ErrResolve", test.Why, value, err)
			}
			if value != "" {
				t.Errorf("%s returned the value %q alongside its error", test.Why, value)
			}
		})
	}
}

// TestConjurGuardsTheAuthnExchangeToo pins that the API-key exchange runs through the same address
// check as the read. The exchange happens first and posts the API key in the request body, so a
// config pointed at the metadata service would hand the key to whatever answers there before the
// read URL is ever checked.
func TestConjurGuardsTheAuthnExchangeToo(t *testing.T) {
	t.Parallel()
	refused := []string{
		"http://169.254.169.254",
		"http://metadata.google.internal",
		"file:///etc/passwd",
	}
	for testNum, base := range refused {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cfg := `{"url":"` + base + `","account":"prod","login":"app","api_key":"k",` +
				`"variable":"db/password"}`
			if _, err := resolveConjur(context.Background(), cfg); !errors.Is(err, ErrResolve) {
				t.Errorf("authn against %q = %v, want a refusal before the API key is posted", base, err)
			}
		})
	}
}

// TestConjurRefusesAnEmptySecretOrAnEmptyToken pins the two places a Conjur exchange can succeed at
// the HTTP level while producing nothing usable. Conjur answers with the raw value, so an empty
// body is indistinguishable from a secret of length zero unless the resolver refuses it.
func TestConjurRefusesAnEmptySecretOrAnEmptyToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Test 0: An empty variable body is a refusal, not an empty credential.
	cfg := `{"url":"` + srv.URL + `","account":"prod","variable":"db/password","token":"t"}`
	if v, err := resolveConjur(context.Background(), cfg); !errors.Is(err, ErrResolve) || v != "" {
		t.Errorf("empty secret body = %q, %v; want ErrResolve", v, err)
	}

	// Test 1: An authn exchange that returns no token is a refusal, not an empty Authorization header.
	cfg = `{"url":"` + srv.URL + `","account":"prod","login":"app","api_key":"k",` +
		`"variable":"db/password"}`
	if v, err := resolveConjur(context.Background(), cfg); !errors.Is(err, ErrResolve) || v != "" {
		t.Errorf("empty authn body = %q, %v; want ErrResolve", v, err)
	}
}

// TestOnePasswordRefusesAMalformedConnectResponse pins that a Connect server answering with
// something other than the documented shape is a refusal. The vault and item lookups drive which
// item is read, so a list the resolver cannot parse must stop the resolve rather than fall through
// to a default.
func TestOnePasswordRefusesAMalformedConnectResponse(t *testing.T) {
	t.Parallel()
	const vaultID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const itemID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"

	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/vaults" && mode == "bad list":
			_, _ = w.Write([]byte(`{"not":"a list"}`))
		case r.URL.Path == "/v1/vaults":
			_, _ = w.Write([]byte(`[{"id":"` + vaultID + `","name":"Prod"}]`))
		case r.URL.Path == "/v1/vaults/"+vaultID+"/items":
			_, _ = w.Write([]byte(`[{"id":"` + itemID + `","title":"DB"}]`))
		default:
			_, _ = w.Write([]byte(`{"fields": "not an array"}`))
		}
	}))
	defer srv.Close()

	// Test 0: A vault list that is not an array is a refusal.
	mode = "bad list"
	cfg := `{"url":"` + srv.URL + `","token":"t","vault":"Prod","item":"DB"}`
	v, err := resolveOnePassword(context.Background(), cfg)
	if !errors.Is(err, ErrResolve) || v != "" {
		t.Errorf("a malformed vault list = %q, %v; want ErrResolve", v, err)
	}

	// Test 1: An item whose fields are not the documented shape is a refusal.
	mode = "bad item"
	v, err = resolveOnePassword(context.Background(), cfg)
	if !errors.Is(err, ErrResolve) || v != "" {
		t.Errorf("a malformed item = %q, %v; want ErrResolve", v, err)
	}
}

// TestCheckResolveURLRefusesAnUnparseableAddress pins that an address url.Parse cannot read is a
// refusal rather than something handed to the HTTP client to interpret for itself.
func TestCheckResolveURLRefusesAnUnparseableAddress(t *testing.T) {
	t.Parallel()
	unparseable := []string{
		"https://vault.example.com/%zz",
		"https://vault.example.com/\x7f",
		"ht tp://vault.example.com",
		"https://[fe80::1",
	}
	for testNum, raw := range unparseable {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := checkResolveURL(raw); !errors.Is(err, ErrResolve) {
				t.Errorf("checkResolveURL(%q) = %v, want ErrResolve", raw, err)
			}
		})
	}
}

// TestTheMutuallyAuthenticatedClientRefusesRedirectsToo pins that the client built for CyberArk CCP
// keeps the redirect refusal the shared client has. A CCP request carries no bearer token, but it
// does present the client certificate, so following a redirect would offer that certificate to a
// host the server chose.
func TestTheMutuallyAuthenticatedClientRefusesRedirectsToo(t *testing.T) {
	t.Parallel()
	var destReached bool
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destReached = true
		_, _ = w.Write([]byte(`{"Content":"secret-from-the-redirect-target"}`))
	}))
	defer dest.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+r.URL.Path, http.StatusFound)
	}))
	defer src.Close()

	certPEM, keyPEM := generateClientCert(t, "redirect-test")
	cfg, err := json.Marshal(ccpConfig{
		URL: src.URL, AppID: "app1", Object: "db-prod",
		ClientCert: string(certPEM), ClientKey: string(keyPEM),
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	value, err := resolveCCP(context.Background(), string(cfg))
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("CCP across a redirect = %q, %v; want a refusal", value, err)
	}
	if destReached {
		t.Error("the mutually-authenticated CCP client followed a redirect, offering its client " +
			"certificate to a host the server chose")
	}
}

// TestEveryStageFailsClosedWhenItsStoreIsUnreachable pins the network-down refusal at each stage of
// the multi-request resolvers. Each of these stages has its own error message, and each is a point
// where an unreachable store must stop the resolve rather than continue with an empty token, an
// empty id, or a partially read item. A run that proceeded past one of them would authenticate with
// nothing and report the failure from somewhere far away.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
//
//nolint:funlen // Test function.
func TestEveryStageFailsClosedWhenItsStoreIsUnreachable(t *testing.T) {
	const vaultID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const itemID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"

	// An address nothing listens on, for the stage under test.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	// A Connect server whose lookups succeed, so the item read is the first stage that can fail. The
	// item read is redirected at the dead address by the id the list hands back.
	opSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vaults":
			_, _ = w.Write([]byte(`[{"id":"` + vaultID + `","name":"Prod"}]`))
		case "/v1/vaults/" + vaultID + "/items":
			_, _ = w.Write([]byte(`[{"id":"` + itemID + `","title":"DB"}]`))
		default:
			// Close the connection mid-response, as a store dropping out does.
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer opSrv.Close()

	origGSM, origGSMMeta := gsmEndpoint, gsmMetadataEndpoint
	origAzure, origIMDS := azureEndpoint, azureIMDSEndpoint
	gsmEndpoint, gsmMetadataEndpoint = deadURL, deadURL
	azureEndpoint, azureIMDSEndpoint = deadURL, deadURL
	defer func() {
		gsmEndpoint, gsmMetadataEndpoint = origGSM, origGSMMeta
		azureEndpoint, azureIMDSEndpoint = origAzure, origIMDS
	}()

	tests := []struct {
		// Kind is the source under test.
		Kind string
		// Config points the failing stage at an address nothing answers.
		Config string
		// Why names the stage.
		Why string
	}{{ // Test 0: The Conjur variable read, with the token already in hand.
		Kind:   KindConjur,
		Config: `{"url":"` + deadURL + `","account":"prod","variable":"db/password","token":"t"}`,
		Why:    "the Conjur variable read",
	}, { // Test 1: The Key Vault secret read, with the bearer token already in hand.
		Kind:   KindAzure,
		Config: `{"vault":"kv","secret":"ci","token":"t"}`,
		Why:    "the Key Vault secret read",
	}, { // Test 2: The Azure metadata token fetch, before any secret is read.
		Kind: KindAzure, Config: `{"vault":"kv","secret":"ci"}`,
		Why: "the Azure managed-identity token fetch",
	}, { // Test 3: The Secret Manager metadata token fetch.
		Kind: KindGSM, Config: `{"project":"p","secret":"s"}`,
		Why: "the Secret Manager metadata token fetch",
	}, { // Test 4: The Secret Manager secret read, with the token already in hand.
		Kind: KindGSM, Config: `{"project":"p","secret":"s","token":"t"}`,
		Why: "the Secret Manager secret read",
	}, { // Test 5: The 1Password item read, after both name lookups succeeded.
		Kind:   KindOnePassword,
		Config: `{"url":"` + opSrv.URL + `","token":"t","vault":"Prod","item":"DB"}`,
		Why:    "the 1Password item read",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			value, lease, err := ResolveLeased(context.Background(), test.Kind, test.Config)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("%s = %q, %v; want ErrResolve", test.Why, value, err)
			}
			if value != "" || lease != nil {
				t.Errorf("%s returned value %q and lease %v alongside its error", test.Why, value, lease)
			}
		})
	}
}

// TestResolveCCPFailsClosedWhenTheProviderIsUnreachable pins that a Central Credential Provider
// that cannot be reached is a refusal rather than a fall back to some other route to the account.
func TestResolveCCPFailsClosedWhenTheProviderIsUnreachable(t *testing.T) {
	t.Parallel()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := `{"url":"` + deadURL + `","app_id":"app1","object":"db-prod"}`
	value, err := resolveCCP(context.Background(), cfg)
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("unreachable CCP = %q, %v; want ErrResolve", value, err)
	}
	if value != "" {
		t.Errorf("unreachable CCP returned the value %q", value)
	}
}
