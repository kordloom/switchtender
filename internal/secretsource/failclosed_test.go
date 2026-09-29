package secretsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonKinds are the source kinds whose config is a JSON document, so one table can drive every
// malformed-config refusal.
var jsonKinds = []string{
	KindVault, KindGSM, KindAWS, KindAzure, KindConjur, KindCCP, KindOnePassword,
	KindVaultDynamic, KindAWSSTS,
}

// urlKinds are the source kinds whose endpoint comes from the config, so one table can drive every
// server-side request forgery refusal. Each entry builds a config for a given base address.
var urlKinds = map[string]func(base string) string{
	KindVault: func(base string) string {
		return `{"addr":"` + base + `","path":"secret/data/ci","field":"token","token":"t"}`
	},
	KindVaultDynamic: func(base string) string {
		return `{"addr":"` + base + `","path":"database/creds/app","field":"password","token":"t"}`
	},
	KindConjur: func(base string) string {
		return `{"url":"` + base + `","account":"prod","variable":"db/password","token":"t"}`
	},
	KindCCP: func(base string) string {
		return `{"url":"` + base + `","app_id":"app1","object":"db-prod"}`
	},
	KindOnePassword: func(base string) string {
		return `{"url":"` + base + `","token":"t","vault":"Prod","item":"DB"}`
	},
}

// TestEveryResolverRefusesAMalformedConfig proves the shared refusal at the front of every
// resolver. A source's config is decrypted from the store just before the run, so a config that is
// empty, truncated, or of the wrong JSON shape means the record is corrupt or was never finished.
// Returning an empty value there would hand the run a blank credential and let it proceed.
//
//nolint:funlen // Test function.
func TestEveryResolverRefusesAMalformedConfig(t *testing.T) {
	t.Parallel()
	configs := []struct {
		// Config is the stored source config.
		Config string
		// Why names the corruption, for the failure message.
		Why string
	}{{ // Test 0: An empty config, as an unfinished record holds.
		Config: "", Why: "an empty config",
	}, { // Test 1: Whitespace only.
		Config: "   \n\t ", Why: "a whitespace config",
	}, { // Test 2: A JSON null, which unmarshals cleanly into a zero config.
		Config: "null", Why: "a JSON null",
	}, { // Test 3: An empty object, so every required field is missing.
		Config: "{}", Why: "an empty object",
	}, { // Test 4: An array where an object is required.
		Config: "[]", Why: "a JSON array",
	}, { // Test 5: A bare JSON string.
		Config: `"just a string"`, Why: "a bare JSON string",
	}, { // Test 6: A number.
		Config: "12345", Why: "a JSON number",
	}, { // Test 7: Truncated JSON, as a partial write leaves.
		Config: `{"addr":"https://vault.example.com"`, Why: "truncated JSON",
	}, { // Test 8: Not JSON at all.
		Config: "addr=https://vault.example.com", Why: "a non-JSON config",
	}, { // Test 9: Valid JSON whose fields are all empty strings.
		Config: `{"addr":"","path":"","field":"","url":"","secret_id":"","vault":"","role_arn":"",` +
			`"account":"","variable":"","app_id":"","token":"","project":"","secret":"","item":""}`,
		Why: "a config whose every field is empty",
	}, { // Test 10: A deeply nested document the JSON decoder must bound rather than recurse on.
		Config: strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		Why:    "a deeply nested document",
	}, { // Test 11: A very long garbage config.
		Config: strings.Repeat("x", 1<<20), Why: "a very long non-JSON config",
	}}
	for _, kind := range jsonKinds {
		for testNum, c := range configs {
			t.Run(fmt.Sprintf("test %d %s", testNum, kind), func(t *testing.T) {
				t.Parallel()
				value, lease, err := ResolveLeased(context.Background(), kind, c.Config)
				if !errors.Is(err, ErrResolve) {
					t.Fatalf("%s given %s = %q, %v; want ErrResolve", kind, c.Why, value, err)
				}
				if value != "" {
					t.Errorf("%s given %s returned the value %q alongside its error", kind, c.Why, value)
				}
				if lease != nil {
					t.Errorf("%s given %s returned a lease, which cleanup would try to revoke", kind, c.Why)
				}
			})
		}
	}
}

