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
