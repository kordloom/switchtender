package secretsource

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestVaultFieldReadsBothKVLayouts pins the field extraction that decides which value becomes the
// credential. Vault's two KV versions put the secret at different depths, and the resolver has to
// read whichever one the operator's mount uses without ever reaching into a neighboring field. A
// wrong pick here does not fail: it silently returns a different secret than the source names.
//
//nolint:funlen // Test function.
func TestVaultFieldReadsBothKVLayouts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the Vault read response.
		Body string
		// Field is the field the source names.
		Field string
		// WantValue is the extracted value.
		WantValue string
		// Want is the expected error.
		Want error
		// Why explains the case.
		Why string
	}{{ // Test 0: KV v2 nests the secret under data.data.
		Body: `{"data":{"data":{"token":"kv2"}}}`, Field: "token", WantValue: "kv2", Why: "KV v2",
	}, { // Test 1: KV v1 puts the secret directly under data.
		Body: `{"data":{"token":"kv1"}}`, Field: "token", WantValue: "kv1", Why: "KV v1",
	}, { // Test 2: When both layouts could match, the nested KV v2 value wins, as documented.
		Body: `{"data":{"token":"outer","data":{"token":"inner"}}}`, Field: "token",
		WantValue: "inner", Why: "the KV v2 layout takes precedence",
	}, { // Test 3: A KV v1 secret with a field literally named data still resolves its other fields.
		Body: `{"data":{"data":"a-string-not-an-object","token":"kv1"}}`, Field: "token",
		WantValue: "kv1", Why: "a KV v1 field named data",
	}, { // Test 4: A KV v1 field named data is returned when it is the field asked for.
		Body: `{"data":{"data":"the-secret"}}`, Field: "data", WantValue: "the-secret",
		Why: "asking for the field named data",
	}, { // Test 5: A field the secret does not have is a refusal, never an empty value.
		Body: `{"data":{"data":{"token":"kv2"}}}`, Field: "missing", Want: errors.New("not found"),
		Why: "a field that is not there",
	}, { // Test 6: Field names are matched exactly, so case matters.
		Body: `{"data":{"data":{"token":"kv2"}}}`, Field: "Token", Want: errors.New("not found"),
		Why: "a field name in the wrong case",
	}, { // Test 7: A numeric field resolves to its JSON text, so a numeric secret is still usable.
		Body: `{"data":{"data":{"port":5432}}}`, Field: "port", WantValue: "5432",
		Why: "a numeric field",
	}, { // Test 8: A boolean field resolves to its JSON text.
		Body: `{"data":{"data":{"enabled":true}}}`, Field: "enabled", WantValue: "true",
		Why: "a boolean field",
	}, { // Test 9: A JSON null unmarshals cleanly into a string, so the field resolves to an empty
		// secret rather than to the literal null. See TestNoResolverReturnsAnEmptySecretWithoutAnError.
		Body: `{"data":{"data":{"token":null}}}`, Field: "token", WantValue: "",
		Why: "a null field",
	}, { // Test 10: An object field resolves to its JSON text.
		Body: `{"data":{"data":{"creds":{"user":"u"}}}}`, Field: "creds", WantValue: `{"user":"u"}`,
		Why: "an object field",
	}, { // Test 11: A unicode field name matches and a unicode value survives.
		Body: `{"data":{"data":{"密码":"pässwörd"}}}`, Field: "密码", WantValue: "pässwörd",
		Why: "unicode names and values",
	}, { // Test 12: An escaped string is unescaped, so a secret with quotes and newlines round-trips.
		Body: `{"data":{"data":{"key":"line1\nline2\t\"q\""}}}`, Field: "key",
		WantValue: "line1\nline2\t\"q\"", Why: "an escaped string value",
	}, { // Test 13: A response that is not JSON is a refusal.
		Body: `not json`, Field: "token", Want: errors.New("not valid JSON"), Why: "a non-JSON body",
	}, { // Test 14: An empty body is a refusal.
		Body: ``, Field: "token", Want: errors.New("not valid JSON"), Why: "an empty body",
	}, { // Test 15: A response with no data object is a refusal.
		Body: `{}`, Field: "token", Want: errors.New("not found"), Why: "a response with no data",
	}, { // Test 16: A null data object is a refusal.
		Body: `{"data":null}`, Field: "token", Want: errors.New("not found"), Why: "a null data object",
	}, { // Test 17: An empty field name matches nothing rather than the whole secret.
		Body: `{"data":{"data":{"token":"kv2"}}}`, Field: "", Want: errors.New("not found"),
		Why: "an empty field name",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := vaultField([]byte(test.Body), test.Field)
			if test.Want != nil {
				if err == nil {
					t.Fatalf("%s = %q, want a refusal", test.Why, got)
				}
				if !strings.Contains(err.Error(), test.Want.Error()) {
					t.Errorf("%s error = %v, want it to mention %q", test.Why, err, test.Want)
				}
				if got != "" {
					t.Errorf("%s returned the value %q alongside its error", test.Why, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", test.Why, err)
			}
			if diff := cmp.Diff(test.WantValue, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", test.Why, diff)
			}
		})
	}
}

