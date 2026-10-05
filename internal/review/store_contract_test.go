package review_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/reviewtest"
)

// TestMemStoreContract runs the report record contract against the in-memory store.
func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	reviewtest.Contract(t, review.NewMemStore)
}
