package credential_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/credtest"
)

func TestValidateSettings(t *testing.T) {
	t.Parallel()
	// The bounds are the contract: 32 entries, 512-byte values.
	long := strings.Repeat("v", 513)
	big := make(map[string]string, 33)
	for i := 0; i < 33; i++ {
		big[fmt.Sprintf("key_%d", i)] = "value"
	}
	host := credential.SettingRegistryHost
	tests := []struct {
		Name string
		Kind credential.Kind
		In   map[string]string
		Want error
	}{{ // Test 0: No settings at all.
		Name: "nil", Kind: credential.KindSSHKey,
	}, { // Test 1: A plain field name.
		Name: "plain", Kind: credential.KindSSHKey, In: map[string]string{"user": "deploy"},
	}, { // Test 2: An environment style name.
		Name: "env style", Kind: credential.KindEnv, In: map[string]string{"AWS_REGION": "us-east-1"},
	}, { // Test 3: More entries than one credential may carry.
		Name: "too many", Kind: credential.KindEnv, In: big, Want: credential.ErrBadSetting,
	}, { // Test 4: A key holding a space.
		Name: "bad key", Kind: credential.KindSSHKey, In: map[string]string{"user name": "x"},
		Want: credential.ErrBadSetting,
	}, { // Test 5: A key starting with a digit.
		Name: "leading digit", Kind: credential.KindSSHKey, In: map[string]string{"1user": "x"},
		Want: credential.ErrBadSetting,
	}, { // Test 6: A value of spaces alone.
		Name: "empty value", Kind: credential.KindSSHKey, In: map[string]string{"user": "  "},
		Want: credential.ErrBadSetting,
	}, { // Test 7: A value past the length bound.
		Name: "long value", Kind: credential.KindSSHKey, In: map[string]string{"user": long},
		Want: credential.ErrBadSetting,
	}, { // Test 8: A value carrying a newline.
		Name: "newline value", Kind: credential.KindSSHKey, In: map[string]string{"user": "a\nb"},
		Want: credential.ErrBadSetting,
	}, { // Test 9: A value carrying a carriage return.
		Name: "carriage return", Kind: credential.KindSSHKey, In: map[string]string{"user": "a\rb"},
		Want: credential.ErrBadSetting,
	}, { // Test 10: A registry credential bound to a registry.
		Name: "registry bound", Kind: credential.KindRegistry, In: map[string]string{host: "ghcr.io"},
	}, { // Test 11: A registry credential with no settings is bound to no registry.
		Name: "registry unbound", Kind: credential.KindRegistry, Want: credential.ErrBadSetting,
	}, { // Test 12: A registry credential whose settings name everything but its registry.
		Name: "registry other keys", Kind: credential.KindRegistry,
		In: map[string]string{"username": "robot"}, Want: credential.ErrBadSetting,
	}, { // Test 13: A registry host that is not a host.
		Name: "registry url", Kind: credential.KindRegistry,
		In: map[string]string{host: "https://ghcr.io"}, Want: credential.ErrBadSetting,
	}, { // Test 14: Another kind needs no registry host.
		Name: "token unbound", Kind: credential.KindToken,
	}, { // Test 15: Another kind may carry the key as plain metadata, whatever it holds.
		Name: "token metadata", Kind: credential.KindToken,
		In: map[string]string{host: "https://ghcr.io"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := credential.ValidateSettings(test.Kind, test.In)
			if !errors.Is(err, test.Want) {
				t.Errorf("ValidateSettings() error = %v, want %v", err, test.Want)
			}
		})
	}
}

func TestValidateSettingReadsARegistryHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want error
	}{
		{In: "ghcr.io"},                   // Test 0: A host name.
		{In: "docker.io"},                 // Test 1: Docker Hub.
		{In: "registry.example.com:5000"}, // Test 2: A host and port.
		{In: "localhost:5000"},            // Test 3: A single label and port.
		{In: "10.1.2.3"},                  // Test 4: An IPv4 address.
		{In: "10.1.2.3:443"},              // Test 5: An IPv4 address and port.
		{In: "[2001:db8::1]"},             // Test 6: An IPv6 address.
		{In: "[2001:db8::1]:5000"},        // Test 7: An IPv6 address and port.
		{In: "GHCR.IO"},                   // Test 8: Case is not the rule.
		{In: "https://ghcr.io", Want: credential.ErrBadSetting},   // Test 9: A scheme.
		{In: "ghcr.io/org", Want: credential.ErrBadSetting},       // Test 10: A path.
		{In: "ghcr.io/", Want: credential.ErrBadSetting},          // Test 11: A trailing slash.
		{In: "ghcr.io:", Want: credential.ErrBadSetting},          // Test 12: A colon and no port.
		{In: "ghcr.io:0", Want: credential.ErrBadSetting},         // Test 13: Port zero.
		{In: "ghcr.io:65536", Want: credential.ErrBadSetting},     // Test 14: A port past the last.
		{In: "ghcr.io:5000:1", Want: credential.ErrBadSetting},    // Test 15: Two ports.
		{In: "ghcr.io:+80", Want: credential.ErrBadSetting},       // Test 16: A signed port.
		{In: "user@ghcr.io", Want: credential.ErrBadSetting},      // Test 17: A user.
		{In: "ghcr .io", Want: credential.ErrBadSetting},          // Test 18: A space.
		{In: "-ghcr.io", Want: credential.ErrBadSetting},          // Test 19: A leading hyphen.
		{In: "ghcr..io", Want: credential.ErrBadSetting},          // Test 20: An empty label.
		{In: "2001:db8::1", Want: credential.ErrBadSetting},       // Test 21: IPv6 with no brackets.
		{In: "[2001:db8::1", Want: credential.ErrBadSetting},      // Test 22: An open bracket.
		{In: "[10.1.2.3]", Want: credential.ErrBadSetting},        // Test 23: IPv4 in brackets.
		{In: "[fe80::1%eth0]", Want: credential.ErrBadSetting},    // Test 24: A zone.
		{In: "[2001:db8::1]5000", Want: credential.ErrBadSetting}, // Test 25: A port with no colon.
		{In: "*.example.com", Want: credential.ErrBadSetting},     // Test 26: A wildcard.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.In), func(t *testing.T) {
			t.Parallel()
			err := credential.ValidateSetting(credential.KindRegistry,
				credential.SettingRegistryHost, test.In)
			if !errors.Is(err, test.Want) {
				t.Errorf("ValidateSetting(%q) error = %v, want %v", test.In, err, test.Want)
			}
		})
	}
}

func TestCheckPullAllowsOnlyARegistryCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       credential.Credential
		WantHost string
		Want     error
	}{{ // Test 0: A registry credential bound to a registry.
		In: credential.Credential{Kind: credential.KindRegistry,
			Settings: map[string]string{credential.SettingRegistryHost: "ghcr.io"}},
		WantHost: "ghcr.io",
	}, { // Test 1: A registry credential bound to none is still the right kind.
		In: credential.Credential{Kind: credential.KindRegistry},
	}, { // Test 2: An SSH key is never a pull credential.
		In:   credential.Credential{Name: "prod-ssh", Kind: credential.KindSSHKey},
		Want: credential.ErrPullKind,
	}, { // Test 3: A token is never one, whatever its settings say.
		In: credential.Credential{Kind: credential.KindToken,
			Settings: map[string]string{credential.SettingRegistryHost: "ghcr.io"}},
		Want: credential.ErrPullKind,
	}, { // Test 4: A custom type is never one, even when its record says registry.
		In: credential.Credential{Kind: credential.KindRegistry, TypeID: "ctype_1",
			Settings: map[string]string{credential.SettingRegistryHost: "ghcr.io"}},
		Want: credential.ErrPullKind,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := credential.CheckPull(&test.In); !errors.Is(err, test.Want) {
				t.Errorf("CheckPull() error = %v, want %v", err, test.Want)
			}
			if got := test.In.RegistryHost(); got != test.WantHost {
				t.Errorf("RegistryHost() = %q, want %q", got, test.WantHost)
			}
		})
	}
}

func TestSettingsEncodeDecode(t *testing.T) {
	t.Parallel()
	in := map[string]string{"user": "deploy", "become_method": "sudo"}
	text, err := credential.EncodeSettings(in)
	if err != nil {
		t.Fatalf("EncodeSettings() error = %v", err)
	}
	out, err := credential.DecodeSettings(text)
	if err != nil {
		t.Fatalf("DecodeSettings() error = %v", err)
	}
	if diff := cmp.Diff(in, out); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
	// Test: empty encodes to the column default and decodes back to none.
	text, err = credential.EncodeSettings(nil)
	if err != nil || text != "" {
		t.Errorf("EncodeSettings(nil) = %q, %v; want empty, nil", text, err)
	}
	if out, err := credential.DecodeSettings(""); err != nil || out != nil {
		t.Errorf("DecodeSettings(empty) = %v, %v; want nil, nil", out, err)
	}
}

// TestMemStoreSettingsIsolation proves the store copies the settings map, so a caller mutating its
// map after a save cannot rewrite what a later Get returns.
func TestMemStoreSettingsIsolation(t *testing.T) {
	t.Parallel()
	store := credential.NewMemStore()
	settings := map[string]string{"user": "deploy"}
	if err := store.Save(context.Background(),
		&credential.Credential{ID: "cred_1", Settings: settings}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	settings["user"] = "mutated"
	got, err := store.Get(context.Background(), "cred_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Settings["user"] != "deploy" {
		t.Errorf("Settings[user] = %q, want the saved value deploy", got.Settings["user"])
	}
	got.Settings["user"] = "mutated again"
	again, err := store.Get(context.Background(), "cred_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Settings["user"] != "deploy" {
		t.Errorf("Settings[user] = %q after mutating a returned copy, want deploy", again.Settings["user"])
	}
}

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	credtest.Contract(t, func() credential.Store { return credential.NewMemStore() })
}

