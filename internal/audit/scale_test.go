package audit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestChainVerifiesAtDepth exercises the tamper-evident chain, the product's core claim, at a depth
// real installs reach rather than the handful of links other tests use. Verification streams the chain
// one entry at a time, so it should stay linear and bounded in memory; this confirms a deep good chain
// verifies, and that a single altered entry deep inside one is still caught at its own position rather
// than slipping through or being misreported.
func TestChainVerifiesAtDepth(t *testing.T) {
	t.Parallel()
	const n = 100_000
	chain := buildChain(t, n) // buildChain verifies the chain it returns, so building this is the pass.
	if len(chain) != n {
		t.Fatalf("built %d entries, want %d", len(chain), n)
	}
	if ok, at := Verify(chain); !ok {
		t.Fatalf("a good %d-entry chain does not verify (broke at %d)", n, at)
	}

	// Alter one entry's content deep in the chain without recomputing its stored hash. Its recomputed
	// hash no longer matches, so a scanner must catch it exactly there.
	mid := n / 2
	original := chain[mid].Actor
	chain[mid].Actor = "mallory"
	ok, brokeAt := Verify(chain)
	if ok {
		t.Fatalf("an altered entry at seq %d passed verification of a %d-entry chain", chain[mid].Seq, n)
	}
	if brokeAt != int(chain[mid].Seq) {
		t.Errorf("verification broke at %d, want the tampered entry's seq %d", brokeAt, chain[mid].Seq)
	}

	// Restoring the entry restores the chain, so the failure was the tamper and not the depth.
	chain[mid].Actor = original
	if ok, at := Verify(chain); !ok {
		t.Fatalf("the restored %d-entry chain does not verify (broke at %d)", n, at)
	}
}

// TestChainScanStreamsAtVolume proves the store's append-and-scan path, the one the health re-walk and
// every bundle export run, holds when a run store has recorded a large chain. Each Append links the
// next entry under the write lock, and ChainScan streams them back in order without materializing the
// whole chain, which is the property that keeps a long-lived install's periodic re-verification from
// loading the entire history into memory.
func TestChainScanStreamsAtVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	const n = 50_000
	for i := 1; i <= n; i++ {
		e := &Entry{
			ID: fmt.Sprintf("aud_%06d", i), At: at.Add(time.Duration(i) * time.Second),
			Actor: "alice", Method: "POST", Path: "/v1/runs",
		}
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}

	// Stream the chain the way the re-walk does, feeding a scanner from genesis.
	scanner := NewChainScanner(true)
	scanned := 0
	if err := store.ChainScan(ctx, 0, func(e *Entry) error {
		scanner.Feed(e)
		scanned++
		return nil
	}); err != nil {
		t.Fatalf("ChainScan() error = %v", err)
	}
	if scanned != n {
		t.Fatalf("ChainScan streamed %d entries, want %d", scanned, n)
	}
	if ok, brokeAt, count := scanner.Result(); !ok || count != n {
		t.Fatalf("streamed verification of %d entries: ok=%v brokeAt=%d count=%d", n, ok, brokeAt, count)
	}
}
