package backup

import (
	"context"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/template"
)

// gatherAWXBindings reads every AWX callback binding, those whose template was deleted included. A
// binding of a deleted template is what keeps its AWX callback address answering gone, so a backup
// that left it out would let a restored install hand the id to whatever claims it next.
func gatherAWXBindings(ctx context.Context, templates template.Store) ([]template.AWXBinding, error) {
	bindings, err := templates.AWXBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: list awx callback bindings: %w", err)
	}
	return bindings, nil
}

// checkAWXBindings refuses, before anything is written, a restore carrying an AWX callback binding
// the install already binds to a different AWX object. Restoring over the binding would hand a boot
// script's callback to a template it never meant, and refusing partway would leave the install
// half restored.
func checkAWXBindings(ctx context.Context, templates template.Store,
	bindings []template.AWXBinding) error {
	for _, b := range bindings {
		held, err := templates.AWXBindingFor(ctx, b.AWXID)
		if errors.Is(err, template.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("restore: read awx callback binding %d: %w", b.AWXID, err)
		}
		if !held.SameObject(b.Organization, b.Name) {
			return fmt.Errorf("%w: awx job template %d is bound to a different awx object on this "+
				"install than in the backup", template.ErrAWXConflict, b.AWXID)
		}
	}
	return nil
}

// claimAWXBindings claims the restore's AWX job template ids before anything else is written and
// returns the release that gives back what it claimed. See template.ClaimAWX.
func claimAWXBindings(ctx context.Context, templates template.Store,
	bindings []template.AWXBinding) (func(context.Context) error, error) {
	if templates == nil || len(bindings) == 0 {
		return func(context.Context) error { return nil }, nil
	}
	release, err := template.ClaimAWX(ctx, templates, bindings)
	if err != nil {
		return nil, fmt.Errorf("restore: %w. It was bound while the restore ran, so nothing was "+
			"restored", err)
	}
	return release, nil
}

// applyAWXBindings restores the AWX callback bindings once the templates they reach are restored,
// and returns how many it wrote.
func applyAWXBindings(ctx context.Context, templates template.Store,
	bindings []template.AWXBinding) (int, error) {
	for n, b := range bindings {
		if err := templates.BindAWX(ctx, b); err != nil {
			return n, fmt.Errorf("restore: bind awx job template %d: %w", b.AWXID, err)
		}
	}
	return len(bindings), nil
}