// TestVaultDynamicSecretPairsTheValueWithItsLease pins that a minted credential always comes back
// with the lease id that revokes it. Losing the lease id does not fail the run, it leaves a live
// database or cloud credential in the engine until its own TTL expires, which is exactly the
// exposure the dynamic source exists to remove.
func TestVaultDynamicSecretPairsTheValueWithItsLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the Vault dynamic read response.
		Body string
		// Field is the field the source names.
		Field string
		// WantValue is the minted value.
		WantValue string
		// WantLease is the lease id that revokes it.
		WantLease string
		// Want is the expected error.
		Want error
		// Why explains the case.
		Why string
	}{{ // Test 0: The field and the lease id come back together.
		Body: `{"lease_id":"database/creds/app/abc","data":{"password":"pw"}}`, Field: "password",
		WantValue: "pw", WantLease: "database/creds/app/abc", Why: "a normal mint",
	}, { // Test 1: The credential sits directly under data, never nested as KV v2 nests it.
		Body:  `{"lease_id":"l","data":{"data":{"password":"nested"},"password":"flat"}}`,
		Field: "password", WantValue: "flat", WantLease: "l", Why: "a flat dynamic payload",
	}, { // Test 2: A response with no lease id still yields the value, with nothing to revoke.
		Body: `{"data":{"password":"pw"}}`, Field: "password", WantValue: "pw", WantLease: "",
		Why: "a mint with no lease",
	}, { // Test 3: A field the mint did not produce is a refusal.
		Body: `{"lease_id":"l","data":{"username":"u"}}`, Field: "password",
		Want: errors.New("not found"), Why: "a missing field",
	}, { // Test 4: A response that is not JSON is a refusal.
		Body: `<html>`, Field: "password", Want: errors.New("not valid JSON"), Why: "a non-JSON body",
	}, { // Test 5: A response with no data at all is a refusal.
		Body: `{"lease_id":"l"}`, Field: "password", Want: errors.New("not found"),
		Why: "a response with no data",
	}, { // Test 6: A non-string field resolves to its JSON text.
		Body: `{"lease_id":"l","data":{"ttl":3600}}`, Field: "ttl", WantValue: "3600", WantLease: "l",
		Why: "a numeric field",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			value, lease, err := vaultDynamicSecret([]byte(test.Body), test.Field)
			if test.Want != nil {
				if err == nil {
					t.Fatalf("%s = %q, want a refusal", test.Why, value)
				}
				if !strings.Contains(err.Error(), test.Want.Error()) {
					t.Errorf("%s error = %v, want it to mention %q", test.Why, err, test.Want)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", test.Why, err)
			}
			if diff := cmp.Diff(test.WantValue, value); diff != "" {
				t.Errorf("%s value mismatch (-want +got):\n%s", test.Why, diff)
			}
			if diff := cmp.Diff(test.WantLease, lease); diff != "" {
				t.Errorf("%s lease mismatch (-want +got):\n%s", test.Why, diff)
			}
		})
	}
}

