package decision_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/decisiontest"
)

// TestMemStoreContract runs the decision record contract against the in-memory store.
func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	decisiontest.Contract(t, decision.NewMemStore)
}
