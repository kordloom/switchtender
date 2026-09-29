package dispatch

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPlaybookRunWithNoRecapSaysSo pins that a playbook run which recorded no per-host result says so
// on its own record.
//
// Such a run contributes nothing to fleet health, drift, host history, or a failed-host relaunch.
// Recording zero hosts and saying nothing is exactly what kept the relay's missing summaries
// invisible: every derived view was quietly short a run with nothing pointing at why. The warning is
// the difference between a gap a reader can see and one they cannot.
func TestPlaybookRunWithNoRecapSaysSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	// Finishes cleanly and emits no events at all, so nothing folds into a host summary.
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "no recap here\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil)
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)

	if got.Status != run.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", got.Status)
	}
	if !strings.Contains(got.Warning, "no per-host result") {
		t.Errorf("warning = %q, want it to say the run recorded no per-host result", got.Warning)
	}
}

// TestRunThatRecordedHostsCarriesNoSuchWarning is the anti-overfit control. A warning that appears on
// every run says nothing, so a run whose events did fold into host summaries must not carry it.
func TestRunThatRecordedHostsCarriesNoSuchWarning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	// Events reach the fold through the run's sidecar file, not through the log stream.
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			lines := strings.Join([]string{
				`{"type":"task_start","ts":1719000000,"task":"install"}`,
				`{"type":"runner_ok","ts":1719000001,"task":"install","host":"web01"}`,
				`{"type":"stats","ts":1719000002,"stats":{` +
					`"web01":{"ok":1,"changed":0,"failures":0,"unreachable":0,"skipped":0}}}`,
			}, "\n") + "\n"
			if err := os.WriteFile(spec.EventsPath, []byte(lines), 0o600); err != nil {
				return roundhouse.Result{}, err
			}
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil)
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)

	summaries, err := store.RunHostSummaries(ctx, created.ID)
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(summaries) == 0 {
		t.Fatal("the fixture recorded no host summaries, so this control proves nothing")
	}
	if strings.Contains(got.Warning, "no per-host result") {
		t.Errorf("a run that recorded %d host(s) was warned about recording none: %q",
			len(summaries), got.Warning)
	}
}
