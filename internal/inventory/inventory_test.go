package inventory_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	inventorytest.Contract(t, func() inventory.Store { return inventory.NewMemStore() })
}
