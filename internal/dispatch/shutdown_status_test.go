package dispatch

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAShutdownLeavesRunsInterruptedNotCanceled covers what an ordinary upgrade recorded.
//
// Stopping the dispatcher cancels every executing run, which is right: the tool cannot outlive the
// process that is recording it. What was wrong is what it wrote down. The run finalized canceled with
// no error, which is the same record a person clicking cancel leaves, so the chain committed a cancel
// nobody requested for every run in flight during a restart. Interrupted is the status whose whole
// meaning is that the server stopped and the run cannot resume.
//
// It also blocked recovery. A partial retry of a split accepts an interrupted parent, the state an
// unclean kill leaves, so a graceful restart was the one shutdown an operator could not retry from:
// the tidy path recovered worse than the crash.
func TestAShutdownLeavesRunsInterruptedNotCanceled(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()

	started := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			close(started)
			<-ctx.Done()
			return roundhouse.Result{ExitCode: -1}, ctx.Err()
		})

	d := New(store, runner, nil, WithNoJanitor())
	r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("upgrade"),
		run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the run never started")
	}

	// The restart: the process stops the dispatcher and waits for it to drain.
	d.Close()

	got, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.Status.Terminal() {
		t.Fatalf("the run is still %q after the dispatcher drained", got.Status)
	}
	if got.Status == run.StatusCanceled {
		t.Errorf("a run in flight during a shutdown was recorded as canceled, which is what a person "+
			"clicking cancel leaves, with error %q", got.Error)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("status = %q, want interrupted: the server stopped and the run cannot resume",
			got.Status)
	}
	// The record says why, so the run page and the chain do not show a run that simply stopped.
	if got.Error == "" {
		t.Error("the interrupted run carries no reason, so nothing distinguishes it from a run that " +
			"ended for no stated cause")
	}
}

// TestAShutdownLeavesASplitInterrupted covers the shape the finding was actually about: a split in
// flight during an upgrade. Its shards roll up into the parent, so a shard the server stopped has to
// leave the parent interrupted too. Rolling it up as canceled left the whole split looking like
// somebody's decision, and rolling it up as failed would state an outcome the shards never reached,
// while a partial retry accepts only the interrupted parent.
func TestAShutdownLeavesASplitInterrupted(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()

	started := make(chan struct{}, 8)
	runner := &listingRunner{
		hosts: []string{"web-1", "web-2", "web-3", "web-4"},
		run: func(ctx context.Context) (roundhouse.Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			return roundhouse.Result{ExitCode: -1}, ctx.Err()
		},
	}

	d := New(store, runner, nil, WithNoJanitor())
	parent, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2,
		run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	// Wait for a shard to actually be executing, so the shutdown lands mid-run.
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("no shard ever started")
	}

	d.Close()

	got, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status == run.StatusCanceled {
		t.Errorf("a split in flight during a shutdown was recorded as canceled, so a partial retry "+
			"refuses it: %+v", got.Status)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("split status = %q, want interrupted", got.Status)
	}

	// The point of recording it correctly: after the restart the operator can retry the shards that did
	// not finish, which is the recovery an unclean crash already allowed and the graceful restart did
	// not. A fresh dispatcher over the same store is the process that came back up.
	after := New(store, runner, nil, WithNoJanitor())
	defer after.Close()
	retry, err := after.RetryFailedShards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("a split interrupted by a restart cannot be retried: %v", err)
	}
	if retry.RetryOf == nil || *retry.RetryOf != parent.ID {
		t.Errorf("the retry does not name the split it recovers: %+v", retry)
	}
}

// listingRunner is a runner that lists hosts so a split can shard, and runs whatever the test wants.
type listingRunner struct {
	// hosts is the inventory the split fans out over.
	hosts []string
	// run is what each shard's execution does.
	run func(context.Context) (roundhouse.Result, error)
}

// Run executes the test's body.
func (l *listingRunner) Run(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	return l.run(ctx)
}

// Hosts reports the inventory so the dispatcher can split it.
func (l *listingRunner) Hosts(context.Context, string, string) ([]string, error) {
	return l.hosts, nil
}

// TestAUserCancelIsStillRecordedAsCanceled is the other half: the shutdown status must not swallow a
// real cancel. A person stopping a run has to keep leaving a cancel on the record, or the distinction
// this fix exists to draw is lost from the other side.
func TestAUserCancelIsStillRecordedAsCanceled(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()

	started := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			close(started)
			<-ctx.Done()
			return roundhouse.Result{ExitCode: -1}, ctx.Err()
		})

	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()
	r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the run never started")
	}

	d.Cancel(r.ID)

	deadline := time.Now().Add(20 * time.Second)
	var got *run.Run
	for time.Now().Before(deadline) {
		got, err = store.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status.Terminal() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got == nil || !got.Status.Terminal() {
		t.Fatalf("the canceled run never finished: %+v", got)
	}
	if got.Status != run.StatusCanceled {
		t.Errorf("status = %q, want canceled: a person stopped this run", got.Status)
	}
}
