package credential_test

import (
	"errors"
	"testing"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/credtest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	credtest.Contract(t, func() credential.Store { return credential.NewMemStore() })
}

func TestSealerRoundTrip(t *testing.T) {
	t.Parallel()
	s := credential.NewSealer("passphrase")
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

	other := credential.NewSealer("different")
	if _, err := other.Open(sealed); err == nil {
		t.Error("Open() with the wrong key succeeded, want failure")
	}
}

func TestSealerDisabled(t *testing.T) {
	t.Parallel()
	s := credential.NewSealer("")
	if s.Enabled() {
		t.Error("empty passphrase Sealer reports enabled")
	}
	if _, err := s.Seal("x"); !errors.Is(err, credential.ErrNoKey) {
		t.Errorf("Seal() error = %v, want ErrNoKey", err)
	}
	if _, err := s.Open("x"); !errors.Is(err, credential.ErrNoKey) {
		t.Errorf("Open() error = %v, want ErrNoKey", err)
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
		{In: credential.Kind("nonsense"), Want: false},  // Test 5: Unknown kind.
		{In: credential.Kind(""), Want: false},          // Test 6: Empty kind.
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
