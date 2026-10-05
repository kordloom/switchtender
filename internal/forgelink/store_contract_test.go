package forgelink_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/forgelinktest"
)

// TestMemStoreContract runs the forge link contract against the in-memory store.
func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	forgelinktest.Contract(t, forgelink.NewMemStore)
}
