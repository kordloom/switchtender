package run

import "github.com/kordloom/switchtender/internal/util"

// SanitizeText replaces anything in r's text fields that a text column cannot hold: a NUL byte, and
// any byte sequence that is not valid UTF-8.
//
// Every store calls this before it writes, so both backends behave the same way. They did not: SQLite
// stores arbitrary bytes and PostgreSQL refuses them, so the same run finished on one and was
// stranded in running on the other, losing the outcome and exit code the terminal write carried. The
// text is somebody else's, arriving from a tool's output, an imported inventory, or a JSON body, so
// the divergence was reachable without anybody doing anything unusual.
//
// It is applied at the store boundary rather than where each value is set, because the point is that
// no write path can miss it. Fields holding an identifier this package mints, a digest, or a status
// are left alone: they cannot carry an unrepresentable byte, and cleaning them would only hide a bug
// that should surface.
func (r *Run) SanitizeText() {
	if r == nil {
		return
	}
	r.Playbook = util.SafeText(r.Playbook)
	r.Inventory = util.SafeText(r.Inventory)
	r.Command = util.SafeText(r.Command)
	r.Error = util.SafeText(r.Error)
	r.Warning = util.SafeText(r.Warning)
	r.Limit = util.SafeText(r.Limit)
	r.StepName = util.SafeText(r.StepName)
	r.Image = util.SafeText(r.Image)
	r.Intent = util.SafeText(r.Intent)
	r.Actor = util.SafeText(r.Actor)
	r.HeldByPolicy = util.SafeText(r.HeldByPolicy)
	r.Tags = util.SafeTexts(r.Tags)
	r.SkipTags = util.SafeTexts(r.SkipTags)
	r.ExtraVars = util.SafeAnyMap(r.ExtraVars)
	r.Outputs = util.SafeAnyMap(r.Outputs)
	r.Labels = util.SafeStringMap(r.Labels)
	for i := range r.Steps {
		r.Steps[i].Name = util.SafeText(r.Steps[i].Name)
		r.Steps[i].Playbook = util.SafeText(r.Steps[i].Playbook)
		r.Steps[i].Command = util.SafeText(r.Steps[i].Command)
		r.Steps[i].Inventory = util.SafeText(r.Steps[i].Inventory)
		r.Steps[i].DependsOn = util.SafeTexts(r.Steps[i].DependsOn)
	}
}

// SanitizeText replaces anything in the terminal write's text fields that a text column cannot hold.
// The error text is the field that matters: it carries whatever the tool printed as it failed, which
// is exactly where a stray byte comes from, and losing this write loses the run's outcome.
func (f *Finalization) SanitizeText() {
	if f == nil {
		return
	}
	f.Error = util.SafeText(f.Error)
	f.Warning = util.SafeText(f.Warning)
	f.Image = util.SafeText(f.Image)
	f.Outputs = util.SafeAnyMap(f.Outputs)
}

// SanitizeText replaces anything in the progress write's text fields that a text column cannot hold.
func (p *Progress) SanitizeText() {
	if p == nil {
		return
	}
	p.Warning = util.SafeText(p.Warning)
	p.Outputs = util.SafeAnyMap(p.Outputs)
}
