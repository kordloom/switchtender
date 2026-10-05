package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// callbackContract runs the guards a provisioning callback relies on across replicas: one
// unfinished callback run per template and host, and allowances every process spends from.
func callbackContract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("one live callback run per template and host", func(t *testing.T) {
		testCallbackLiveGuard(t, newStore())
	})
	t.Run("every unfinished status holds the callback lane", func(t *testing.T) {
		testCallbackLiveStatuses(t, newStore)
	})
	t.Run("budgets count within a window", func(t *testing.T) { testBudgets(t, newStore()) })
}

// callbackRun returns an unfinished callback run of template for host.
func callbackRun(id, template, host string, status run.Status) *run.Run {
	return &run.Run{ID: id, Playbook: "boot.yml", Status: status, Source: run.SourceCallback,
		SourceID: template, Limit: host, CreatedAt: time.Now()}
}

// testCallbackLiveGuard pins the lane the store holds: a second unfinished callback run for one
// template and host is refused and not stored, while another host, another template, a run that
// is not a callback, and a child run are all their own, and a finished run frees the lane.
func testCallbackLiveGuard(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, callbackRun("run_cb_a", "tpl_cb", "web01", run.StatusPending)); err != nil {
		t.Fatalf("Save(first callback run) error = %v", err)
	}
	second := callbackRun("run_cb_b", "tpl_cb", "web01", run.StatusPending)
	if err := store.Save(ctx, second); !errors.Is(err, run.ErrCallbackPending) {
		t.Fatalf("Save(second callback run for the host) error = %v, want ErrCallbackPending", err)
	}
	if _, err := store.Get(ctx, "run_cb_b"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Get(refused run) error = %v, want ErrNotFound: a refused save landed", err)
	}
	parent := "run_cb_a"
	child := callbackRun("run_cb_child", "tpl_cb", "web01", run.StatusPending)
	child.ParentID = &parent
	others := []*run.Run{
		callbackRun("run_cb_host", "tpl_cb", "web02", run.StatusPending),
		callbackRun("run_cb_tpl", "tpl_other", "web01", run.StatusPending),
		{ID: "run_cb_api", Playbook: "boot.yml", Status: run.StatusPending, Source: "api",
			SourceID: "tpl_cb", Limit: "web01", CreatedAt: time.Now()},
		child,
	}
	for _, r := range others {
		if err := store.Save(ctx, r); err != nil {
			t.Errorf("Save(%s) error = %v, want it stored: it is not the first run's lane", r.ID, err)
		}
	}
	// The run that holds the lane may still be saved, moving from pending to running.
	first, err := store.Get(ctx, "run_cb_a")
	if err != nil {
		t.Fatalf("Get(first) error = %v", err)
	}
	first.Status = run.StatusRunning
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save(first, running) error = %v, want the holder's own save to land", err)
	}
	first.Status = run.StatusSucceeded
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save(first, succeeded) error = %v", err)
	}
	if err := store.Save(ctx, second); err != nil {
		t.Errorf("Save(second) after the first finished error = %v, want the lane free", err)
	}
}

// testCallbackLiveStatuses walks every declared status: a callback run in an unfinished one holds
// its lane against a second, and one in a finished status does not. It is derived from the declared
// set, so a status added to the type has to be decided here before it can reach the stores.
func testCallbackLiveStatuses(t *testing.T, newStore func() run.Store) {
	ctx := context.Background()
	for testNum, st := range run.AllStatuses() {
		t.Run(fmt.Sprintf("test %d %s", testNum, st), func(t *testing.T) {
			store := newStore()
			if err := store.Save(ctx, callbackRun("run_holder", "tpl_cb", "web01", st)); err != nil {
				t.Fatalf("Save(holder in %s) error = %v", st, err)
			}
			err := store.Save(ctx, callbackRun("run_next", "tpl_cb", "web01", run.StatusPending))
			if held := errors.Is(err, run.ErrCallbackPending); held == st.Terminal() {
				t.Errorf("a callback run in %s: second save error = %v, terminal %v: an unfinished "+
					"run must hold the lane and a finished one must not", st, err, st.Terminal())
			}
		})
	}
}

