package run_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/storetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	storetest.Contract(t, func() run.Store { return run.NewMemStore() })
}