// TestSameVaultEndpointDecidesWhereTheServersTokenMayGo pins the comparison that gates the
// VAULT_TOKEN fallback. The server's own Vault token is the most valuable credential the process
// holds, and this function alone decides whether a config-supplied address is close enough to the
// pinned VAULT_ADDR to receive it. It has to be strict, since anything it accepts by accident is an
// address an operator chose that now gets the token, and it has to accept the ordinary spelling
// differences or the fallback never works.
//
//nolint:funlen // Test function.
func TestSameVaultEndpointDecidesWhereTheServersTokenMayGo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Addr is the address from the source config.
		Addr string
		// EnvAddr is the server's pinned VAULT_ADDR.
		EnvAddr string
		// WantSame is whether the token may be sent.
		WantSame bool
		// Why explains the case.
		Why string
	}{{ // Test 0: The same address matches.
		Addr: "https://vault.example.com:8200", EnvAddr: "https://vault.example.com:8200",
		WantSame: true, Why: "identical addresses",
	}, { // Test 1: A trailing slash is not a different endpoint.
		Addr: "https://vault.example.com:8200/", EnvAddr: "https://vault.example.com:8200",
		WantSame: true, Why: "a trailing slash",
	}, { // Test 2: A different path is still the same endpoint, since only the authority decides.
		Addr: "https://vault.example.com:8200/v1/x", EnvAddr: "https://vault.example.com:8200",
		WantSame: true, Why: "a differing path",
	}, { // Test 3: Host and scheme compare case-insensitively, as DNS and URLs do.
		Addr: "HTTPS://VAULT.EXAMPLE.COM:8200", EnvAddr: "https://vault.example.com:8200",
		WantSame: true, Why: "differing case",
	}, { // Test 4: A different host does not receive the token.
		Addr: "https://evil.example.com:8200", EnvAddr: "https://vault.example.com:8200",
		WantSame: false, Why: "a different host",
	}, { // Test 5: A subdomain of the pinned host is a different host.
		Addr: "https://vault.example.com.evil.test:8200", EnvAddr: "https://vault.example.com:8200",
		WantSame: false, Why: "a lookalike subdomain",
	}, { // Test 6: Userinfo does not make an attacker host look like the pinned one.
		Addr:     "https://vault.example.com:8200@evil.example.com",
		EnvAddr:  "https://vault.example.com:8200",
		WantSame: false, Why: "the pinned host smuggled into userinfo",
	}, { // Test 7: A different port is a different endpoint.
		Addr: "https://vault.example.com:8201", EnvAddr: "https://vault.example.com:8200",
		WantSame: false, Why: "a different port",
	}, { // Test 8: An explicit default port does not match an implicit one, which fails closed.
		Addr: "https://vault.example.com:443", EnvAddr: "https://vault.example.com",
		WantSame: false, Why: "an explicit default port",
	}, { // Test 9: A plaintext address never receives a token pinned to an https endpoint.
		Addr: "http://vault.example.com:8200", EnvAddr: "https://vault.example.com:8200",
		WantSame: false, Why: "a downgrade to http",
	}, { // Test 10: An address with no host matches nothing.
		Addr: "https:///v1", EnvAddr: "https://vault.example.com", WantSame: false,
		Why: "an address with no host",
	}, { // Test 11: An empty config address matches nothing.
		Addr: "", EnvAddr: "https://vault.example.com", WantSame: false, Why: "an empty address",
	}, { // Test 12: An empty pinned address matches nothing.
		Addr: "https://vault.example.com", EnvAddr: "", WantSame: false, Why: "an empty VAULT_ADDR",
	}, { // Test 13: Both empty still matches nothing, so an unset VAULT_ADDR never opens the fallback.
		Addr: "", EnvAddr: "", WantSame: false, Why: "both addresses empty",
	}, { // Test 14: A bare host with no scheme has no host to url.Parse and matches nothing.
		Addr: "vault.example.com:8200", EnvAddr: "https://vault.example.com:8200", WantSame: false,
		Why: "a scheme-less address",
	}, { // Test 15: An IPv6 literal matches itself.
		Addr: "https://[fd00::1]:8200", EnvAddr: "https://[fd00::1]:8200", WantSame: true,
		Why: "an IPv6 literal",
	}, { // Test 16: A control character makes the address unparseable, so nothing matches.
		Addr: "https://vault.example.com\x7f:8200", EnvAddr: "https://vault.example.com:8200",
		WantSame: false, Why: "an unparseable address",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := sameVaultEndpoint(test.Addr, test.EnvAddr); got != test.WantSame {
				t.Errorf("sameVaultEndpoint(%q, %q) = %v, want %v: %s",
					test.Addr, test.EnvAddr, got, test.WantSame, test.Why)
			}
		})
	}
}

