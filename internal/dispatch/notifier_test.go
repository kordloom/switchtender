package dispatch

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRegisterNotifier confirms an empty name, a nil notifier, or a duplicate name panics. It does
// not call t.Parallel: it writes the package notifier registry, so it runs in the sequential phase
// before the parallel tests that read the registry resume.
func TestRegisterNotifier(t *testing.T) {
	RegisterNotifier("regdup", NotifierFunc(func(context.Context, *run.Run) error { return nil }))

	tests := []struct {
		Name string
		Reg  string
		N    Notifier
	}{ // Test 0: Empty name is a programming error.
		{Name: "empty name", Reg: "", N: NotifierFunc(func(context.Context, *run.Run) error { return nil })},
		// Test 1: A nil notifier is a programming error.
		{Name: "nil notifier", Reg: "regnil", N: nil},
		// Test 2: A duplicate name is a programming error.
		{Name: "duplicate", Reg: "regdup", N: NotifierFunc(func(context.Context, *run.Run) error { return nil })},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("test %d: RegisterNotifier(%q) did not panic", testNum, test.Reg)
				}
			}()
			RegisterNotifier(test.Reg, test.N)
		})
	}
}

// TestDispatcherNotifiesRegistered registers a Notifier and confirms a terminal run reaches it with
// its extra vars redacted, since a registered channel is external. Non-parallel for the same
// registry-write reason as TestRegisterNotifier.
func TestDispatcherNotifiesRegistered(t *testing.T) {
	got := make(chan *run.Run, 8)
	RegisterNotifier("regdeliver", NotifierFunc(func(_ context.Context, r *run.Run) error {
		select {
		case got <- r:
		default:
		}
		return nil
	}))

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil)
	defer d.Close()

	d.notifyExtra(&run.Run{
		ID: "r-red", Playbook: "red.yml", Status: run.StatusSucceeded,
		ExtraVars: map[string]any{"secret": "x"},
	})

	select {
	case rec := <-got:
		if rec.Playbook != "red.yml" {
			t.Errorf("notifier run = %q, want red.yml", rec.Playbook)
		}
		if rec.ExtraVars != nil {
			t.Errorf("notifier received extra vars %v, want them redacted", rec.ExtraVars)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registered notifier never received the run")
	}
}
