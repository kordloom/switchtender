package invsource_test

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/invsourcetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	invsourcetest.Contract(t, func() invsource.Store { return invsource.NewMemStore() })
}