// TestVaultResolveTokenRefusesWithoutAPinnedAddress pins the fallback rule end to end, including
// the refusal message an operator has to act on. It sets VAULT_ADDR and VAULT_TOKEN, so it does not
// run in parallel.
func TestVaultResolveTokenRefusesWithoutAPinnedAddress(t *testing.T) {
	const pinned = "https://vault.internal.example:8200"
	const envToken = "hvs.the-servers-own-token"

	// Test 0: A config token is used whatever the address, and the environment is not consulted.
	t.Setenv("VAULT_ADDR", pinned)
	t.Setenv("VAULT_TOKEN", envToken)
	got, err := vaultResolveToken("hvs.config-token", "https://anywhere.example.com")
	if err != nil || got != "hvs.config-token" {
		t.Errorf("config token = %q, %v; want the config token", got, err)
	}

	// Test 1: With no config token, the pinned address gets the environment token.
	got, err = vaultResolveToken("", pinned)
	if err != nil || got != envToken {
		t.Errorf("pinned address = %q, %v; want the environment token", got, err)
	}

	// Test 2: Any other address is refused rather than sent the server's token.
	for _, addr := range []string{
		"https://evil.example.com:8200",
		"http://vault.internal.example:8200",
		"https://vault.internal.example:8201",
		"https://vault.internal.example:8200@evil.example.com",
		"",
	} {
		got, err := vaultResolveToken("", addr)
		if !errors.Is(err, ErrResolve) {
			t.Errorf("addr %q = %q, %v; want a refusal so the server's token stays home", addr, got, err)
		}
		if got != "" {
			t.Errorf("addr %q returned the token %q alongside its error", addr, got)
		}
	}

	// Test 3: A pinned address with no token set is refused, not answered with an empty token.
	t.Setenv("VAULT_TOKEN", "")
	if got, err := vaultResolveToken("", pinned); !errors.Is(err, ErrResolve) || got != "" {
		t.Errorf("unset VAULT_TOKEN = %q, %v; want a refusal", got, err)
	}

	// Test 4: An unset VAULT_ADDR closes the fallback entirely.
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("VAULT_TOKEN", envToken)
	if got, err := vaultResolveToken("", pinned); !errors.Is(err, ErrResolve) || got != "" {
		t.Errorf("unset VAULT_ADDR = %q, %v; want a refusal", got, err)
	}
}

// TestLooksLikeOPIDDecidesWhetherANameIsLookedUp pins the id shape check. A value the check calls
// an id is used verbatim as a Connect path segment with no lookup, so a name it mistakes for an id
// silently reads a different item, while an id it fails to recognize is merely a wasted lookup.
func TestLooksLikeOPIDDecidesWhetherANameIsLookedUp(t *testing.T) {
	t.Parallel()
	const validID = "abcdefghijklmnopqrstuvwxyz"
	tests := []struct {
		// In is the candidate vault or item value.
		In string
		// WantID is whether it has the shape of a Connect id.
		WantID bool
		// Why explains the case.
		Why string
	}{{ // Test 0: Twenty-six lowercase letters is an id.
		In: validID, WantID: true, Why: "26 lowercase letters",
	}, { // Test 1: Digits are part of the alphabet.
		In: "a1b2c3d4e5f6g7h8i9j0klmnop", WantID: true, Why: "letters and digits",
	}, { // Test 2: All digits is still an id.
		In: "12345678901234567890123456", WantID: true, Why: "26 digits",
	}, { // Test 3: One character short is a name to look up.
		In: validID[:25], WantID: false, Why: "25 characters",
	}, { // Test 4: One character long is a name to look up.
		In: validID + "a", WantID: false, Why: "27 characters",
	}, { // Test 5: An empty value is a name, not an id.
		In: "", WantID: false, Why: "an empty value",
	}, { // Test 6: An uppercase letter rules it out, so a 26-letter title is looked up.
		In: "Abcdefghijklmnopqrstuvwxyz", WantID: false, Why: "an uppercase letter",
	}, { // Test 7: A hyphen rules it out.
		In: "abcdefghijklm-opqrstuvwxyz", WantID: false, Why: "a hyphen",
	}, { // Test 8: A space rules it out, so a 26-character title with a space is looked up.
		In: "abcdefghijklm opqrstuvwxyz", WantID: false, Why: "a space",
	}, { // Test 9: A slash rules it out, so a name cannot add a path segment.
		In: "abcdefghijklm/opqrstuvwxyz", WantID: false, Why: "a slash",
	}, { // Test 10: A 26-byte unicode value is not an id, since the alphabet is ASCII.
		In: "abcdefghijklmnopqrstuv✓", WantID: false, Why: "a 26-byte unicode value",
	}, { // Test 11: A 26-rune unicode value is not 26 bytes and is not an id.
		In: strings.Repeat("é", 26), WantID: false, Why: "26 unicode runes",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := looksLikeOPID(test.In); got != test.WantID {
				t.Errorf("looksLikeOPID(%q) = %v, want %v: %s", test.In, got, test.WantID, test.Why)
			}
		})
	}
}

