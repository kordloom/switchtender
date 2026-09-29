package receipt_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/receipt"
)

// noMaterializeAudits refuses Chain, the whole-history read. A receipt built against it provably
// streams the chain instead of assembling every entry ever written per request.
type noMaterializeAudits struct {
	audit.Store
}

// Chain refuses the whole-history read.
func (s *noMaterializeAudits) Chain(context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("test: receipt.Build materialized the whole chain")
}

// TestReceiptBuildStreamsTheChain proves both receipt shapes are built from a streamed chain read.
// The endpoint that serves receipts is reachable below admin, so materializing an install's entire
// history per request was a denial of service one curl loop away.
func TestReceiptBuildStreamsTheChain(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := held(t, "approve")
	for _, opts := range []receipt.Options{{}, {Sparse: true}} {
		if _, err := receipt.Build(ctx, runs, &noMaterializeAudits{Store: audits}, id, "test", r.ID, opts); err != nil {
			t.Errorf("Build(sparse=%v) against a no-materialize store: %v", opts.Sparse, err)
		}
	}
}