// TestEveryURLResolverRefusesTheCloudMetadataEndpoint proves the server-side request forgery guard
// is wired into each resolver and not only into the helper.
//
// A source config is the one place an address travels from the database into an outbound request.
// The cloud metadata service answers an unauthenticated GET with the instance's own role
// credentials, so a source pointed at it turns the executor into a credential thief, and the
// fetched value is then stored as a credential and served back. Loopback and private addresses stay
// reachable because an internal secrets server, or a local agent, is an ordinary deployment.
func TestEveryURLResolverRefusesTheCloudMetadataEndpoint(t *testing.T) {
	t.Parallel()
	refused := []string{
		"http://169.254.169.254",
		"https://169.254.169.254:8200",
		"http://metadata.google.internal",
		"http://METADATA.GOOGLE.INTERNAL",
		"http://metadata",
		"http://0.0.0.0:8200",
		"http://[::]:8200",
		"http://[fe80::1]:8200",
		"http://[::ffff:169.254.169.254]:8200",
		"file:///etc/switchtender/master.key",
		"gopher://169.254.169.254:70",
		"ftp://vault.example.com",
		"//vault.example.com",
		"vault.example.com:8200",
		"https://",
	}
	for kind, build := range urlKinds {
		for testNum, base := range refused {
			t.Run(fmt.Sprintf("test %d %s", testNum, kind), func(t *testing.T) {
				t.Parallel()
				value, _, err := ResolveLeased(context.Background(), kind, build(base))
				if !errors.Is(err, ErrResolve) {
					t.Fatalf("%s pointed at %q = %q, %v; want a refusal before any request is made",
						kind, base, value, err)
				}
			})
		}
	}
}

// TestCheckResolveURLBoundaries pins the exact edge of the address rule. It is the one check
// standing between a stored config and an outbound request, so both halves matter: an address form
// it lets through by accident reaches the metadata service, and an ordinary internal address it
// refuses breaks a real deployment.
func TestCheckResolveURLBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// URL is the address the check is given.
		URL string
		// WantRefused is whether the address must be refused.
		WantRefused bool
		// Why explains the case.
		Why string
	}{{ // Test 0: The IPv4 metadata address is refused.
		URL: "http://169.254.169.254/latest/meta-data", WantRefused: true, Why: "the metadata address",
	}, { // Test 1: Any link-local IPv4 address is refused, not only the metadata one.
		URL: "http://169.254.1.2/x", WantRefused: true, Why: "a link-local address",
	}, { // Test 2: The last address outside link-local space is allowed.
		URL: "http://169.253.255.255/x", WantRefused: false, Why: "just below link-local space",
	}, { // Test 3: The first address above link-local space is allowed.
		URL: "http://169.255.0.0/x", WantRefused: false, Why: "just above link-local space",
	}, { // Test 4: An IPv4-mapped IPv6 form of the metadata address is refused.
		URL: "http://[::ffff:169.254.169.254]/x", WantRefused: true,
		Why: "the IPv4-mapped metadata address",
	}, { // Test 5: IPv6 link-local is refused.
		URL: "http://[fe80::1]/x", WantRefused: true, Why: "IPv6 link-local",
	}, { // Test 6: The IPv6 unspecified address is refused.
		URL: "http://[::]/x", WantRefused: true, Why: "the IPv6 unspecified address",
	}, { // Test 7: The IPv4 unspecified address is refused.
		URL: "http://0.0.0.0/x", WantRefused: true, Why: "the IPv4 unspecified address",
	}, { // Test 8: Loopback is allowed, since an admin-configured source may read a local agent.
		URL: "http://127.0.0.1:8200/v1/secret", WantRefused: false, Why: "IPv4 loopback",
	}, { // Test 9: IPv6 loopback is allowed for the same reason.
		URL: "http://[::1]:8200/v1/secret", WantRefused: false, Why: "IPv6 loopback",
	}, { // Test 10: Private space is allowed, since an internal secrets server is ordinary.
		URL: "https://10.0.0.5/v1/secret", WantRefused: false, Why: "private space",
	}, { // Test 11: The metadata hostname is refused whatever its case.
		URL: "https://Metadata.Google.Internal/x", WantRefused: true, Why: "the metadata hostname",
	}, { // Test 12: The short metadata hostname is refused.
		URL: "http://metadata/x", WantRefused: true, Why: "the short metadata hostname",
	}, { // Test 13: A scheme other than http or https is refused.
		URL: "file:///etc/passwd", WantRefused: true, Why: "a file URL",
	}, { // Test 14: An address with no host is refused.
		URL: "https:///v1/secret", WantRefused: true, Why: "an address with no host",
	}, { // Test 15: An empty address is refused.
		URL: "", WantRefused: true, Why: "an empty address",
	}, { // Test 16: A scheme-relative address has no scheme and is refused.
		URL: "//169.254.169.254/x", WantRefused: true, Why: "a scheme-relative address",
	}, { // Test 17: Userinfo does not move the host check off the real host.
		URL: "http://vault.example.com@169.254.169.254/x", WantRefused: true,
		Why: "userinfo before the host",
	}, { // Test 18: A public name with the metadata name only as a prefix is allowed.
		URL: "https://metadata.google.internal.example.com/x", WantRefused: false,
		Why: "a lookalike name",
	}, { // Test 19: An ordinary public secrets server is allowed.
		URL: "https://vault.example.com:8200/v1/secret/data/ci", WantRefused: false,
		Why: "a public server",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkResolveURL(test.URL)
			if test.WantRefused && !errors.Is(err, ErrResolve) {
				t.Errorf("checkResolveURL(%q) = %v, want a refusal: %s", test.URL, err, test.Why)
			}
			if !test.WantRefused && err != nil {
				t.Errorf("checkResolveURL(%q) = %v, want nil: %s", test.URL, err, test.Why)
			}
		})
	}
}

