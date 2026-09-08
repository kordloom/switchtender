package policy_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestUnreachableFailsClosed pins the store that cannot answer.
//
// A relay worker leases runs across a segment boundary and never sees the control node's database.
// Handing it a nil store made the plan-content gate vanish, so an apply scoped by a destroy
// threshold was held when the control node claimed the run and applied straight to production when a
// worker did, decided by a race between claim loops. Every method here has to report that it cannot
// see the policies, and it has to be distinguishable from "there are no policies" and from "that
// policy does not exist", because a caller that cannot tell those apart will pick the wrong one.
func TestUnreachableFailsClosed(t *testing.T) {
	t.Parallel()
	var store policy.Store = policy.Unreachable{}
	ctx := context.Background()

	list, err := store.List(ctx)
	if !errors.Is(err, policy.ErrUnreachable) {
		t.Errorf("List() error = %v, want ErrUnreachable", err)
	}
	if list != nil {
		t.Errorf("List() = %v, want no policies alongside the error, so a caller that ignores the "+
			"error cannot mistake it for an empty rule set", list)
	}
	if errors.Is(err, policy.ErrNotFound) || errors.Is(err, policy.ErrReadOnly) {
		t.Error("the unreachable error is indistinguishable from not-found or read-only")
	}
	if !strings.Contains(err.Error(), "cannot") {
		t.Errorf("List() error = %q, want it to say the policies cannot be read from here", err)
	}

	got, err := store.Get(ctx, "pol_anything")
	if !errors.Is(err, policy.ErrUnreachable) || got != nil {
		t.Errorf("Get() = (%v, %v), want (nil, ErrUnreachable)", got, err)
	}
	if err := store.Save(ctx, policy.NewPolicy("x")); !errors.Is(err, policy.ErrUnreachable) {
		t.Errorf("Save() error = %v, want ErrUnreachable", err)
	}
	if err := store.Delete(ctx, "pol_anything"); !errors.Is(err, policy.ErrUnreachable) {
		t.Errorf("Delete() error = %v, want ErrUnreachable", err)
	}
	// A canceled context changes nothing: the answer does not depend on doing any work.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(canceled); !errors.Is(err, policy.ErrUnreachable) {
		t.Errorf("List(canceled) error = %v, want ErrUnreachable", err)
	}
}

// TestFileStoreServesEveryRuleDimension pins that the file schema carries every field that decides
// what a rule does, and that each one lands on the loaded rule.
//
// A field the file parser drops is a rule that reads one way in the diff a reviewer approved and
// behaves another way on the server. The queue scope and the separation-of-duties flag are the two
// worst to lose: the first widens a rule from one segment of the estate to all of them, and the
// second lets the requester release their own change.
func TestFileStoreServesEveryRuleDimension(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, `
policies:
  - name: everything
    tool: opentofu
    command_contains: destroy
    inventory_id: inv_prod
    queue: prod
    exclude_dry_run: true
    actor_kind: agent
    actor: deploy-bot
    min_risk: high
    effect: deny
    require_distinct_approver: true
`)
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	all, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("loaded %d policies, want 1", len(all))
	}
	got := all[0]
	want := &policy.Policy{
		ID: got.ID, Name: "everything", Tool: run.ToolOpenTofu, CommandContains: "destroy",
		InventoryID: "inv_prod", Queue: "prod", ExcludeDryRun: true,
		MaxDestroy: policy.DisabledMaxDestroy, ActorKind: policy.ActorKindAgent, Actor: "deploy-bot",
		MinRisk: run.RiskHigh, Effect: policy.EffectDeny, RequireDistinctApprover: true,
		CreatedAt: got.CreatedAt,
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("loaded rule mismatch (-want +got):\n%s", diff)
	}
	if got.CreatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want the file's modification time in UTC", got.CreatedAt)
	}
	// The rule the file describes has to behave as written, not merely parse.
	matching := &run.Run{
		Tool: run.ToolOpenTofu, Command: "tofu destroy -auto-approve", InventoryID: "inv_prod",
		Queue: "prod", ActorType: "agent", Actor: "deploy-bot",
	}
	if policy.Denying(all, matching) == nil {
		t.Error("the deny rule the file declares refused nothing")
	}
	elsewhere := *matching
	elsewhere.Queue = "staging"
	if policy.Denying(all, &elsewhere) != nil {
		t.Error("a rule scoped to the prod queue refused a run routed elsewhere, so the queue scope " +
			"was dropped on load")
	}
}

