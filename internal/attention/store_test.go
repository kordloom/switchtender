package attention_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/attentiontest"
)

// TestMemStoreContract runs the shared store contract against the in-memory store.
func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	attentiontest.Contract(t, attention.NewMemStore)
}