// TestTheSafeClientRefusesToDialTheMetadataService proves the second layer holds on its own. The
// name check reads the address as written, so a hostname that resolves to the metadata service,
// whether by a DNS record an attacker controls or by a rebind after the check, passes it. The
// dialer sees the resolved address and is the layer that must refuse.
func TestTheSafeClientRefusesToDialTheMetadataService(t *testing.T) {
	t.Parallel()
	refused := []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://0.0.0.0:8080/",
	}
	for testNum, target := range refused {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := safeClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("safeClient reached %q; the dial guard is the only defense against a "+
					"hostname that resolves to the metadata service", target)
			}
			if !errors.Is(err, ErrResolve) {
				t.Errorf("dial refusal = %v, want it to wrap ErrResolve", err)
			}

			// The mutually-authenticated client CyberArk CCP uses must keep the same guard.
			tlsClient := safeClientWithTLS(nil, nil)
			req2, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp2, err := tlsClient.Do(req2)
			if err == nil {
				_ = resp2.Body.Close()
				t.Fatalf("the TLS client reached %q, so the CCP path lost the dial guard", target)
			}
			if !errors.Is(err, ErrResolve) {
				t.Errorf("TLS client dial refusal = %v, want it to wrap ErrResolve", err)
			}
		})
	}
}