// TestFileStoreThresholdIsThreeWay pins the difference between an omitted threshold, a zero
// threshold, and a negative one.
//
// Omitted means a blanket gate that holds every matching run. Zero means a plan-content gate that
// holds on any destroy at all. Collapsing the two in either direction changes what an operator wrote
// into something else: the gate either stops holding at submission, or starts holding every routine
// apply. Negative is refused rather than silently read as disabled, so a reviewer is never shown a
// threshold that does nothing.
func TestFileStoreThresholdIsThreeWay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the policy file.
		Body string
		// WantMaxDestroy is the threshold the loaded rule must carry.
		WantMaxDestroy int
		// WantErr reports whether the file must be refused.
		WantErr bool
	}{{ // Test 0: Omitted is a blanket rule.
		Body:           "policies:\n  - name: x\n    tool: terraform\n",
		WantMaxDestroy: policy.DisabledMaxDestroy,
	}, { // Test 1: Zero is a plan-content rule that holds on any destroy.
		Body: "policies:\n  - name: x\n    tool: terraform\n    max_destroy: 0\n", WantMaxDestroy: 0,
	}, { // Test 2: A positive threshold is carried through.
		Body: "policies:\n  - name: x\n    tool: terraform\n    max_destroy: 25\n", WantMaxDestroy: 25,
	}, { // Test 3: Negative is refused, since it would read as a blanket rule an operator did not write.
		Body: "policies:\n  - name: x\n    max_destroy: -1\n", WantErr: true,
	}, { // Test 4: A far negative value is refused the same way.
		Body: "policies:\n  - name: x\n    max_destroy: -9999\n", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store, err := policy.NewFileStore(writePolicyFile(t, test.Body))
			if test.WantErr {
				if err == nil {
					t.Fatal("a negative threshold loaded, so a reviewer sees a number that does nothing")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFileStore() error = %v", err)
			}
			all, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if all[0].MaxDestroy != test.WantMaxDestroy {
				t.Errorf("MaxDestroy = %d, want %d", all[0].MaxDestroy, test.WantMaxDestroy)
			}
			r := &run.Run{Tool: run.ToolTerraform, Command: "/infra"}
			wantBlanket := test.WantMaxDestroy < 0
			if policy.Requires(all, r) != wantBlanket {
				t.Errorf("Requires() = %v, want %v: an omitted threshold is a blanket gate and a "+
					"stated one is not", !wantBlanket, wantBlanket)
			}
			if policy.PlanGated(all, r) == wantBlanket {
				t.Errorf("PlanGated() = %v, want %v", wantBlanket, !wantBlanket)
			}
		})
	}
}

