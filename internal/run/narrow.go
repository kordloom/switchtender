package run

import (
	"fmt"
	"strings"
)

// CheckLimitNarrows refuses a launch limit that would widen a template's target instead of narrowing
// it. pinned is the host pattern the template itself sets, empty when it sets none, and requested is
// the one the launch asks for, empty when it asks for none.
//
// A person who chose a template may aim it anywhere their grants reach, and the launch takes their
// limit as a replacement for the template's. An agent works from a menu of templates an operator
// wrote, and a replacement limit let it aim a template pinned to one canary host at a whole
// inventory, under the template name the audit trail records. The widest pattern was worse than
// widening, because the risk grade approval policies key on is computed partly from how wide a run
// reaches, so the widest possible run also graded itself down.
//
// Narrowing is allowed. Touching one host out of many is the useful case, and it cannot do harm the
// template did not already permit.
func CheckLimitNarrows(pinned, requested string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	if WholeInventoryLimit(requested) {
		return fmt.Errorf("%w: %q means every host. Name the hosts this run should touch, or leave "+
			"limit out to use the template's own target", ErrLimitWidens, requested)
	}
	if p := strings.TrimSpace(pinned); p != "" && p != requested {
		return fmt.Errorf("%w: this template pins its target to %q, so the limit cannot change. "+
			"Launch it as defined, or ask an operator for a template that targets %q",
			ErrLimitWidens, p, requested)
	}
	return nil
}
