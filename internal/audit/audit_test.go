package audit_test

import (
	"testing"

	"github.com/dcadolph/railwarden/internal/audit"
	"github.com/dcadolph/railwarden/internal/audittest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	audittest.Contract(t, func() audit.Store { return audit.NewMemStore() })
}
