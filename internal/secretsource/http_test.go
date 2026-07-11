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
