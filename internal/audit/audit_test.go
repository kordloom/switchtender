package audit_test

import (
	"testing"

	"github.com/dcadolph/switchtender/internal/audit"
	"github.com/dcadolph/switchtender/internal/audittest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	audittest.Contract(t, func() audit.Store { return audit.NewMemStore() })
}