// TestFileStoreRefusesEveryMalformedShape widens the malformed-file check, because the failure it
// guards is an install where nothing is gated and nothing says so.
//
// Every one of these is a plausible edit. A file that loads with zero rules after any of them is a
// server that starts, serves, and approves everything.
func TestFileStoreRefusesEveryMalformedShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the policy file as written.
		Body string
	}{
		// Test 0: A policy named only by whitespace is still a name, so this one must load. It is
		// here as the boundary either side of the no-name refusal.
		{Body: "policies:\n  - name: \" \"\n"},
		// Test 1: A second policy with no name, so the index in the message matters.
		{Body: "policies:\n  - name: ok\n  - tool: bash\n"},
		// Test 2: A tool that does not exist in this build.
		{Body: "policies:\n  - name: x\n    tool: kubectl\n"},
		// Test 3: A tool in the wrong case, which no run will ever carry.
		{Body: "policies:\n  - name: x\n    tool: Terraform\n"},
		// Test 4: An actor kind that is not one of the two.
		{Body: "policies:\n  - name: x\n    actor_kind: service\n"},
		// Test 5: A risk level this build does not grade.
		{Body: "policies:\n  - name: x\n    min_risk: critical\n"},
		// Test 6: A deny rule with a plan-content threshold, which is a contradiction.
		{Body: "policies:\n  - name: x\n    effect: deny\n    max_destroy: 0\n"},
		// Test 7: The top-level key is not a list.
		{Body: "policies: prod\n"},
		// Test 8: A threshold that is not a number.
		{Body: "policies:\n  - name: x\n    max_destroy: many\n"},
		// Test 9: A boolean field carrying text.
		{Body: "policies:\n  - name: x\n    exclude_dry_run: sometimes\n"},
		// Test 10: Tabs, which YAML refuses as indentation.
		{Body: "policies:\n\t- name: x\n"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store, err := policy.NewFileStore(writePolicyFile(t, test.Body))
			if testNum == 0 {
				if err != nil {
					t.Fatalf("a policy named with a space was refused: %v", err)
				}
				return
			}
			if err == nil {
				all, _ := store.List(context.Background())
				t.Errorf("a malformed policy file loaded %d policies instead of refusing, so the "+
					"server starts with gates the file does not describe:\n%s", len(all), test.Body)
			}
		})
	}
}

// TestFileStoreServesStaleNothing pins that a file that stops being readable stops being served.
//
// The cached parse exists so a read does not touch the disk every time, and that cache is exactly
// where a stale rule set would hide. If the file is deleted, replaced with something malformed, or
// swapped for a directory, the store has to say so rather than keep answering from the last good
// parse: an operator who removed the file believes the gates are gone, and a server still enforcing
// a cached copy is lying in the other direction too, because the next restart would enforce nothing.
func TestFileStoreServesStaleNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("test 0", func(t *testing.T) { // A deleted file is an error, not a cached answer.
		t.Parallel()
		path := writePolicyFile(t, "policies:\n  - name: gate\n    tool: terraform\n")
		store, err := policy.NewFileStore(path)
		if err != nil {
			t.Fatalf("NewFileStore() error = %v", err)
		}
		if all, _ := store.List(ctx); len(all) != 1 {
			t.Fatalf("loaded %d policies, want 1", len(all))
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove() error = %v", err)
		}
		got, err := store.List(ctx)
		if err == nil {
			t.Errorf("List() served %d policies from cache after the file was deleted", len(got))
		}
		if got != nil {
			t.Errorf("List() = %v, want nil alongside the error", got)
		}
		if _, err := store.Get(ctx, "anything"); err == nil {
			t.Error("Get() answered from cache after the file was deleted")
		}
	})

	t.Run("test 1", func(t *testing.T) { // A file that becomes malformed stops being served.
		t.Parallel()
		path := writePolicyFile(t, "policies:\n  - name: gate\n    tool: terraform\n")
		store, err := policy.NewFileStore(path)
		if err != nil {
			t.Fatalf("NewFileStore() error = %v", err)
		}
		if _, err := store.List(ctx); err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if err := os.WriteFile(path, []byte("policies:\n  - name: gate\n    tool: kubectl\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if _, err := store.List(ctx); err == nil {
			t.Error("List() kept serving the last good parse after the file became invalid, so a " +
				"broken edit is enforced until the next restart and then not at all")
		}
	})

	t.Run("test 2", func(t *testing.T) { // A path that is a directory is refused at construction.
		t.Parallel()
		if _, err := policy.NewFileStore(t.TempDir()); err == nil {
			t.Error("a directory loaded as a policy file")
		}
	})

	t.Run("test 3", func(t *testing.T) { // An empty path is refused.
		t.Parallel()
		if _, err := policy.NewFileStore(""); err == nil {
			t.Error("an empty path loaded as a policy file")
		}
	})
}

