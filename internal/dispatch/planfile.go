package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
// the run's private directory, which is removed whole when the run ends. A plan the gate makes before
// an apply saves its plan file there. A gated apply carries out plan, the plan file its approval
// bound, written there for the tool. Any other execution touches no plan file. A run with no private
// directory is refused rather than left to apply without its plan, which would plan again.
func preparePlanFile(r *run.Run, dryRun bool, plan []byte, spec *roundhouse.Spec) error {
	planning := isPlanTool(r.Tool) && dryRun && !r.DryRun
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
