package template

import (
	"context"
	"errors"
	"fmt"
)

// ClaimAWX binds every AWX job template id in bindings that holds no binding yet, and it is called
// before anything else an import or a restore writes, so a different AWX object taking one of those
// ids is refused while nothing else has been written.
//
// Checking the bindings first and binding after every template was stored left a window two imports
// could both pass through, and the one that lost the id learned it only at its bind, after its
// organizations, projects, inventories, credentials, and templates were stored: a refused import
// left half of itself behind, its templates marked as answering an AWX address that reaches
// something else.
//
// An id already bound to the same AWX object, by organization and name, is left as it is. Nothing
// else can take it, and the caller points it at the template it creates once that template is
// stored. An id bound to a different object, found here or lost at the bind to an import that got
// there first, fails with ErrAWXConflict once the ids claimed so far are given back. The returned
// release gives them back too, for a caller that stops before its templates are stored.
func ClaimAWX(ctx context.Context, store Store, bindings []AWXBinding) (func(context.Context) error,
	error) {
	var claimed []AWXBinding
	release := func(ctx context.Context) error {
		var errs []error
		for _, b := range claimed {
			if err := store.UnbindAWX(ctx, b); err != nil {
				errs = append(errs, err)
			}
		}
		claimed = nil
		return errors.Join(errs...)
	}
	for _, b := range bindings {
		held, err := store.AWXBindingFor(ctx, b.AWXID)
		switch {
		case err == nil && held.SameObject(b.Organization, b.Name):
			continue
		case err == nil:
			return nil, errors.Join(fmt.Errorf("%w: awx job template %d is bound to %q",
				ErrAWXConflict, b.AWXID, held.Name), release(ctx))
		case !errors.Is(err, ErrNotFound):
			return nil, errors.Join(fmt.Errorf("read the binding of awx job template %d: %w",
				b.AWXID, err), release(ctx))
		}
		if err := store.BindAWX(ctx, b); err != nil {
			return nil, errors.Join(fmt.Errorf("claim awx job template %d: %w", b.AWXID, err),
				release(ctx))
		}
		claimed = append(claimed, b)
	}
	return release, nil
}