// testBudgets pins the allowance arithmetic every backend shares: uses count up inside a window,
// the window closes after its length and not before, the next use opens a new one, and keys are
// independent.
func testBudgets(t *testing.T, store run.Store) {
	ctx := context.Background()
	budgets, ok := store.(run.Budgets)
	if !ok {
		t.Fatalf("%T does not keep budgets, so a limit on it holds once per process", store)
	}
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for want := 1; want <= 3; want++ {
		got, err := budgets.SpendBudget(ctx, "callback:10.0.0.1", time.Minute, base)
		if err != nil || got != want {
			t.Fatalf("SpendBudget() use %d = %d, %v, want %d", want, got, err, want)
		}
	}
	tests := []struct {
		Name      string
		Key       string
		At        time.Time
		WantSpent int
	}{{ // Test 0: Inside the window the uses are counted.
		Name: "inside", Key: "callback:10.0.0.1", At: base.Add(30 * time.Second), WantSpent: 3,
	}, { // Test 1: The window's last instant still counts.
		Name: "last instant", Key: "callback:10.0.0.1", At: base.Add(time.Minute), WantSpent: 3,
	}, { // Test 2: Past its end the window is closed.
		Name: "closed", Key: "callback:10.0.0.1", At: base.Add(time.Minute + time.Nanosecond),
	}, { // Test 3: Another key has spent nothing.
		Name: "other key", Key: "callback:10.0.0.2", At: base,
	}}
	for testNum, test := range tests {
		got, err := budgets.BudgetSpent(ctx, test.Key, test.At)
		if err != nil || got != test.WantSpent {
			t.Errorf("test %d %s: BudgetSpent() = %d, %v, want %d", testNum, test.Name, got, err,
				test.WantSpent)
		}
	}
	later := base.Add(time.Minute + time.Nanosecond)
	if got, err := budgets.SpendBudget(ctx, "callback:10.0.0.1", time.Minute, later); err != nil ||
		got != 1 {
		t.Errorf("SpendBudget() after the window closed = %d, %v, want 1: a new window", got, err)
	}
	if got, err := budgets.BudgetSpent(ctx, "callback:10.0.0.1", later.Add(time.Minute)); err != nil ||
		got != 1 {
		t.Errorf("BudgetSpent() in the new window = %d, %v, want 1", got, err)
	}
}

// CallbackGuardsCrossReplicas proves the callback lane and the budgets live in the database rather
// than in a handle: a lane taken and a budget spent through one handle hold through the other, the
// way two server replicas share one database. It is exported so a store package can supply two
// handles on one database, which a single newStore factory cannot express.
func CallbackGuardsCrossReplicas(t *testing.T, a, b run.Store) {
	ctx := context.Background()
	if err := a.Save(ctx, callbackRun("run_cross_a", "tpl_cross", "web01",
		run.StatusPendingApproval)); err != nil {
		t.Fatalf("Save through handle a error = %v", err)
	}
	err := b.Save(ctx, callbackRun("run_cross_b", "tpl_cross", "web01", run.StatusPending))
	if !errors.Is(err, run.ErrCallbackPending) {
		t.Errorf("Save through handle b error = %v, want ErrCallbackPending: two replicas launched "+
			"one host's callback twice", err)
	}
	ab, aok := a.(run.Budgets)
	bb, bok := b.(run.Budgets)
	if !aok || !bok {
		t.Fatalf("%T or %T does not keep budgets", a, b)
	}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := ab.SpendBudget(ctx, "callback-key:10.0.0.9", time.Minute, now); err != nil {
			t.Fatalf("SpendBudget through handle a error = %v", err)
		}
	}
	if got, err := bb.BudgetSpent(ctx, "callback-key:10.0.0.9", now); err != nil || got != 3 {
		t.Errorf("BudgetSpent through handle b = %d, %v, want 3: a budget spent on one replica was "+
			"whole again on the other", got, err)
	}
}
