package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheCommunityDaemonRefusesBeforeOpeningAnything checks that the worker daemon a Community
// build carries names Team and never calls Open, so a refused worker creates no database and
// dials no control node.
func TestTheCommunityDaemonRefusesBeforeOpeningAnything(t *testing.T) {
	t.Parallel()
	opened := false
	err := CurrentDaemon().Run(context.Background(), DaemonConfig{
		Open: func() (run.Store, []Option, func(), error) {
			opened = true
			return run.NewMemStore(), nil, func() {}, nil
		},
		Log: zap.NewNop(),
	})
	if !errors.Is(err, license.ErrRequiresTier) {
		t.Fatalf("Run() = %v, want the Team refusal", err)
	}
	if !strings.Contains(err.Error(), "A Team license is required for running distributed workers") {
		t.Errorf("Run() = %q, want it to name Team and distributed workers", err)
	}
	if opened {
		t.Error("the Community daemon called Open before refusing")
	}
}
