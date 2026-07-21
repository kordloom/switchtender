package org_test

import (
	"testing"

	"github.com/dcadolph/switchtender/internal/org"
	"github.com/dcadolph/switchtender/internal/orgtest"
)

// TestMemStore runs the shared org.Store contract against the in-memory store.
func TestMemStore(t *testing.T) {
	t.Parallel()
	orgtest.Contract(t, org.NewMemStore)
}
