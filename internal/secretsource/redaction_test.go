package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// canary is a value no error, log line, or stored record may ever contain. Every config in this
// file carries it in each field that holds a credential, so a single substring search proves the
// whole error chain stayed clean.
const canary = "CANARY-8f3a91d4-DO-NOT-LEAK"

// canaryPEM is the canary wrapped in something that looks like a private key, for the resolvers
// whose config carries PEM material rather than a token.
const canaryPEM = "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----\n"

// leakConfigs returns one config per source kind, each carrying the canary in every credential
// field the kind supports and pointed at base for the kinds whose endpoint lives in the config. The
// endpoints of the cloud kinds are package vars the caller redirects instead.
func leakConfigs(base string) map[string]string {
	must := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(b)
	}
	return map[string]string{
		KindVault: must(vaultConfig{Addr: base, Path: "secret/data/ci", Field: "token", Token: canary}),
		KindVaultDynamic: must(vaultDynamicConfig{
			Addr: base, Path: "database/creds/app", Field: "password", Token: canary,
		}),
		KindGSM: must(gsmConfig{Project: "proj", Secret: "ci", Token: canary}),
		KindAWS: must(awsConfig{
			SecretID: "db/password", Region: "us-east-1",
			AccessKeyID: canary, SecretAccessKey: canary, SessionToken: canary,
		}),
		KindAWSSTS: must(awsSTSConfig{
			RoleARN: "arn:aws:iam::123456789012:role/deploy", Region: "us-east-1",
			AccessKeyID: canary, SecretAccessKey: canary, SessionToken: canary,
		}),
		KindAzure: must(azureConfig{
			Vault: "kv", Secret: "ci", TenantID: "tenant", ClientID: "client", ClientSecret: canary,
		}),
		KindConjur: must(conjurConfig{
			URL: base, Account: "prod", Login: "app", APIKey: canary, Variable: "db/password",
		}),
		KindCCP: must(ccpConfig{
			URL: base, AppID: "app1", Object: "db-prod", ClientCert: canaryPEM, ClientKey: canaryPEM,
		}),
		KindOnePassword: must(onePasswordConfig{
			URL: base, Token: canary, Vault: "Prod", Item: "DB",
		}),
	}
}

// redirectCloudEndpoints points every package-level cloud endpoint at base for the duration of the
// test, restoring them afterwards, so the cloud resolvers reach a mock instead of the real
// internet.
func redirectCloudEndpoints(t *testing.T, base string) {
	t.Helper()
	origAWS, origSTS := awsEndpoint, awsSTSEndpoint
	origGSM, origGSMMeta := gsmEndpoint, gsmMetadataEndpoint
	origAzure, origAzureAuth, origIMDS := azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint
	awsEndpoint, awsSTSEndpoint = base+"/", base+"/"
	gsmEndpoint, gsmMetadataEndpoint = base, base
	azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = base, base, base
	t.Cleanup(func() {
		awsEndpoint, awsSTSEndpoint = origAWS, origSTS
		gsmEndpoint, gsmMetadataEndpoint = origGSM, origGSMMeta
		azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = origAzure, origAzureAuth, origIMDS
	})
}