// TestFileStoreRereadsWhenOnlyTheSizeChanged pins the second half of the freshness check.
//
// The cache is keyed on modification time and size together, because a modification time on its own
// can repeat inside a filesystem's timestamp granularity: two writes in the same tick look identical
// by time alone. A deployment that rewrites the file quickly is exactly that case, so a change that
// only the size reveals still has to take effect.
func TestFileStoreRereadsWhenOnlyTheSizeChanged(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, "policies:\n  - name: one\n    tool: terraform\n")
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	ctx := context.Background()
	if all, _ := store.List(ctx); len(all) != 1 {
		t.Fatalf("loaded %d policies, want 1", len(all))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	body := "policies:\n  - name: one\n    tool: terraform\n  - name: two\n    tool: bash\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// Force the modification time back to what it was, leaving only the size to reveal the change.
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("the store serves %d policies, want 2: a change the modification time could not "+
			"reveal was missed, so a merged rule never took effect", len(all))
	}
}

// TestFileStoreHandsOutCopies pins that a caller cannot reach into the served rule set.
//
// The cached parse is shared by every reader. Handing out the cached pointers meant one caller
// mutating a policy changed what every later caller was gated by, and concurrent readers alongside
// it were a data race on the values that decide approval.
func TestFileStoreHandsOutCopies(t *testing.T) {
	t.Parallel()
	store, err := policy.NewFileStore(writePolicyFile(t,
		"policies:\n  - name: gate\n    tool: terraform\n    command_contains: destroy\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	ctx := context.Background()
	first, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	first[0].Effect = policy.EffectDeny
	first[0].CommandContains = ""
	first[0].Name = "mutated"

	second, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if second[0].Effect == policy.EffectDeny || second[0].CommandContains != "destroy" ||
		second[0].Name != "gate" {
		t.Errorf("mutating a listed policy changed what the store serves: %+v", second[0])
	}
	got, err := store.Get(ctx, second[0].ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	got.Effect = policy.EffectDeny
	again, err := store.Get(ctx, second[0].ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Effect == policy.EffectDeny {
		t.Error("mutating a policy from Get changed what the store serves")
	}
	if _, err := store.Get(ctx, "pol_missing"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, ""); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(empty id) error = %v, want ErrNotFound", err)
	}
}

// TestFileStoreEmptyFileIsNoRules pins that a file declaring nothing is a rule set of nothing rather
// than an error, so an install can legitimately start with no gates.
func TestFileStoreEmptyFileIsNoRules(t *testing.T) {
	t.Parallel()
	for testNum, body := range []string{"", "policies: []\n", "policies:\n", "# only a comment\n"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store, err := policy.NewFileStore(writePolicyFile(t, body))
			if err != nil {
				t.Fatalf("NewFileStore() error = %v", err)
			}
			all, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if all == nil {
				t.Error("List() = nil, want a non-nil empty slice")
			}
			if len(all) != 0 {
				t.Errorf("loaded %d policies from a file declaring none", len(all))
			}
			if policy.Requires(all, &run.Run{Tool: run.ToolTerraform}) {
				t.Error("an empty rule set held a run")
			}
		})
	}
}

// TestFileStorePathIsReported pins that the store names the file it serves, which is what the doctor
// and the logs print when an operator is asking why a gate is or is not in force.
func TestFileStorePathIsReported(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, "policies: []\n")
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	if store.Path() != path {
		t.Errorf("Path() = %q, want %q", store.Path(), path)
	}
	// The refusal to write names the file too, because the whole answer to "why did my change not
	// take" is where to make it instead.
	err = store.Save(context.Background(), policy.NewPolicy("x"))
	if !errors.Is(err, policy.ErrReadOnly) || !strings.Contains(err.Error(), path) {
		t.Errorf("Save() error = %v, want ErrReadOnly naming %q", err, path)
	}
	err = store.Delete(context.Background(), "pol_x")
	if !errors.Is(err, policy.ErrReadOnly) || !strings.Contains(err.Error(), path) {
		t.Errorf("Delete() error = %v, want ErrReadOnly naming %q", err, path)
	}
}

