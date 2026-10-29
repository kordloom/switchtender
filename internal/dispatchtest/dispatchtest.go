// Package dispatchtest holds what dispatcher tests share across packages: waiting on a run's
// status, runners that stand in for a tool, the configurations the plan gate tests submit, and the
// options an agent's or a person's request stamps on a run. It imports nothing from the dispatcher,
// so the dispatcher's own tests can use it too.
package dispatchtest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// WaitBudget is how long a wait gives a run before failing the test. Ten seconds was enough locally
// and not enough for the largest plan buffer under -race on a loaded two-core runner, so the
// release gate failed on timing rather than on behavior. The budget only bounds how long a genuinely
// stuck run takes to report, so setting it well clear of the slow case costs nothing.
const WaitBudget = 60 * time.Second

// Configurations the plan gate tests submit.
const (
	// CleanConfig declares nothing that runs a program while it plans.
	CleanConfig = "resource \"terraform_data\" \"x\" {\n  input = \"y\"\n}\n"
	// ExternalConfig declares one external data source, which runs its program while it plans.
	ExternalConfig = "data \"external\" \"lookup\" {\n  program = [\"sh\", \"-c\", \"echo {}\"]\n}\n"
)

// WaitTerminal polls the store until the run reaches a terminal state or the deadline passes.
func WaitTerminal(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(WaitBudget)
	for {
		r, err := store.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach a terminal state", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// WaitProposal polls the store until a run proposed from parentID appears, or the deadline passes.
func WaitProposal(t *testing.T, store run.Store, parentID string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := store.List(context.Background())
		if err == nil {
			for _, r := range runs {
				if r.ProposedFrom == parentID {
					return r
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no apply proposed from %s", parentID)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// WaitForStatus blocks until the run reaches want or the deadline passes, failing the test with the
// status it was actually left in.
func WaitForStatus(t *testing.T, store run.Store, id string, want run.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last run.Status
	for time.Now().Before(deadline) {
		got, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		last = got.Status
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s status = %q after 5s, want %q", id, last, want)
}

// OKRunner reports success without doing anything, for tests that exercise submission rather than
// execution output.
func OKRunner() roundhouse.Runner {
	return roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
}

// PlanRunner returns a runner that emits destroyLine for a plan (dry run), returning the saved plan
// it describes, and a plain apply line for a real run, so a plan-content gate test controls the
// destroy count its plan reports.
func PlanRunner(destroyLine string) roundhouse.Runner {
	return roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			if spec.DryRun {
				_, _ = io.WriteString(out, destroyLine+"\n")
				return plantest.Result(destroyLine), nil
			}
			_, _ = io.WriteString(out, "Apply complete!\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
}

// FakeRunnerLister is a runner that also lists a fixed set of hosts for split tests.
type FakeRunnerLister struct {
	// HostNames is what Hosts returns.
	HostNames []string
}

// Run reports success without doing anything.
func (f *FakeRunnerLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns the fixed host set.
func (f *FakeRunnerLister) Hosts(context.Context, string, string) ([]string, error) {
	return f.HostNames, nil
}

// PlanGateRunner emits a fixed plan summary for a plan and counts the applies that really executed,
// so a test can assert whether a gated apply ever reached the infrastructure.
type PlanGateRunner struct {
	// Summary is the plan output written whenever a dry run executes.
	Summary string
	// applies counts executions that were not dry runs, which are the real applies.
	applies atomic.Int64
}

// Run writes the plan summary for a dry run, returning the plan it saved, and records a real apply
// otherwise.
func (p *PlanGateRunner) Run(_ context.Context, spec roundhouse.Spec,
	out io.Writer) (roundhouse.Result, error) {
	if spec.DryRun {
		_, _ = io.WriteString(out, p.Summary)
		return plantest.Result(p.Summary), nil
	}
	p.applies.Add(1)
	_, _ = io.WriteString(out, "Apply complete!\n")
	return roundhouse.Result{ExitCode: 0}, nil
}

// Applies returns how many real applies executed.
func (p *PlanGateRunner) Applies() int64 { return p.applies.Load() }

// WriteFiles writes files under dir, creating the directories they sit in.
func WriteFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// RulesHolding returns a policy store holding the given rules.
func RulesHolding(t *testing.T, rules ...*policy.Policy) policy.Store {
	t.Helper()
	store := policy.NewMemStore()
	for _, p := range rules {
		if err := store.Save(context.Background(), p); err != nil {
			t.Fatalf("Save(policy) error = %v", err)
		}
	}
	return store
}

// AgentOpts returns the options an agent token's request stamps on what it submits: the token's
// label, its kind, the account it is bound to by id and by name, and the identity evidence naming
// that account.
func AgentOpts() []run.SubmitOption {
	return []run.SubmitOption{run.WithActor("release-agent"),
		run.WithActorType(policy.ActorKindAgent), run.WithActorAccount("usr_dev_lead"),
		run.WithAccount("dev-lead"),
		run.WithInitiator(&run.Initiator{InitiatedBy: "release-agent", BoundTo: "dev-lead"})}
}

// PersonOpts returns the options a signed-in person's request stamps on what it submits.
func PersonOpts() []run.SubmitOption {
	return []run.SubmitOption{run.WithActor("dev-lead"), run.WithActorType("session")}
}