// TestARedirectNeverResendsTheCredentialToTheNewHost proves the redirect refusal on the client is
// what its comment claims. A secrets server that answers with a 302, whether compromised or merely
// misconfigured behind a proxy, would otherwise have Go replay the request to a host of its
// choosing, carrying the Vault token, the Connect bearer, or the Conjur access token in the
// headers. The client stops at the redirect instead, so the second host receives nothing.
func TestARedirectNeverResendsTheCredentialToTheNewHost(t *testing.T) {
	t.Parallel()
	const secretToken = "REDIRECT-TARGET-MUST-NOT-SEE-THIS"

	var gotHeaders []string
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Vault-Token", "Authorization"} {
			if v := r.Header.Get(h); v != "" {
				gotHeaders = append(gotHeaders, h+": "+v)
			}
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"token":"stolen"}}}`))
	}))
	defer dest.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+r.URL.Path, http.StatusFound)
	}))
	defer src.Close()

	tests := []struct {
		// Kind is the source under test.
		Kind string
		// Config points the source at the redirecting server.
		Config string
	}{{ // Test 0: A Vault read that is redirected must not resend X-Vault-Token.
		Kind: KindVault,
		Config: `{"addr":"` + src.URL + `","path":"secret/data/ci","field":"token","token":"` +
			secretToken + `"}`,
	}, { // Test 1: A Vault dynamic mint that is redirected must not resend X-Vault-Token.
		Kind: KindVaultDynamic,
		Config: `{"addr":"` + src.URL + `","path":"database/creds/app","field":"password","token":"` +
			secretToken + `"}`,
	}, { // Test 2: A 1Password Connect read that is redirected must not resend its bearer token.
		Kind:   KindOnePassword,
		Config: `{"url":"` + src.URL + `","token":"` + secretToken + `","vault":"Prod","item":"DB"}`,
	}, { // Test 3: A Conjur read that is redirected must not resend its access token.
		Kind: KindConjur,
		Config: `{"url":"` + src.URL + `","account":"prod","variable":"db/password","token":"` +
			secretToken + `"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			value, err := Resolve(context.Background(), test.Kind, test.Config)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("%s across a redirect = %q, %v; want a refusal", test.Kind, value, err)
			}
			if value != "" {
				t.Errorf("%s returned %q from the redirect target", test.Kind, value)
			}
			for _, h := range gotHeaders {
				if strings.Contains(h, secretToken) {
					t.Errorf("%s replayed its credential to the redirect target: %s", test.Kind, h)
				}
			}
			gotHeaders = nil
		})
	}
}

// TestAnOversizeResponseIsBoundedAndFailsClosed pins the response cap. A secret store that streams
// without end, because it was replaced or because a proxy in front of it is broken, would otherwise
// be read into memory until the server dies. The cap is one mebibyte, and a JSON body cut off at
// the cap no longer parses, so the resolver refuses rather than returning half a document.
func TestAnOversizeResponseIsBoundedAndFailsClosed(t *testing.T) {
	t.Parallel()
	// A JSON document far longer than the cap, valid only if read whole.
	oversize := `{"data":{"data":{"token":"` + strings.Repeat("A", 4*httpMaxBody) + `"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(oversize))
	}))
	defer srv.Close()

	cfg := `{"addr":"` + srv.URL + `","path":"secret/data/ci","field":"token","token":"t"}`
	value, err := resolveVault(context.Background(), cfg)
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("oversize response = %d bytes, %v; want ErrResolve", len(value), err)
	}
	if len(value) > httpMaxBody {
		t.Errorf("resolver returned %d bytes, more than the %d byte cap", len(value), httpMaxBody)
	}

	// A response just under the cap still resolves, so the bound did not break ordinary secrets.
	fits := strings.Repeat("B", httpMaxBody/2)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"data":{"token":"` + fits + `"}}}`))
	}))
	defer srv2.Close()
	cfg2 := `{"addr":"` + srv2.URL + `","path":"secret/data/ci","field":"token","token":"t"}`
	got, err := resolveVault(context.Background(), cfg2)
	if err != nil || got != fits {
		t.Errorf("a large but in-bounds secret resolved to %d bytes, %v; want %d bytes",
			len(got), err, len(fits))
	}
}

// TestConjurTruncatesASecretLargerThanTheResponseCap demonstrates a real bug. Conjur's response
// body is the secret itself rather than a document wrapping it, so the one mebibyte cap that makes
// the other resolvers fail closed on a short read makes this one succeed with a truncated value.
// The run then authenticates with the first mebibyte of a certificate or key and the failure
// surfaces somewhere far from here, with the audit trail recording a successful resolve.
func TestConjurTruncatesASecretLargerThanTheResponseCap(t *testing.T) {
	t.Parallel()

	secret := strings.Repeat("K", httpMaxBody+512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(secret))
	}))
	defer srv.Close()

	cfg := `{"url":"` + srv.URL + `","account":"prod","variable":"db/key","token":"t"}`
	got, err := resolveConjur(context.Background(), cfg)
	if err == nil && got != secret {
		t.Errorf("conjur returned %d bytes of a %d byte secret with no error; a truncated "+
			"credential must be a refusal, not a success", len(got), len(secret))
	}
}

// TestNoResolverReturnsAnEmptySecretWithoutAnError demonstrates a real bug. Most resolvers already
// refuse an empty value: Secrets Manager says the secret has no value, Key Vault and CCP say the
// same, and Conjur refuses an empty body. Vault, Vault dynamic, Secret Manager, and the command
// source do not, so a field that exists but is blank, a Secret Manager response with no payload, or
// a fetch command that prints nothing and exits zero all resolve to an empty credential and the run
// proceeds with it. An empty password or token is the failure mode a credential exists to prevent,
// and the audit trail records the resolve as a success.
// It does not run in parallel: it points the package-level gsmEndpoint at its own mock, and
// TestResolveGSM swaps the same variable, so running the two at once makes each read the other's
// server.
func TestNoResolverReturnsAnEmptySecretWithoutAnError(t *testing.T) {
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "creds"):
			_, _ = w.Write([]byte(`{"lease_id":"l","data":{"password":""}}`))
		case strings.Contains(r.URL.Path, "null"):
			// A field explicitly set to null unmarshals cleanly into an empty string.
			_, _ = w.Write([]byte(`{"data":{"data":{"token":null}}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"data":{"token":""}}}`))
		}
	}))
	defer vaultSrv.Close()

	gsmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A Secret Manager reply with no payload at all, as a partial or unexpected response gives.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer gsmSrv.Close()
	origGSM := gsmEndpoint
	gsmEndpoint = gsmSrv.URL
	defer func() { gsmEndpoint = origGSM }()

	tests := []struct {
		// Kind is the source under test.
		Kind string
		// Config is a config whose store answers with nothing.
		Config string
	}{{ // Test 0: A Vault KV field that exists but is blank.
		Kind:   KindVault,
		Config: `{"addr":"` + vaultSrv.URL + `","path":"secret/data/ci","field":"token","token":"t"}`,
	}, { // Test 1: A Vault dynamic field that is minted blank.
		Kind: KindVaultDynamic,
		Config: `{"addr":"` + vaultSrv.URL +
			`","path":"database/creds/app","field":"password","token":"t"}`,
	}, { // Test 2: A Secret Manager response carrying no payload.
		Kind:   KindGSM,
		Config: `{"project":"proj","secret":"ci","token":"t"}`,
	}, { // Test 3: A fetch command that prints nothing and exits zero.
		Kind: KindCommand, Config: "true",
	}, { // Test 4: A Vault KV field whose value is an explicit JSON null.
		Kind:   KindVault,
		Config: `{"addr":"` + vaultSrv.URL + `","path":"secret/data/null","field":"token","token":"t"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			value, err := Resolve(context.Background(), test.Kind, test.Config)
			if err == nil && value == "" {
				t.Errorf("%s resolved to an empty secret with no error; the run proceeds with a "+
					"blank credential and the audit trail records a successful resolve", test.Kind)
			}
		})
	}
}

// TestCCPClientPicksTheTransportItsConfigAsksFor checks the wiring between the CCP config and the
// client it builds. A config with no certificate material must reuse the shared safe client, and
// any certificate material must produce a client that still carries the dial guard, since a CCP
// client built from a bare http.Client would reach the metadata service. Invalid material must be a
// refusal rather than a silent fall back to an unauthenticated client, which CCP would then answer
// by its allowed-machine rule alone.
func TestCCPClientPicksTheTransportItsConfigAsksFor(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := generateClientCert(t, "wiring-test")
	caPEM := certPEM

	// Test 0: Neither a certificate nor a CA reuses the shared safe client.
	client, err := ccpClient(ccpConfig{URL: "https://ccp.example.com", AppID: "a", Object: "o"})
	if err != nil {
		t.Fatalf("plain config: %v", err)
	}
	if client != safeClient {
		t.Error("a CCP config with no certificate material built a new client instead of reusing " +
			"the shared safe client")
	}

	// Test 1: A client certificate builds a separate client that still refuses redirects.
	client, err = ccpClient(ccpConfig{ClientCert: string(certPEM), ClientKey: string(keyPEM)})
	if err != nil {
		t.Fatalf("client certificate config: %v", err)
	}
	if client == safeClient {
		t.Error("a CCP config with a client certificate reused the shared client, so the " +
			"certificate would never be presented")
	}
	if client.CheckRedirect == nil {
		t.Error("the mutually-authenticated CCP client follows redirects, so it would replay the " +
			"request to a host the server chose")
	}
	if client.Timeout == 0 {
		t.Error("the mutually-authenticated CCP client has no timeout, so a hung CCP holds the run")
	}

	// Test 2: A private CA alone also builds a separate client.
	client, err = ccpClient(ccpConfig{CACert: string(caPEM)})
	if err != nil {
		t.Fatalf("ca config: %v", err)
	}
	if client == safeClient {
		t.Error("a CCP config with a private CA reused the shared client, so the CA would be ignored")
	}

	// Test 3: Broken certificate material is a refusal, never a fall back to no client certificate.
	broken := []ccpConfig{
		{ClientCert: "not a pem", ClientKey: string(keyPEM)},
		{ClientCert: string(certPEM), ClientKey: "not a pem"},
		{ClientCert: string(certPEM)},
		{ClientKey: string(keyPEM)},
		{CACert: "not a pem"},
		{CACert: "-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----"},
		{ClientCert: string(certPEM), ClientKey: string(certPEM)},
	}
	for testNum, cfg := range broken {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := ccpClient(cfg)
			if !errors.Is(err, ErrResolve) {
				t.Errorf("broken certificate material = %v, want ErrResolve", err)
			}
			if got != nil {
				t.Error("a refused CCP client config still returned a client, which would send the " +
					"request unauthenticated")
			}
		})
	}
}

// TestAzurePrefersAConfigTokenOverEveryOtherAuthPath pins the documented order in azureToken. A
// token in the config is used directly and no grant runs, so a config carrying both a token and a
// partial service principal is not the misconfiguration error, and never reaches the metadata
// service.
func TestAzurePrefersAConfigTokenOverEveryOtherAuthPath(t *testing.T) {
	var gotAuth string
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"value":"azure-value"}`))
	}))
	defer vaultSrv.Close()

	// Any grant or metadata call would reach this server, which refuses, so a token that was not used
	// directly shows up as a failure rather than as a silent second choice.
	denySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer denySrv.Close()

	origEP, origAuth, origIMDS := azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint
	azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = vaultSrv.URL, denySrv.URL, denySrv.URL
	defer func() {
		azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = origEP, origAuth, origIMDS
	}()

	cfg := `{"vault":"kv","secret":"ci","token":"direct-token","tenant_id":"t"}`
	got, err := resolveAzure(context.Background(), cfg)
	if err != nil || got != "azure-value" {
		t.Fatalf("config token with a partial service principal = %q, %v; want azure-value", got, err)
	}
	if gotAuth != "Bearer direct-token" {
		t.Errorf("auth = %q, want Bearer direct-token; the config token must be used directly", gotAuth)
	}
}

