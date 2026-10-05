package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// openPlanFile opens the sealed plan file the gated apply r carries out, after checking that it is
// the plan file r's approval binds. An apply that carries none, one whose sealed plan no longer
// matches the digest bound when it was proposed, and one that does not open are all refused, never
// planned again.
func (d *Dispatcher) openPlanFile(r *run.Run) ([]byte, error) {
	switch {
	case r.PlanSealed == "":
		return nil, fmt.Errorf("%w: the plan file this apply was proposed with is missing, so it is "+
			"refused rather than planned again", ErrPlanFile)
	case !r.PlanMatches(r.PlanSealed):
		return nil, fmt.Errorf("%w: the stored plan file changed after the apply was proposed, so "+
			"this is not the plan its approval covers", ErrPlanFile)
	}
	b, err := d.openBytes(r.PlanSealed)
	if err != nil {
		return nil, fmt.Errorf("%w: the plan file does not open: %w", ErrPlanFile, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: the plan file is empty", ErrPlanFile)
	}
	return b, nil
}

// SealPlanFile seals the plan file a relay worker's plan saved, with this control node's key, for the
// apply proposed from it to carry at rest.
func (d *Dispatcher) SealPlanFile(plan []byte) (string, error) {
	if len(plan) == 0 {
		return "", fmt.Errorf("%w: the plan file is empty", ErrPlanFile)
	}
	return d.sealBytes(plan)
}

// planFileName is the name a plan file takes in the run's private directory.
const planFileName = "plan.tfplan"

// openPlanForApply opens the plan file a gated apply carries out, through src, held to the digest
// the apply binds. It returns nil for every other execution. It is opened while src still holds what
// a relay worker was delivered, before the run's credentials are applied and the delivery is wiped.
func (d *Dispatcher) openPlanForApply(ctx context.Context, src secretSource, r *run.Run,
	dryRun bool) ([]byte, error) {
	if !isPlanTool(r.Tool) || dryRun || r.PlanSHA256 == "" {
		return nil, nil
	}
	return src.planFile(ctx, r)
}

// isPlanTool reports whether tool saves and applies Terraform or OpenTofu plan files.
func isPlanTool(tool string) bool {
	t := run.NormalizeTool(tool)
	return t == run.ToolTerraform || t == run.ToolOpenTofu
}

// preparePlanFile readies the plan file a Terraform or OpenTofu execution saves or carries out, in
// the run's private directory, which is removed whole when the run ends. Every plan saves its plan
// file there: the one the gate makes before an apply, and a drift check, whose plan a reconcile
// carries out. A gated apply carries out plan, the plan file its approval bound, written there for
// the tool. Any other execution touches no plan file. A run with no private directory is refused
// rather than left to apply without its plan, which would plan again.
func preparePlanFile(r *run.Run, dryRun bool, plan []byte, spec *roundhouse.Spec) error {
	planning := isPlanTool(r.Tool) && dryRun
	if !planning && len(plan) == 0 {
		return nil
	}
	if spec.RunDir == "" {
		return fmt.Errorf("%w: the run has no private directory to hold its plan file", ErrPlanFile)
	}
	dir := filepath.Join(spec.RunDir, "plan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: make the plan file's directory: %w", ErrPlanFile, err)
	}
	path := filepath.Join(dir, planFileName)
	if planning {
		spec.PlanOut = path
		return nil
	}
	if err := os.WriteFile(path, plan, 0o600); err != nil {
		return fmt.Errorf("%w: write the plan file: %w", ErrPlanFile, err)
	}
	spec.PlanFile = path
	return nil
}

// driftPlanKeeper is a store that keeps a drift check's plan on the check's behalf. A relay-backed
// store implements it, because a worker holds no key to seal the plan with: it hands the plan file
// to the control node, which seals it with its own.
type driftPlanKeeper interface {
	// KeepDriftPlanFile hands over the plan file the drift check saved, empty for a check that found
	// no drift.
	KeepDriftPlanFile(ctx context.Context, checkID string, plan []byte) error
}

// keepDriftPlan keeps the plan a Terraform or OpenTofu drift check saved, sealed, so a reconcile
// proposed from the check carries out exactly the plan its approver is shown, the one this check
// made, rather than planning again when it runs. A check that found no drift keeps nothing and
// drops the plan an earlier check of the same target kept, which no longer describes it. A check
// that found drift and could not keep its plan says so on its record, since a reconcile from it is
// refused.
func (d *Dispatcher) keepDriftPlan(r *run.Run, res roundhouse.Result, runErr error, mask *masker) {
	if !isPlanTool(r.Tool) || !r.DryRun || runErr != nil || res.ExitCode != 0 {
		return
	}
	var plan []byte
	if res.Drift {
		plan = res.PlanFile
	}
	ctx := context.Background()
	var err error
	if keeper, ok := d.store.(driftPlanKeeper); ok {
		err = keeper.KeepDriftPlanFile(ctx, r.ID, plan)
	} else {
		sealed := ""
		if len(plan) > 0 {
			sealed, err = d.sealBytes(plan)
		}
		if err == nil {
			err = d.store.KeepDriftPlan(ctx, r.ID, sealed)
		}
	}
	switch {
	case err != nil:
		d.log.Error("dispatch: keep drift plan: "+mask.redactString(err.Error()),
			zap.String("run_id", r.ID))
		if res.Drift {
			addWarning(r, "the plan this check saved was not kept, so a reconcile cannot be proposed "+
				"from it: "+mask.redactString(err.Error()))
		}
	case res.Drift && len(plan) == 0:
		addWarning(r, "this check saved no plan file, so a reconcile cannot be proposed from it")
	}
}

// staleNotice is what Terraform and OpenTofu print when they refuse a saved plan because the state
// changed after the plan was made.
const staleNotice = "Saved plan is stale"

// staleWatch notices the tool refusing a saved plan as stale in the output of an apply that carries
// one out. It holds only the tail of the output that could begin the notice, so a notice split
// across two writes is still seen, and it is safe for the concurrent writes of standard output and
// standard error.
type staleWatch struct {
	// mu guards tail and seen.
	mu sync.Mutex
	// tail is the end of the output so far, shorter than the notice.
	tail []byte
	// seen reports that the notice was printed.
	seen bool
}

// Write scans p for the notice and always reports the whole of it written.
func (w *staleWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen {
		return len(p), nil
	}
	buf := append(w.tail, p...)
	if bytes.Contains(buf, []byte(staleNotice)) {
		w.seen, w.tail = true, nil
		return len(p), nil
	}
	keep := min(len(buf), len(staleNotice)-1)
	w.tail = append(w.tail[:0], buf[len(buf)-keep:]...)
	return len(p), nil
}

// refused reports whether the apply failed because the tool refused its saved plan as stale.
func (w *staleWatch) refused(res roundhouse.Result, runErr error) bool {
	if w == nil || runErr != nil || res.ExitCode == 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// staleRefusal is the failure an apply records when the tool refused its saved plan as stale. The
// tool refuses before it changes anything, so nothing was applied, and the plan cannot be carried
// out any more, so the proposal has to be made again from a new plan: a reconcile from a new drift
// check, and any other apply by submitting it again, which plans it afresh.
func staleRefusal(r *run.Run) string {
	if r.Source == "reconcile" {
		return "the saved plan is stale: the state changed after the drift check made it, so the " +
			"tool applied nothing. Run the drift check again and propose the reconcile again."
	}
	return "the saved plan is stale: the state changed after it was planned, so the tool applied " +
		"nothing. Submit the apply again to plan it afresh and propose it again."
}
