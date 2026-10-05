package federation_test

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/federationtest"
)

// TestMemKeyStoreContract runs the signing key contract against the in-memory store, with no clock
// of its own and with one, so the store every issuer test stands on keeps the same promises as the
// database stores it stands in for.
func TestMemKeyStoreContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Store func() federation.KeyStore
	}{{ // Test 0: A store that leaves time to each caller.
		Name: "no clock", Store: func() federation.KeyStore { return federation.NewMemKeyStore() },
	}, { // Test 1: A store standing for a database, with a clock of its own.
		Name: "own clock", Store: func() federation.KeyStore {
			return federation.NewMemKeyStoreWithClock(time.Now)
		},
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			federationtest.KeyContract(t, test.Store)
		})
	}
}

// TestMemKeyStoreIssuerContract runs the issuer's cross-process promises with two simulated
// processes sharing one in-memory store that keeps a clock of its own, as a database does.
func TestMemKeyStoreIssuerContract(t *testing.T) {
	t.Parallel()
	federationtest.IssuerContract(t, func(*testing.T) (federation.KeyStore, federation.KeyStore) {
		store := federation.NewMemKeyStoreWithClock(time.Now)
		return store, store
	})
}