// TestOPFieldValuePicksTheFieldTheSourceNames pins which field of a 1Password item becomes the
// credential. An item routinely carries a username, a password, a recovery code, and notes, so
// picking the wrong one hands the run a value that is not a secret at all, or the wrong secret,
// with no error to say so.
//
//nolint:funlen // Test function.
func TestOPFieldValuePicksTheFieldTheSourceNames(t *testing.T) {
	t.Parallel()
	full := opItem{Fields: []opField{
		{ID: "un", Label: "username", Value: "svc"},
		{ID: "pw", Label: "password", Value: "s3cr3t", Purpose: "PASSWORD"},
		{ID: "blank", Label: "empty", Value: ""},
		{ID: "note", Label: "Recovery Code", Value: "rc-123"},
	}}
	tests := []struct {
		// Item is the Connect item read from the server.
		Item opItem
		// Want is the field the source names, empty for the item's password.
		Want string
		// WantValue is the value that must come back.
		WantValue string
		// WantErr is whether the lookup must refuse.
		WantErr bool
		// Why explains the case.
		Why string
	}{{ // Test 0: An empty want returns the field marked as the password.
		Item: full, Want: "", WantValue: "s3cr3t", Why: "the default password field",
	}, { // Test 1: A label selects that field.
		Item: full, Want: "username", WantValue: "svc", Why: "a field by label",
	}, { // Test 2: Labels match case-insensitively, as documented.
		Item: full, Want: "USERNAME", WantValue: "svc", Why: "a label in a different case",
	}, { // Test 3: A label with a space matches.
		Item: full, Want: "recovery code", WantValue: "rc-123", Why: "a multi-word label",
	}, { // Test 4: An id selects that field.
		Item: full, Want: "pw", WantValue: "s3cr3t", Why: "a field by id",
	}, { // Test 5: An id in the wrong case does not match, since ids are exact.
		Item: full, Want: "PW", WantErr: true, Why: "an id in the wrong case",
	}, { // Test 6: A named field with no value is a refusal, never an empty credential.
		Item: full, Want: "empty", WantErr: true, Why: "a field with no value",
	}, { // Test 7: A field the item does not have is a refusal.
		Item: full, Want: "nope", WantErr: true, Why: "an unknown field",
	}, { // Test 8: An item with no password field is a refusal when none was named.
		Item: opItem{Fields: []opField{{ID: "n", Label: "note", Value: "x"}}}, Want: "",
		WantErr: true, Why: "an item with no password",
	}, { // Test 9: A password field whose value is empty is skipped, and the next one is taken.
		Item: opItem{Fields: []opField{
			{ID: "a", Label: "password", Value: "", Purpose: "PASSWORD"},
			{ID: "b", Label: "password", Value: "real", Purpose: "PASSWORD"},
		}}, Want: "", WantValue: "real", Why: "a blank password field before a real one",
	}, { // Test 10: An item with no fields at all is a refusal.
		Item: opItem{}, Want: "", WantErr: true, Why: "an item with no fields",
	}, { // Test 11: An item with no fields is a refusal for a named field too.
		Item: opItem{}, Want: "password", WantErr: true, Why: "a named field on an empty item",
	}, { // Test 12: The first matching label wins when an item carries duplicates.
		Item: opItem{Fields: []opField{
			{ID: "a", Label: "token", Value: "first"},
			{ID: "b", Label: "token", Value: "second"},
		}}, Want: "token", WantValue: "first", Why: "duplicate labels",
	}, { // Test 13: A unicode label matches and a unicode value survives.
		Item: opItem{Fields: []opField{{ID: "u", Label: "密码", Value: "pässwörd"}}},
		Want: "密码", WantValue: "pässwörd", Why: "a unicode label",
	}, { // Test 14: A field whose purpose is not PASSWORD is not the default, whatever its label.
		Item: opItem{Fields: []opField{{ID: "p", Label: "password", Value: "x"}}}, Want: "",
		WantErr: true, Why: "a password-labeled field with no PASSWORD purpose",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := opFieldValue(test.Item, test.Want)
			if test.WantErr {
				if !errors.Is(err, ErrResolve) {
					t.Fatalf("%s = %q, %v; want ErrResolve", test.Why, got, err)
				}
				if got != "" {
					t.Errorf("%s returned the value %q alongside its error", test.Why, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", test.Why, err)
			}
			if diff := cmp.Diff(test.WantValue, got); diff != "" {
				t.Errorf("%s mismatch (-want +got):\n%s", test.Why, diff)
			}
		})
	}
}

// TestAzureVaultNameBoundaries pins the exact edge of the Key Vault name rule. The name is spliced
// into the request authority, so every character the pattern lets through has to be one that cannot
// end the host, and the length bounds are where an off-by-one would either admit a name that
// changes the authority or refuse a vault an operator really has.
//
// It reads azureEndpoint at its default empty value, so it does not run in parallel.
//
//nolint:funlen // Test function.
func TestAzureVaultNameBoundaries(t *testing.T) {
	tests := []struct {
		// Name is the configured Key Vault name.
		Name string
		// WantRefused is whether the name must be refused.
		WantRefused bool
		// Why explains the case.
		Why string
	}{{ // Test 0: An ordinary vault name is accepted.
		Name: "my-vault-01", WantRefused: false, Why: "an ordinary name",
	}, { // Test 1: The shortest name the pattern accepts is two characters.
		Name: "ab", WantRefused: false, Why: "two characters",
	}, { // Test 2: A single character is refused, though Azure's own minimum is three.
		Name: "a", WantRefused: true, Why: "one character",
	}, { // Test 3: An empty name is refused.
		Name: "", WantRefused: true, Why: "an empty name",
	}, { // Test 4: The longest name the pattern accepts is 63 characters.
		Name: "a" + strings.Repeat("b", 62), WantRefused: false, Why: "63 characters",
	}, { // Test 5: One character past the limit is refused.
		Name: "a" + strings.Repeat("b", 63), WantRefused: true, Why: "64 characters",
	}, { // Test 6: A leading hyphen is refused.
		Name: "-vault", WantRefused: true, Why: "a leading hyphen",
	}, { // Test 7: A trailing hyphen is accepted by the pattern, and Azure refuses it later.
		Name: "vault-", WantRefused: false, Why: "a trailing hyphen",
	}, { // Test 8: A dot would end the host and is refused.
		Name: "vault.evil.example.com", WantRefused: true, Why: "a dot",
	}, { // Test 9: An underscore is refused.
		Name: "my_vault", WantRefused: true, Why: "an underscore",
	}, { // Test 10: A newline is refused, so a name cannot split the request.
		Name: "vault\nX-Injected: 1", WantRefused: true, Why: "a newline",
	}, { // Test 11: A nul byte is refused.
		Name: "vault\x00", WantRefused: true, Why: "a nul byte",
	}, { // Test 12: A trailing newline alone is refused, since the pattern is anchored.
		Name: "vault\n", WantRefused: true, Why: "a trailing newline",
	}, { // Test 13: Unicode is refused, so a homoglyph host cannot be built.
		Name: "vаult", WantRefused: true, Why: "a unicode homoglyph",
	}, { // Test 14: Percent encoding is refused rather than decoded into a separator.
		Name: "vault%2eevil", WantRefused: true, Why: "percent encoding",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			base, err := azureVaultBase(test.Name)
			if test.WantRefused {
				if !errors.Is(err, ErrResolve) {
					t.Fatalf("azureVaultBase(%q) = %q, %v; want a refusal: %s",
						test.Name, base, err, test.Why)
				}
				return
			}
			if err != nil {
				t.Fatalf("azureVaultBase(%q) = %v, want the data-plane base: %s", test.Name, err, test.Why)
			}
			want := "https://" + test.Name + ".vault.azure.net"
			if diff := cmp.Diff(want, base); diff != "" {
				t.Errorf("%s base mismatch (-want +got):\n%s", test.Why, diff)
			}
			if err := checkResolveURL(base); err != nil {
				t.Errorf("the base built from %q does not pass the address check: %v", test.Name, err)
			}
		})
	}
}

