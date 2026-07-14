package inventory_test

import (
	"testing"

	"github.com/dcadolph/railwarden/internal/inventory"
	"github.com/dcadolph/railwarden/internal/inventorytest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	inventorytest.Contract(t, func() inventory.Store { return inventory.NewMemStore() })
}