// TestAFailedResolveNeverPutsTheConfigSecretInItsError is the central redaction proof for the
// package.
//
// A resolve error is wrapped with the credential id and stored on the run, which is served over the
// API and written to the audit trail. The masker registers a credential's value only after the
// value exists, so when resolution itself fails there is nothing registered and nothing would be
// redacted. Anything a resolver copies from its config into an error is therefore stored in the
// clear forever. Every kind's config carries the canary in each credential field it has, and every
// failure stage a resolver can reach is driven, so a future resolver that adds "%v" of its config
// to an error fails here rather than in production.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
//
//nolint:funlen // Test function.
func TestAFailedResolveNeverPutsTheConfigSecretInItsError(t *testing.T) {
	// Isolate from any ambient AWS environment, so a host key never stands in for the config's.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	// mode selects how the mock fails, so one server drives every stage a resolver can fail at.
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "refused":
			w.WriteHeader(http.StatusForbidden)
			// A store often echoes the credential it refused. It must not survive into our error.
			_, _ = w.Write([]byte(`{"errors":["bad token ` + canary + `"]}`))
		case "garbage":
			_, _ = w.Write([]byte("not json at all"))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	// A second address nothing listens on, to drive the transport-error path.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	// The refusal stage: every kind gets a 403 whose body quotes the credential it refused, and every
	// kind must fail on it.
	mode = "refused"
	redirectCloudEndpoints(t, srv.URL)
	for kind, config := range leakConfigs(srv.URL) {
		t.Run(fmt.Sprintf("test refused %s", kind), func(t *testing.T) {
			value, lease, err := ResolveLeased(context.Background(), kind, config)
			if err == nil {
				t.Fatalf("%s resolved to %q against a refusing store, want a refusal", kind, value)
			}
			if !errors.Is(err, ErrResolve) {
				t.Errorf("error = %v, want it to wrap ErrResolve so callers can classify it", err)
			}
			if value != "" || lease != nil {
				t.Errorf("failed resolve returned value %q and lease %v, want neither", value, lease)
			}
			assertNoCanary(t, kind, err)
		})
	}

	// The malformed-response stages. Conjur takes the body as the value, so a 200 is a success there
	// whatever the body says; the rest fail. Either way nothing from the config may reach an error.
	for _, stage := range []string{"garbage", "empty"} {
		mode = stage
		for kind, config := range leakConfigs(srv.URL) {
			t.Run(fmt.Sprintf("test %s %s", stage, kind), func(t *testing.T) {
				_, _, err := ResolveLeased(context.Background(), kind, config)
				if err != nil {
					assertNoCanary(t, kind, err)
				}
			})
		}
	}

	// The transport-error path: nothing is listening, so every resolver reports a dial failure.
	redirectCloudEndpoints(t, deadURL)
	for kind, config := range leakConfigs(deadURL) {
		t.Run(fmt.Sprintf("test unreachable %s", kind), func(t *testing.T) {
			_, _, err := ResolveLeased(context.Background(), kind, config)
			if err == nil {
				t.Fatalf("%s resolved against an unreachable store, want a refusal", kind)
			}
			assertNoCanary(t, kind, err)
		})
	}
}

// assertNoCanary fails when the canary appears anywhere in an error, under any of the formatting
// verbs a caller or a logger might use to render it.
func assertNoCanary(t *testing.T, kind string, err error) {
	t.Helper()
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
		if strings.Contains(rendered, canary) {
			t.Errorf("the %s resolver put a config credential in its error, which is stored on the "+
				"run and served unredacted:\n%s", kind, rendered)
		}
	}
}

// TestAFailedResolveNeverPutsTheStoreResponseInItsError pins that a secret manager's response body
// stays out of the error. Vault and Conjur both answer a refused read with a JSON document that can
// quote the token that was refused, and Secrets Manager echoes request fields, so copying the body
// into the error would store the caller's own credential on the run.
//
// It redirects package-level endpoint vars, so it does not run in parallel.
func TestAFailedResolveNeverPutsTheStoreResponseInItsError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	// The body a hostile or merely chatty secret store returns on a failure.
	const bodySecret = "RESPONSE-BODY-SECRET-4c1d"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":["denied for ` + bodySecret + `"],` +
			`"value":"` + bodySecret + `"}`))
	}))
	defer srv.Close()
	redirectCloudEndpoints(t, srv.URL)

	for kind, config := range leakConfigs(srv.URL) {
		t.Run(fmt.Sprintf("test %s", kind), func(t *testing.T) {
			_, _, err := ResolveLeased(context.Background(), kind, config)
			if err == nil {
				t.Fatalf("%s resolved against a failing store, want a refusal", kind)
			}
			if strings.Contains(err.Error(), bodySecret) {
				t.Errorf("the %s resolver copied the store's response body into its error, which is "+
					"stored on the run:\n%v", kind, err)
			}
		})
	}
}

// TestAFailedCommandNeverPutsItsOutputInTheError extends the existing stderr proof to stdout. A
// secret-fetch command prints the value it fetched on stdout, so a resolver that reported "got %q,
// exit 1" would store the fetched secret on the run in the clear.
func TestAFailedCommandNeverPutsItsOutputInTheError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Command is the shell the source runs.
		Command string
		// Why names the stream the secret went out on, for the failure message.
		Why string
	}{{ // Test 0: The value reached stdout before the command failed.
		Command: "printf '" + canary + "'; exit 1", Why: "stdout",
	}, { // Test 1: The value reached stderr before the command failed.
		Command: "printf '" + canary + "' >&2; exit 1", Why: "stderr",
	}, { // Test 2: The value reached both streams.
		Command: "printf '" + canary + "'; printf '" + canary + "' >&2; exit 9", Why: "both streams",
	}, { // Test 3: The command itself names the secret and cannot be run.
		Command: "/nonexistent/fetch --token=" + canary, Why: "the command line",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			value, err := resolveCommand(context.Background(), test.Command)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("error = %v, want ErrResolve", err)
			}
			if value != "" {
				t.Errorf("value = %q, want empty; a failed fetch must not return partial output", value)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the command resolver put %s into its error, which is stored on the run:\n%v",
					test.Why, err)
			}
		})
	}
}

