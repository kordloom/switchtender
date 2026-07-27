package logutil

import "testing"

// TestNew verifies the production logger constructor succeeds.
func TestNew(t *testing.T) {
	t.Parallel()
	log, err := New()
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if log == nil {
		t.Fatal("expected non-nil logger")
	}
	if err := log.Sync(); err != nil {
		t.Logf("Sync returned %v (acceptable on stderr)", err)
	}
}
