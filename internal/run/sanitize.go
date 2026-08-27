package run

import (
	"math"

	"github.com/kordloom/switchtender/internal/util"
)

// Sanitize makes a run storable identically on every backend: it replaces anything in the text
// fields that a text column cannot hold, and holds the numeric fields inside the range the narrowest
// backend's integer column can take.
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
func (r *Run) Sanitize() {
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
		r.Steps[i].Retries = boundInt32(r.Steps[i].Retries)
	}
	// The numeric columns are declared INTEGER on both backends, which is 64 bits on SQLite and 32 on
	// PostgreSQL. A run submitted with a timeout of three billion seconds was therefore stored on one
	// and refused by the other with an encoding error, so the same request answered 202 or 500
	// depending on which database was behind it. Nothing was clamping them on the way in. The bound
	// is the widest value both can hold rather than a product limit: a timeout of sixty-eight years
	// is already no limit at all, so holding it there changes nothing anybody meant.
	r.Timeout = boundInt32(r.Timeout)
	r.Verbosity = boundInt32(r.Verbosity)
	r.Forks = boundInt32(r.Forks)
	r.Attempt = boundInt32(r.Attempt)
	if r.ShardIndex != nil {
		bounded := boundInt32(*r.ShardIndex)
		r.ShardIndex = &bounded
	}
	if r.StepIndex != nil {
		bounded := boundInt32(*r.StepIndex)
		r.StepIndex = &bounded
	}
	if r.ShardCount != nil {
		bounded := boundInt32(*r.ShardCount)
		r.ShardCount = &bounded
	}
	if r.ExitCode != nil {
		bounded := boundInt32(*r.ExitCode)
		r.ExitCode = &bounded
	}
}

// boundInt32 holds n inside the range a 32-bit integer column can store.
func boundInt32(n int) int {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return n
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
	if f.ExitCode != nil {
		bounded := boundInt32(*f.ExitCode)
		f.ExitCode = &bounded
	}
}

// SanitizeText replaces anything in the progress write's text fields that a text column cannot hold.
func (p *Progress) SanitizeText() {
	if p == nil {
		return
	}
	p.Warning = util.SafeText(p.Warning)
	p.Outputs = util.SafeAnyMap(p.Outputs)
}

// Sanitize replaces anything in a host summary's text fields that a text column cannot hold.
//
// The run's own fields go through Run.Sanitize, but the per-host and per-task summaries written
// beside it did not, and they carry names this install did not choose: a host name comes from an
// imported or dynamic inventory, and a task name comes from somebody's playbook. SQLite stores an
// unrepresentable byte and PostgreSQL refuses it, so the same finished run recorded its fleet
// summary on one backend and, on the other, failed the insert while the run itself finalized. The
// caller logs and continues, so the run looked complete and was silently missing from fleet health,
// drift, host history, task trends, and host costs, on PostgreSQL only.
func (h *HostSummary) Sanitize() {
	if h == nil {
		return
	}
	h.Host = util.SafeText(h.Host)
	h.Worst = util.SafeText(h.Worst)
}

// Sanitize replaces anything in a task summary's text fields that a text column cannot hold.
func (t *TaskSummary) Sanitize() {
	if t == nil {
		return
	}
	t.Task = util.SafeText(t.Task)
}

// Sanitize replaces anything in a host's gathered facts that a text column cannot hold. The keys and
// values both come from the target machine rather than from this install.
func (f *HostFacts) Sanitize() {
	if f == nil {
		return
	}
	f.Host = util.SafeText(f.Host)
	f.Facts = util.SafeStringMap(f.Facts)
}

// SanitizeHostSummaries cleans every summary in place, so a store writes the same bytes on every
// backend from one call at its boundary.
func SanitizeHostSummaries(in []HostSummary) {
	for i := range in {
		in[i].Sanitize()
	}
}

// SanitizeTaskSummaries cleans every task summary in place.
func SanitizeTaskSummaries(in []TaskSummary) {
	for i := range in {
		in[i].Sanitize()
	}
}

// SanitizeHostFacts cleans every host's facts in place.
func SanitizeHostFacts(in []HostFacts) {
	for i := range in {
		in[i].Sanitize()
	}
}
