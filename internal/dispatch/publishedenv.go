package dispatch

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// publishedEnvApplies reports whether r is a workflow step whose vars Terraform or OpenTofu reads
// from its environment, after its credentials' environment, so a published var could replace one.
func publishedEnvApplies(r *run.Run) bool {
	if r == nil || r.ParentID == nil {
		return false
	}
	tool := run.NormalizeTool(r.Tool)
	return tool == run.ToolTerraform || tool == run.ToolOpenTofu
}

// approvedVarNames returns the extra var keys and secret survey answer names of r's parent, the
// values a person approved the workflow with. It reports false when the parent cannot be read.
func (d *Dispatcher) approvedVarNames(ctx context.Context, r *run.Run) ([]string, bool) {
	if r == nil || r.ParentID == nil || d.store == nil {
		return nil, false
	}
	parent, err := d.store.Get(ctx, *r.ParentID)
	if err != nil || parent == nil {
		return nil, false
	}
	names := slices.Collect(maps.Keys(parent.ExtraVars))
	for _, n := range parent.SealedNames {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names, true
}

// publishedEnvRefusal refuses a Terraform or OpenTofu workflow step that would take a var another
// step published under a name whose TF_VAR_ entry replaces one the step's environment already holds,
// such as an env credential's TF_VAR_api_url. The tool reads the run's vars after the credential
// environment, so the published value would win. A var the workflow was approved with is a choice a
// person saw and is let through. When the approved names cannot be had, every var counts as
// published. A published key that is not a Terraform identifier is refused, since its entry would
// not name the var it seems to.
func publishedEnvRefusal(ctx context.Context, src secretSource, r *run.Run, spec *roundhouse.Spec) error {
	if !publishedEnvApplies(r) || len(spec.ExtraVars) == 0 {
		return nil
	}
	approved, _ := src.approvedVars(ctx, r)
	set := make(map[string]bool, len(spec.Env))
	for _, e := range spec.Env {
		name, _, _ := strings.Cut(e, "=")
		set[name] = true
	}
	for _, k := range slices.Sorted(maps.Keys(spec.ExtraVars)) {
		if slices.Contains(approved, k) {
			continue
		}
		if !terraformIdentifier(k) {
			return fmt.Errorf("%w: %q is not a Terraform variable name, so the step stopped before it "+
				"started", ErrPublishedEnv, k)
		}
		if set["TF_VAR_"+k] {
			return fmt.Errorf("%w: the step's credentials set TF_VAR_%s, and the published var %q "+
				"would replace it, so the step stopped before it started. Publish it under another "+
				"name, or set it in the workflow's own vars", ErrPublishedEnv, k, k)
		}
	}
	return nil
}

// terraformIdentifier reports whether s is a valid Terraform identifier: a letter or underscore
// followed by letters, digits, underscores, and hyphens.
func terraformIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c == '-' || c >= '0' && c <= '9'):
		default:
			return false
		}
	}
	return true
}
