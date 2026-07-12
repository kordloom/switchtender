package secretsource

import (
	"errors"
	"testing"
)

func TestCheckResolveURL(t *testing.T) {
	t.Parallel()
	// Normal targets pass, including loopback and private hosts, which are ordinary internal
	// deployments.
	ok := []string{
		"https://vault.example.com:8200/v1/secret/data/ci",
		"http://127.0.0.1:8200/v1/secret/data/ci",
		"https://10.0.0.5/v1/secret",
	}
	for _, u := range ok {
		if err := checkResolveURL(u); err != nil {
			t.Errorf("checkResolveURL(%q) = %v, want nil", u, err)
		}
	}
	// Metadata and link-local endpoints, a bad scheme, and a missing host are rejected.
	bad := []string{
		"http://169.254.169.254/latest/meta-data",
		"https://metadata.google.internal/computeMetadata",
		"ftp://vault.example.com/x",
		"https:///v1/secret",
		"http://0.0.0.0/v1/secret",
	}
	for _, u := range bad {
		if err := checkResolveURL(u); !errors.Is(err, ErrResolve) {
			t.Errorf("checkResolveURL(%q) = %v, want ErrResolve", u, err)
		}
	}
}

// TestBlockUnsafeDial confirms the dialer guard refuses a resolved metadata, link-local, or
// unspecified address, so a hostname that resolves to one of those cannot slip past the name check.
func TestBlockUnsafeDial(t *testing.T) {
	t.Parallel()
	// A resolved public, private, or loopback address connects normally.
	ok := []string{"93.184.216.34:443", "10.0.0.5:8200", "127.0.0.1:8200"}
	for _, a := range ok {
		if err := blockUnsafeDial("tcp", a, nil); err != nil {
			t.Errorf("blockUnsafeDial(%q) = %v, want nil", a, err)
		}
	}
	// A resolved metadata, link-local, or unspecified address is refused, even when a hostname
	// resolved to it.
	bad := []string{"169.254.169.254:80", "0.0.0.0:80", "[fe80::1]:80"}
	for _, a := range bad {
		if err := blockUnsafeDial("tcp", a, nil); !errors.Is(err, ErrResolve) {
			t.Errorf("blockUnsafeDial(%q) = %v, want ErrResolve", a, err)
		}
	}
}
