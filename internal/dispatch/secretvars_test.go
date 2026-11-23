package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// secretVarsSealer is one sealer for every test in this file, since deriving a key costs an
// argon2id pass.
//
//nolint:gochecknoglobals // Not modified, simplifies testing.
var secretVarsSealer = sync.OnceValue(func() *credential.Sealer {
	return credential.NewSealer("dispatch-secret-vars-passphrase", "dispatch-secret-vars-salt")
})

// echoSecretRunner records each spec's variables and prints the token answer to the run's output.
type echoSecretRunner struct {
	// mu guards specs.
	mu sync.Mutex
	// specs holds every executed spec's variables.
	specs []map[string]any
}

// Run records the variables and echoes the token.
func (e *echoSecretRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	e.mu.Lock()
	e.specs = append(e.specs, spec.ExtraVars)
	e.mu.Unlock()
	_, _ = fmt.Fprintf(out, "token is %v\n", spec.ExtraVars["token"])
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts lists nothing.
func (e *echoSecretRunner) Hosts(context.Context, string, string) ([]string, error) { return nil, nil }

// seen returns a copy of every executed spec's variables.
func (e *echoSecretRunner) seen() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.specs...)
}

// TestASecretAnswerIsOpenedOnlyForTheExecution pins the executor's half: the sealed answer reaches
// the tool as an ordinary variable, the run keeps only the sealed form, and the answer is masked in
// the stored log even though its name says nothing about being secret.
func TestASecretAnswerIsOpenedOnlyForTheExecution(t *testing.T) {
	t.Parallel()
	const answer = "plain-answer-for-the-tool"
	sealer := secretVarsSealer()
	sealed, err := sealer.Seal(answer)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := run.NewMemStore()
	runner := &echoSecretRunner{}
	d := New(store, runner, nil, WithCredentials(credential.NewMemStore(), sealer))
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("deploy"), run.WithExtraVars(map[string]any{"env": "prod"}),
		run.WithSealedVars(map[string]string{"token": sealed}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := dispatchtest.WaitTerminal(t, store, created.ID)
	if got.Status != run.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", got.Status, got.Error)
	}
	specs := runner.seen()
	if len(specs) != 1 {
		t.Fatalf("executions = %d, want 1", len(specs))
	}
	if diff := cmp.Diff(map[string]any{"env": "prod", "token": answer}, specs[0]); diff != "" {
		t.Errorf("the tool's variables mismatch (-want +got):\n%s", diff)
	}
	if _, ok := got.ExtraVars["token"]; ok {
		t.Errorf("the opened answer was written onto the run: %v", got.ExtraVars)
	}
	logs, err := store.Log(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if strings.Contains(string(logs), answer) || !strings.Contains(string(logs), "token is ***") {
		t.Errorf("log = %q, want the answer masked", logs)
	}
}

// TestASecretAnswerThatCannotBeOpenedFailsTheRun pins that a run never executes with a secret
// answer missing. A tool handed an empty password does something nobody chose, so a run whose
// sealed answers did not reach the executor, which is what a relay worker holds, or that has no key
// to open them, or that was sealed under another key, fails with the reason instead.
func TestASecretAnswerThatCannotBeOpenedFailsTheRun(t *testing.T) {
	t.Parallel()
	sealed, err := secretVarsSealer().Seal("an-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	foreign, err := credential.NewSealer("another-install", "another-salt").Seal("an-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	tests := []struct {
		// Name says what is wrong.
		Name string
		// Sealer is the executor's sealer, nil for none.
		Sealer *credential.Sealer
		// Run is the run handed to the executor.
		Run *run.Run
		// WantErr is the error openSecretVars must return.
		WantErr error
	}{{ // Test 0: Names arrive without values, as on a relay worker.
		Name: "values missing", Sealer: secretVarsSealer(),
		Run: &run.Run{SealedNames: []string{"token"}}, WantErr: ErrSecretAnswer,
	}, { // Test 1: No key on the executor.
		Name: "no key", Sealer: nil,
		Run:     &run.Run{SealedNames: []string{"token"}, SealedVars: map[string]string{"token": sealed}},
		WantErr: credential.ErrNoKey,
	}, { // Test 2: Sealed under a different key.
		Name: "other key", Sealer: secretVarsSealer(),
		Run: &run.Run{
			SealedNames: []string{"token"}, SealedVars: map[string]string{"token": foreign},
		},
		WantErr: ErrSecretAnswer,
	}, { // Test 3: A run with no secret answers needs no key at all.
		Name: "none", Sealer: nil, Run: &run.Run{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			d := &Dispatcher{sealer: test.Sealer}
			_, err := d.openSecretVars(test.Run)
			if !errors.Is(err, test.WantErr) {
				t.Errorf("openSecretVars() error = %v, want %v", err, test.WantErr)
			}
		})
	}

	// End to end, the run fails and the tool never starts.
	store := run.NewMemStore()
	runner := &echoSecretRunner{}
	d := New(store, runner, nil)
	defer d.Close()
	created, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("deploy"), run.WithSealedVars(map[string]string{"token": sealed}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := dispatchtest.WaitTerminal(t, store, created.ID)
	if got.Status != run.StatusFailed || !strings.Contains(got.Error, ErrSecretAnswer.Error()) {
		t.Errorf("run = %s (%q), want failed naming the unavailable answer", got.Status, got.Error)
	}
	if n := len(runner.seen()); n != 0 {
		t.Errorf("the tool ran %d times without its secret answer", n)
	}
}

// TestEveryPipelineStepReceivesTheSecretAnswer pins that a saved workflow's secret answer reaches
// each step the way its plain answers do, opened only inside each step's own execution.
func TestEveryPipelineStepReceivesTheSecretAnswer(t *testing.T) {
	t.Parallel()
	const answer = "workflow-secret-answer"
	sealer := secretVarsSealer()
	sealed, err := sealer.Seal(answer)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	store := run.NewMemStore()
	runner := &echoSecretRunner{}
	d := New(store, runner, nil, WithCredentials(credential.NewMemStore(), sealer))
	defer d.Close()
	steps := []run.PipelineStep{
		{Name: "one", Tool: run.ToolBash, Command: "a"},
		{Name: "two", Tool: run.ToolBash, Command: "b", DependsOn: []string{"one"}},
	}
	parent, err := d.SubmitPipeline(context.Background(), "rotate", "", steps,
		run.WithSealedVars(map[string]string{"token": sealed}))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := dispatchtest.WaitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Fatalf("pipeline = %s (%s), want succeeded", got.Status, got.Error)
	}
	specs := runner.seen()
	if len(specs) != len(steps) {
		t.Fatalf("executions = %d, want %d", len(specs), len(steps))
	}
	for i, vars := range specs {
		if vars["token"] != answer {
			t.Errorf("step %d received token = %v, want the workflow's answer", i, vars["token"])
		}
	}
}

// untemplatedRunner records each executed spec's untemplated names and the answer it carried, and
// lists two hosts so a split has two shards.
type untemplatedRunner struct {
	// mu guards seen.
	mu sync.Mutex
	// seen holds each execution's untemplated names, keyed by its limit.
	seen map[string][]string
	// answers holds each execution's pw variable, keyed by its limit.
	answers map[string]any
}

// Run records the spec.
func (u *untemplatedRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.seen == nil {
		u.seen, u.answers = map[string][]string{}, map[string]any{}
	}
	u.seen[spec.Limit] = spec.UntemplatedVars
	u.answers[spec.Limit] = spec.ExtraVars["pw"]
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts lists two hosts.
func (u *untemplatedRunner) Hosts(context.Context, string, string) ([]string, error) {
	return []string{"web1", "web2"}, nil
}

// got returns what the execution with limit carried.
func (u *untemplatedRunner) got(limit string) ([]string, any, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	names, ok := u.seen[limit]
	return names, u.answers[limit], ok
}

// TestEveryRouteMarksSecretAnswersUntemplated pins that the spec of an Ansible run carrying a secret
// answer names it as untemplated on every route that reaches execution: a launch, a schedule's
// sealed default, a relaunch of failed hosts, and a split child. The vars file marks exactly these
// names, so a route that dropped them would hand Ansible template code nobody approved.
func TestEveryRouteMarksSecretAnswersUntemplated(t *testing.T) {
	t.Parallel()
	const answer = "{{ lookup('pipe','id') }}"
	sealer := secretVarsSealer()
	sealed, err := sealer.Seal(answer)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	ctx := context.Background()
	want := []string{"pw"}
	check := func(t *testing.T, runner *untemplatedRunner, limit string) {
		t.Helper()
		names, got, ok := runner.got(limit)
		if !ok {
			t.Fatalf("no execution with limit %q", limit)
		}
		if diff := cmp.Diff(want, names); diff != "" {
			t.Errorf("UntemplatedVars mismatch (-want +got):\n%s", diff)
		}
		if got != answer {
			t.Errorf("pw = %v, want the answer as typed", got)
		}
	}
	newDispatcher := func(t *testing.T) (*Dispatcher, run.Store, *untemplatedRunner) {
		t.Helper()
		store, runner := run.NewMemStore(), &untemplatedRunner{}
		d := New(store, runner, nil, WithCredentials(credential.NewMemStore(), sealer), WithNoJanitor())
		t.Cleanup(d.Close)
		return d, store, runner
	}

	t.Run("launch and relaunch", func(t *testing.T) {
		t.Parallel()
		d, store, runner := newDispatcher(t)
		created, err := d.Submit(ctx, "site.yml", "inv", run.WithSealedVars(map[string]string{"pw": sealed}))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		done := dispatchtest.WaitTerminal(t, store, created.ID)
		if done.Status != run.StatusSucceeded {
			t.Fatalf("launch = %s (%s), want succeeded", done.Status, done.Error)
		}
		check(t, runner, "")
		saveFinishedRun(t, store, done, []run.HostSummary{{Host: "web2", Failures: 1}})
		relaunch, err := d.RelaunchFailedHosts(ctx, created.ID)
		if err != nil {
			t.Fatalf("RelaunchFailedHosts() error = %v", err)
		}
		if got := dispatchtest.WaitTerminal(t, store, relaunch.ID); got.Status != run.StatusSucceeded {
			t.Fatalf("relaunch = %s (%s), want succeeded", got.Status, got.Error)
		}
		check(t, runner, "web2")
	})

	t.Run("schedule sealed default", func(t *testing.T) {
		t.Parallel()
		d, store, runner := newDispatcher(t)
		tmpl := &template.Template{Playbook: "site.yml", Survey: []template.SurveyField{
			{Var: "pw", Label: "Password", Type: template.FieldSecret, SealedDefault: sealed},
		}}
		opts, err := tmpl.UnattendedOptions()
		if err != nil {
			t.Fatalf("UnattendedOptions() error = %v", err)
		}
		created, err := d.Submit(ctx, "site.yml", "inv", append(opts, run.WithActorType("schedule"))...)
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		if got := dispatchtest.WaitTerminal(t, store, created.ID); got.Status != run.StatusSucceeded {
			t.Fatalf("scheduled run = %s (%s), want succeeded", got.Status, got.Error)
		}
		check(t, runner, "")
	})

	t.Run("split child", func(t *testing.T) {
		t.Parallel()
		d, store, runner := newDispatcher(t)
		parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithSealedVars(map[string]string{"pw": sealed}))
		if err != nil {
			t.Fatalf("SubmitSplit() error = %v", err)
		}
		if got := dispatchtest.WaitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
			t.Fatalf("split = %s (%s), want succeeded", got.Status, got.Error)
		}
		check(t, runner, "web1")
		check(t, runner, "web2")
	})
}
