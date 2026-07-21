package invsource_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/invsourcetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	invsourcetest.Contract(t, func() invsource.Store { return invsource.NewMemStore() })
}
