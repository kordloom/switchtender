package run_test

import (
	"testing"

	"github.com/dcadolph/railwarden/internal/run"
	"github.com/dcadolph/railwarden/internal/storetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	storetest.Contract(t, func() run.Store { return run.NewMemStore() })
}
