package credential_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/credtest"
)

// TestMemTypeStoreContract runs the shared TypeStore contract against the in-memory backend.
func TestMemTypeStoreContract(t *testing.T) {
	t.Parallel()
	credtest.TypeContract(t, credential.NewMemTypeStore)
}