// TestGSMConfigCannotMoveTheRequestOffTheSecretManagerHost pins that the Secret Manager resolver,
// the one resolver that does not run its URL through the address check, still cannot be steered off
// its endpoint. Its project, secret, and version are interpolated into the path without escaping,
// so a name carrying a slash, an at sign, or a hash must stay in the path rather than becoming a
// new authority.
func TestGSMConfigCannotMoveTheRequestOffTheSecretManagerHost(t *testing.T) {
	var gotHost string
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		gotHost = r.Host
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	origEP, origMeta := gsmEndpoint, gsmMetadataEndpoint
	gsmEndpoint, gsmMetadataEndpoint = srv.URL, srv.URL
	defer func() { gsmEndpoint, gsmMetadataEndpoint = origEP, origMeta }()

	wantHost := strings.TrimPrefix(srv.URL, "http://")
	projects := []string{
		"../../..", "proj@evil.example.com", "proj#@evil.example.com",
		"proj/../../v1", "proj?x=1", "proj%2f..%2f",
	}
	for testNum, project := range projects {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			reached, gotHost = false, ""
			cfg := `{"project":"` + project + `","secret":"ci","token":"t"}`
			if _, err := resolveGSM(context.Background(), cfg); !errors.Is(err, ErrResolve) {
				t.Errorf("project %q resolved, %v; want a refusal", project, err)
			}
			if reached && gotHost != wantHost {
				t.Errorf("project %q sent the request to %q, want %q; a config value moved the "+
					"request off the Secret Manager endpoint", project, gotHost, wantHost)
			}
		})
	}

	// A project name carrying a control character cannot even build a request, which is a refusal.
	newlineCfg := "{\"project\":\"proj\\ninjected\",\"secret\":\"ci\",\"token\":\"t\"}"
	if _, err := resolveGSM(context.Background(), newlineCfg); !errors.Is(err, ErrResolve) {
		t.Errorf("a project with a newline = %v, want ErrResolve", err)
	}
}
