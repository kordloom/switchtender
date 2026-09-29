package logutil

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// TestWrapUnwrapRoundTrip verifies WrapLogger and UnwrapLogger return the attached logger.
func TestWrapUnwrapRoundTrip(t *testing.T) {
	t.Parallel()
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment failed: %v", err)
	}
	ctx := WrapLogger(context.Background(), log)
	got := UnwrapLogger(ctx)
	if got != log {
		t.Errorf("expected attached logger, got different instance")
	}
}

// TestUnwrapLoggerNopWhenAbsent verifies UnwrapLogger returns a non-nil no-op logger when none was attached.
func TestUnwrapLoggerNopWhenAbsent(t *testing.T) {
	t.Parallel()
	got := UnwrapLogger(context.Background())
	if got == nil {
		t.Fatal("expected non-nil no-op logger, got nil")
	}
}

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
