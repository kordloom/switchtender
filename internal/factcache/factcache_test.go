package factcache_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/factcachetest"
	"github.com/kordloom/switchtender/internal/inventory"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	factcachetest.Contract(t, func() (factcache.Store, inventory.Store) {
		return factcache.NewMemStore(), inventory.NewMemStore()
	})
}