func TestSealerRoundTrip(t *testing.T) {
	t.Parallel()
	s := credential.NewSealer("passphrase", "salt-a")
	sealed, err := s.Seal("-----BEGIN OPENSSH PRIVATE KEY-----")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if sealed == "-----BEGIN OPENSSH PRIVATE KEY-----" {
		t.Fatal("sealed value equals plaintext")
	}
	plain, err := s.Open(sealed)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if plain != "-----BEGIN OPENSSH PRIVATE KEY-----" {
		t.Errorf("Open() = %q, want the original plaintext", plain)
	}

	other := credential.NewSealer("different", "salt-a")
	if _, err := other.Open(sealed); err == nil {
		t.Error("Open() with the wrong passphrase succeeded, want failure")
	}
}

func TestSealerSaltStabilityAndSeparation(t *testing.T) {
	t.Parallel()
	// Same passphrase and salt rebuilt independently must open each other's ciphertext, which is
	// what lets a restarted process decrypt what a prior one sealed.
	first := credential.NewSealer("pass", "deploy-salt")
	sealed, err := first.Seal("secret")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	second := credential.NewSealer("pass", "deploy-salt")
	plain, err := second.Open(sealed)
	if err != nil {
		t.Fatalf("Open() across a rebuilt sealer error = %v", err)
	}
	if plain != "secret" {
		t.Errorf("Open() = %q, want %q", plain, "secret")
	}

	// A different salt with the same passphrase derives a different key, so it cannot open the
	// ciphertext. This is the per-deployment separation the salt provides.
	otherSalt := credential.NewSealer("pass", "other-salt")
	if _, err := otherSalt.Open(sealed); err == nil {
		t.Error("Open() with a different salt succeeded, want failure")
	}
}

func TestSealerDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Passphrase string
		Salt       string
	}{
		{Name: "no passphrase", Passphrase: "", Salt: "salt"}, // Test 0: Missing key.
		{Name: "no salt", Passphrase: "passphrase", Salt: ""}, // Test 1: Missing salt.
		{Name: "neither", Passphrase: "", Salt: ""},           // Test 2: Missing both.
	}
	for i, test := range tests {
		s := credential.NewSealer(test.Passphrase, test.Salt)
		if s.Enabled() {
			t.Errorf("test %d (%s): Sealer reports enabled", i, test.Name)
		}
		if _, err := s.Seal("x"); !errors.Is(err, credential.ErrNoKey) {
			t.Errorf("test %d (%s): Seal() error = %v, want ErrNoKey", i, test.Name, err)
		}
		if _, err := s.Open("x"); !errors.Is(err, credential.ErrNoKey) {
			t.Errorf("test %d (%s): Open() error = %v, want ErrNoKey", i, test.Name, err)
		}
	}
}

func TestValidKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   credential.Kind
		Want bool
	}{
		{In: credential.KindSSHKey, Want: true},         // Test 0: SSH key.
		{In: credential.KindVaultPassword, Want: true},  // Test 1: Vault password.
		{In: credential.KindEnv, Want: true},            // Test 2: Environment.
		{In: credential.KindBecomePassword, Want: true}, // Test 3: Become password.
		{In: credential.KindRegistry, Want: true},       // Test 4: Registry login.
		{In: credential.KindToken, Want: true},          // Test 5: API token or JWT.
		{In: credential.Kind("nonsense"), Want: false},  // Test 6: Unknown kind.
		{In: credential.Kind(""), Want: false},          // Test 7: Empty kind.
	}
	for i, test := range tests {
		if got := credential.ValidKind(test.In); got != test.Want {
			t.Errorf("test %d: ValidKind(%q) = %v, want %v", i, test.In, got, test.Want)
		}
	}
}

func TestRegistryLogin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       string
		WantUser string
		WantPass string
	}{
		{In: "admin\ns3cret", WantUser: "admin", WantPass: "s3cret"},               // Test 0: Basic split.
		{In: "admin\np:a/s\ns=w0rd", WantUser: "admin", WantPass: "p:a/s\ns=w0rd"}, // Test 1: Password keeps newlines and symbols.
		{In: "solo", WantUser: "solo", WantPass: ""},                               // Test 2: Username only.
	}
	for i, test := range tests {
		user, pass := credential.RegistryLogin(test.In)
		if user != test.WantUser || pass != test.WantPass {
			t.Errorf("test %d: RegistryLogin() = (%q, %q), want (%q, %q)",
				i, user, pass, test.WantUser, test.WantPass)
		}
	}
}

func TestEnvLines(t *testing.T) {
	t.Parallel()
	got := credential.EnvLines("AWS_ACCESS_KEY_ID=abc\n# comment\n\nAWS_SECRET_ACCESS_KEY=xyz\nnot a pair\n")
	want := []string{"AWS_ACCESS_KEY_ID=abc", "AWS_SECRET_ACCESS_KEY=xyz"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("EnvLines() = %v, want %v", got, want)
	}
}