// TestAWSRegionBoundaries pins the exact edge of the region rule. The region is spliced into the
// Secrets Manager and STS authorities, and the request that follows carries the host's AWS
// credentials, so a region that can end the host sends those credentials to an address the config
// chose. It isolates the AWS environment, so it does not run in parallel.
func TestAWSRegionBoundaries(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	tests := []struct {
		// Region is the configured region.
		Region string
		// WantRefused is whether the region must be refused.
		WantRefused bool
		// Why explains the case.
		Why string
	}{{ // Test 0: A real region is accepted.
		Region: "us-east-1", WantRefused: false, Why: "a real region",
	}, { // Test 1: A one-character region is accepted, since the pattern's floor is one.
		Region: "a", WantRefused: false, Why: "one character",
	}, { // Test 2: An empty region is refused before the pattern, as nothing to fall back to.
		Region: "", WantRefused: true, Why: "an empty region",
	}, { // Test 3: The longest accepted region is 64 characters.
		Region: "a" + strings.Repeat("b", 62) + "c", WantRefused: false, Why: "64 characters",
	}, { // Test 4: One character past the limit is refused.
		Region: "a" + strings.Repeat("b", 63) + "c", WantRefused: true, Why: "65 characters",
	}, { // Test 5: A leading hyphen is refused.
		Region: "-us-east-1", WantRefused: true, Why: "a leading hyphen",
	}, { // Test 6: A trailing hyphen is refused, so the authority cannot be left dangling.
		Region: "us-east-1-", WantRefused: true, Why: "a trailing hyphen",
	}, { // Test 7: Uppercase is refused, since AWS regions are lowercase.
		Region: "US-EAST-1", WantRefused: true, Why: "uppercase",
	}, { // Test 8: A dot would end the host and is refused.
		Region: "us-east-1.evil.example.com", WantRefused: true, Why: "a dot",
	}, { // Test 9: A newline is refused.
		Region: "us-east-1\n", WantRefused: true, Why: "a newline",
	}, { // Test 10: Unicode is refused.
		Region: "us-eаst-1", WantRefused: true, Why: "a unicode homoglyph",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			cfg := awsConfig{AccessKeyID: "AKID", SecretAccessKey: "secret", Region: test.Region}
			creds, region, err := awsResolveCredentials(cfg)
			if test.WantRefused {
				if !errors.Is(err, ErrResolve) {
					t.Fatalf("region %q = %q, %v; want a refusal: %s", test.Region, region, err, test.Why)
				}
				if creds.AccessKeyID != "" || creds.SecretAccessKey != "" {
					t.Error("a refused region still handed back signing credentials")
				}
				return
			}
			if err != nil {
				t.Fatalf("region %q = %v, want it accepted: %s", test.Region, err, test.Why)
			}
			endpoint := fmt.Sprintf("https://%s.%s.amazonaws.com/", awsService, region)
			if err := checkResolveURL(endpoint); err != nil {
				t.Errorf("the endpoint built from %q does not pass the address check: %v", region, err)
			}
			if !strings.HasSuffix(endpoint, ".amazonaws.com/") {
				t.Errorf("region %q built the endpoint %q, which leaves the AWS authority",
					test.Region, endpoint)
			}
		})
	}
}
