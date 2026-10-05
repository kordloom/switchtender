package dispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAnApprovedRunIsHeldToItsFactCacheSetting pins that the fact cache is part of what an approver
// released. A held run is approved with the cache set one way, its stored row is then changed, and
// the executor must refuse it: serving cached facts, or serving older ones, can change what the
// play does to a host, so it is not the change that was approved. An untouched run still executes.
func TestAnApprovedRunIsHeldToItsFactCacheSetting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Tamper      func(r *run.Run)
		UseCache    bool
		Timeout     int
		WantRefused bool
	}{{ // Test 0: The cache turned off after the approval.
		UseCache: true, Timeout: 600, WantRefused: true,
		Tamper: func(r *run.Run) { r.UseFactCache = false },
	}, { // Test 1: The timeout lifted after the approval, so facts of any age would be served.
		UseCache: true, Timeout: 600, WantRefused: true,
		Tamper: func(r *run.Run) { r.FactCacheTimeout = 0 },
	}, { // Test 2: The cache turned on for a run approved without it.
		WantRefused: true,
		Tamper:      func(r *run.Run) { r.UseFactCache = true },
	}, { // Test 3: Nothing changed, so the approved run executes.
		UseCache: true, Timeout: 600, Tamper: func(*run.Run) {},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			// The claim loop stays out of it: the executor is called directly on the tampered row.
			// The audit store is what makes an approval stamp the binding at all.
			d := New(store, okRunner(), nil, WithAudits(audit.NewMemStore()), WithNoJanitor(),
				WithClaimGate(func() error { return errNoClaimingInThisTest }))
			defer d.Close()
			held := &run.Run{
				ID: fmt.Sprintf("run_facts_%d", testNum), Playbook: "site.yml", Inventory: "prod",
				Status: run.StatusPendingApproval, CreatedAt: time.Now(),
				UseFactCache: test.UseCache, FactCacheTimeout: test.Timeout,
			}
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if _, err := d.Approve(ctx, held.ID, decider("approver-pat", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			stored, err := store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			test.Tamper(stored)
			if err := store.Save(ctx, stored); err != nil {
				t.Fatalf("Save(tampered) error = %v", err)
			}
			got := d.execute(ctx, stored)
			after, err := store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			refused := got == run.StatusFailed &&
				strings.Contains(after.Error, "changed after it was approved")
			if refused != test.WantRefused {
				t.Errorf("execute() = %q with error %q, want refused = %v", got, after.Error,
					test.WantRefused)
			}
		})
	}
}