// TestFilePolicyIDIsDerivedFromTheNameAlone pins the identity rule for file policies at its edges.
//
// The id is a hash of the name so an approval recorded against a rule still resolves after the file
// is reordered or edited. That also means two rules sharing a name share an id, and a rule whose
// name changes is a different rule as far as any recorded approval is concerned. Both are worth
// knowing, because both are decisions an operator makes by typing in a file.
func TestFilePolicyIDIsDerivedFromTheNameAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Changing anything but the name leaves the id alone.
	a, err := policy.NewFileStore(writePolicyFile(t, "policies:\n  - name: gate\n    tool: bash\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	b, err := policy.NewFileStore(writePolicyFile(t,
		"policies:\n  - name: gate\n    tool: terraform\n    effect: deny\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	first, _ := a.List(ctx)
	second, _ := b.List(ctx)
	if first[0].ID != second[0].ID {
		t.Errorf("editing a rule changed its id from %s to %s, so an approval recorded against it "+
			"no longer resolves", first[0].ID, second[0].ID)
	}

	// Renaming a rule is a new identity, which is what the hash means.
	c, err := policy.NewFileStore(writePolicyFile(t, "policies:\n  - name: gate2\n    tool: bash\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	third, _ := c.List(ctx)
	if third[0].ID == first[0].ID {
		t.Error("two rules with different names share an id")
	}

	// Names that differ only by unicode, case, or length are distinct rules.
	long := strings.Repeat("policy-", 5000)
	d, err := policy.NewFileStore(writePolicyFile(t, fmt.Sprintf(
		"policies:\n  - name: \"gate\"\n  - name: \"Gate\"\n  - name: \"gaté\"\n  - name: \"%s\"\n",
		long)))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	fourth, _ := d.List(ctx)
	ids := map[string]bool{}
	for _, p := range fourth {
		if ids[p.ID] {
			t.Errorf("two distinct names produced the same id %s", p.ID)
		}
		ids[p.ID] = true
		if !strings.HasPrefix(p.ID, "pol_file_") {
			t.Errorf("ID = %q, want a pol_file_ prefix", p.ID)
		}
	}

	// Two rules with the same name share an id, so an approval against that id names both. The file
	// is still served, since refusing the whole file would take every other gate down with it.
	e, err := policy.NewFileStore(writePolicyFile(t,
		"policies:\n  - name: dup\n    tool: bash\n  - name: dup\n    tool: terraform\n"))
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	fifth, _ := e.List(ctx)
	if len(fifth) != 2 {
		t.Fatalf("loaded %d policies, want 2", len(fifth))
	}
	if fifth[0].ID != fifth[1].ID {
		t.Errorf("two rules named the same have different ids %s and %s", fifth[0].ID, fifth[1].ID)
	}
	got, err := e.Get(ctx, fifth[0].ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Tool != run.ToolBash {
		t.Errorf("Get() on a duplicated name returned the %q rule, want the first one declared",
			got.Tool)
	}
}

// TestFileStoreIsSafeUnderConcurrentReadsAndEdits runs the store the way a serving process does,
// with many readers while the file underneath is being replaced.
//
// Every submitted run reads the rule set, so this is the hottest path in the gate. A torn read here
// would be a run gated by half of one rule set and half of another, and the race detector is the
// only thing that catches the shared-pointer version of that before a customer does.
func TestFileStoreIsSafeUnderConcurrentReadsAndEdits(t *testing.T) {
	t.Parallel()
	path := writePolicyFile(t, "policies:\n  - name: one\n    tool: terraform\n")
	store, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	bodies := []string{
		"policies:\n  - name: one\n    tool: terraform\n",
		"policies:\n  - name: one\n    tool: terraform\n  - name: two\n    tool: bash\n",
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.WriteFile(path, []byte(bodies[i%len(bodies)]), 0o600); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				all, err := store.List(ctx)
				if err != nil {
					continue
				}
				for _, p := range all {
					// A torn rule set would show a name that was never written together with the
					// rest of the rule.
					if p.Name != "one" && p.Name != "two" {
						t.Errorf("List() returned a rule named %q that no version of the file "+
							"declares", p.Name)
						return
					}
					_ = p.Matches(&run.Run{Tool: run.ToolTerraform})
				}
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestMemStoreIsSafeUnderConcurrentUse pins the in-memory store under the mixed traffic a serving
// process gives it, and pins that it hands out copies rather than the values it holds.
//
// The store is documented as safe for concurrent use and it is read on every submission, so a shared
// pointer escaping it is a data race on the values that decide approval.
func TestMemStoreIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	store := policy.NewMemStore()
	ctx := context.Background()
	saved := policy.NewPolicy("gate")
	saved.Tool = run.ToolTerraform
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// The store copies on the way in, so mutating the caller's policy afterwards changes nothing.
	saved.Tool = run.ToolBash
	got, err := store.Get(ctx, saved.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Tool != run.ToolTerraform {
		t.Errorf("Tool = %q, want terraform: the store kept the caller's pointer", got.Tool)
	}
	// And on the way out.
	got.Tool = run.ToolBash
	again, err := store.Get(ctx, saved.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Tool != run.ToolTerraform {
		t.Error("mutating a policy from Get changed stored state")
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := policy.NewPolicy(fmt.Sprintf("rule %d", i))
			for range 25 {
				if err := store.Save(ctx, p); err != nil {
					t.Errorf("Save() error = %v", err)
					return
				}
				if _, err := store.List(ctx); err != nil {
					t.Errorf("List() error = %v", err)
					return
				}
				if _, err := store.Get(ctx, p.ID); err != nil {
					t.Errorf("Get() error = %v", err)
					return
				}
				if err := store.Delete(ctx, p.ID); err != nil {
					t.Errorf("Delete() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := store.Delete(ctx, saved.ID); err != nil {
		t.Errorf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, saved.ID); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Delete() twice error = %v, want ErrNotFound", err)
	}
}

// TestMemStoreOrdersTiedCreationTimesByID pins the tie-break in the listed order.
//
// Policies are consulted in list order and the first matching rule is the one recorded as having
// held a run, so an order that depends on map iteration would make that record differ between two
// reads of the same store. Rules written by an import all carry the same creation time, which is
// exactly when the tie-break decides.
func TestMemStoreOrdersTiedCreationTimesByID(t *testing.T) {
	t.Parallel()
	store := policy.NewMemStore()
	ctx := context.Background()
	same := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	older := same.Add(-time.Hour)
	for _, p := range []*policy.Policy{
		{ID: "pol_c", CreatedAt: same}, {ID: "pol_a", CreatedAt: same},
		{ID: "pol_b", CreatedAt: same}, {ID: "pol_z", CreatedAt: older},
	} {
		if err := store.Save(ctx, p); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	for range 5 {
		all, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		gotIDs := make([]string, 0, len(all))
		for _, p := range all {
			gotIDs = append(gotIDs, p.ID)
		}
		want := []string{"pol_z", "pol_a", "pol_b", "pol_c"}
		if diff := cmp.Diff(want, gotIDs); diff != "" {
			t.Fatalf("List() order mismatch (-want +got):\n%s", diff)
		}
	}
}

// TestFileStoreLoadsFromAnAbsentDirectory pins that a path pointing nowhere is refused with the
// underlying reason attached, so a typo in the configured path is reported as a missing file rather
// than as an install with no gates.
func TestFileStoreLoadsFromAnAbsentDirectory(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-dir", "policies.yaml")
	_, err := policy.NewFileStore(missing)
	if err == nil {
		t.Fatal("a path through a missing directory loaded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("NewFileStore() error = %v, want it to wrap os.ErrNotExist so the operator is told "+
			"the file is not there", err)
	}
	if !strings.Contains(err.Error(), "policy file") {
		t.Errorf("NewFileStore() error = %q, want it to say which file", err)
	}
}
