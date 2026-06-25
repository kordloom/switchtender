package run_test

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/storetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	storetest.Contract(t, func() run.Store { return run.NewMemStore() })
}