// TestAResolvedValueIsNeverEchoedBackInALaterError pins that a value which resolved successfully
// does not reappear in an error raised further along. The 1Password resolver reads a list, then an
// item, then a field, and the Conjur resolver exchanges an API key for a token before the read, so
// each has a secret in hand when the next request fails.
func TestAResolvedValueIsNeverEchoedBackInALaterError(t *testing.T) {
	t.Parallel()
	const vaultID = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	const itemID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	const issued = "ISSUED-TOKEN-7b2e"

	opSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vaults":
			_, _ = w.Write([]byte(`[{"id":"` + vaultID + `","name":"Prod"}]`))
		case "/v1/vaults/" + vaultID + "/items":
			_, _ = w.Write([]byte(`[{"id":"` + itemID + `","title":"DB"}]`))
		default:
			// The item read fails after the ids resolved, with the issued token in the body.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"denied ` + issued + `"}`))
		}
	}))
	defer opSrv.Close()

	conjurSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// The authn exchange succeeds, so the resolver holds a live access token.
			_, _ = w.Write([]byte(issued))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer conjurSrv.Close()

	tests := []struct {
		// Kind is the source under test.
		Kind string
		// Config points the source at its mock.
		Config string
	}{{ // Test 0: 1Password fails on the item read after resolving both ids.
		Kind: KindOnePassword,
		Config: `{"url":"` + opSrv.URL + `","token":"` + canary +
			`","vault":"Prod","item":"DB"}`,
	}, { // Test 1: Conjur fails on the variable read after exchanging its API key for a token.
		Kind: KindConjur,
		Config: `{"url":"` + conjurSrv.URL + `","account":"prod","login":"app","api_key":"` +
			canary + `","variable":"db/password"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(context.Background(), test.Kind, test.Config)
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("error = %v, want ErrResolve", err)
			}
			for _, secret := range []string{canary, issued} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s leaked a secret it already held into a later error:\n%v", test.Kind, err)
				}
			}
		})
	}
}

// TestAnUnknownKindErrorNamesTheKindNotTheConfig pins that the one error built from caller-supplied
// text quotes the kind and not the config. The config of an unknown kind is whatever the operator
// stored, which for a mistyped source kind is the secret itself.
func TestAnUnknownKindErrorNamesTheKindNotTheConfig(t *testing.T) {
	t.Parallel()
	_, _, err := ResolveLeased(context.Background(), "vaultt", canary)
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("error = %v, want ErrResolve", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("the unknown-kind error carries the source config, which is the secret:\n%v", err)
	}
	if !strings.Contains(err.Error(), "vaultt") {
		t.Errorf("the unknown-kind error does not name the kind, so the operator cannot fix it:\n%v", err)
	}
}

// TestCheckResolveURLLeaksUserinfoFromAnUnparseableAddress demonstrates a real leak. url.Parse puts
// the whole raw URL in its error, and checkResolveURL renders that error with %v, so an address
// carrying HTTP basic credentials, which is how an operator points a source at a store behind a
// simple proxy, ends up in the run's stored error in the clear. The transport path is safe because
// net/http strips the password itself; only this parse path does not.
func TestCheckResolveURLLeaksUserinfoFromAnUnparseableAddress(t *testing.T) {
	t.Parallel()

	const password = "hunter2-in-the-url"
	err := checkResolveURL("https://user:" + password + "@vault.example.com/%zz")
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("error = %v, want ErrResolve", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("the URL check put the address password in its error:\n%v", err)
	}

	// The same leak reaches a caller through a resolver, where the error is stored on the run.
	cfg := `{"addr":"https://user:` + password + `@vault.example.com/%zz",` +
		`"path":"secret/data/ci","field":"token","token":"t"}`
	_, rErr := resolveVault(context.Background(), cfg)
	if rErr == nil || strings.Contains(rErr.Error(), password) {
		t.Errorf("resolveVault stored the address password in its error:\n%v", rErr)
	}
}
