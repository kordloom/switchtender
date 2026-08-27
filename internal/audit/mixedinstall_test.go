package audit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestMixedInstallChainVerifies pins the safe degradation an HA install depends on: a chain that ran
// unbound, then adopted an install id, verifies as one whole rather than breaking at the seam.
//
// This is not a contrived state. An install started against a shared database with no signing key
// records unbound, and the operator later supplies a seed to every process; from that point entries
// carry the install and the ones before it do not. A relying party who kept an unbound receipt from
// the early period must still verify it beside the bound ones, or adopting a key would silently void
// every receipt already issued. It works because an empty install id is omitted from the link
// preimage, so an unbound entry hashes exactly as it did before the binding existed, and a bound one
// commits to its id. A change that defaulted the empty id to any placeholder, or refused it, would
// break this without any all-bound or all-unbound test noticing.
//
// It runs against an in-memory store on purpose. The property is in the audit package's own hashing
// and linking, identical on every backend, so a store isolated in this process proves it without the
// shared-database contention that a cross-backend contract test would carry.
func TestMixedInstallChainVerifies(t *testing.T) {
	t.Parallel()
	store := audit.NewMemStore()
	binder, ok := store.(audit.InstallBinder)
	if !ok {
		t.Fatalf("%T does not bind an install", store)
	}

	ctx := context.Background()
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	add := func(i int) {
		e := &audit.Entry{
			ID: fmt.Sprintf("aud_mix%d", i), At: at.Add(time.Duration(i) * time.Second),
			Actor: "release-token", ActorType: "token", Method: "POST", Path: "/v1/runs",
		}
		if err := store.Append(ctx, e); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}

	// Two entries written before any key is supplied, the shape every chain had before the binding.
	add(0)
	add(1)
	// The operator supplies a seed to this process; from here entries name the install.
	const installID = "in_0123456789ab"
	binder.BindInstall(installID)
	add(2)
	add(3)

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 4 {
		t.Fatalf("chain length = %d, want 4", len(chain))
	}
	want := []string{"", "", installID, installID}
	for i, e := range chain {
		if e.InstallID != want[i] {
			t.Errorf("entry %d read back with install_id %q, want %q", e.Seq, e.InstallID, want[i])
		}
	}
	if ok, brokeAt := audit.Verify(chain); !ok {
		t.Fatalf("a chain that adopted an install mid-life does not verify, broke at %d: adopting a "+
			"key would then void every receipt issued before it", brokeAt)
	}
}
