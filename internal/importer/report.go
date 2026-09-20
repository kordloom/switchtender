package importer

import (
	"regexp"
	"sort"
	"strings"
)

// Report is what an import would do, in the shape somebody deciding whether to migrate needs.
//
// A preview already returns everything it would create and every warning it raised. That is enough
// to be correct and not enough to be persuasive: an operator holding three hundred job templates is
// not asking whether the importer ran, they are asking what does not come across and what they will
// have to do by hand afterward. A flat list of warnings makes them count.
//
// Every warning is one of two things: something the export held that does not come across, or
// something that does come across and should be looked at. There is no third kind, so the report
// has two lists and the second is the default.
//
// The failure that matters is a drop counted as a review item, because that undercounts what is
// missing while the report still looks complete. A guard in the tests reads every warning this
// package raises and fails when one describes a drop in words the classifier does not know.
type Report struct {
	// Created counts what the import would write, by kind, highest first.
	Created []Count `json:"created"`
	// CreatedTotal is every object the import would write.
	CreatedTotal int `json:"created_total"`
	// NeedsSecret is how many credential shells arrive empty and must have their secret entered
	// before anything that uses them can run. Exports never carry secret values, so this is not a
	// limitation of the importer.
	NeedsSecret int `json:"needs_secret"`
	// LeftOut itemizes what the export held and the import does not carry.
	LeftOut []string `json:"left_out,omitempty"`
	// NeedsReview itemizes what came across but should be looked at before it is trusted.
	NeedsReview []string `json:"needs_review,omitempty"`
	// Suppressed is how many warnings were never listed because the plan hit its cap, so a long
	// report is not mistaken for a complete one.
	Suppressed int `json:"suppressed,omitempty"`
}

// Count is one kind of object and how many of it an import would create.
type Count struct {
	// Kind is the object kind, for example templates.
	Kind string `json:"kind"`
	// N is how many of that kind the import would create.
	N int `json:"n"`
}

// leftOutPhrases mark a warning that names something the export held and the import does not carry.
//
// Matched against the warning text rather than recorded at each of the sites that raise one. There
// are fifty-odd such sites across five importers, and tagging each by hand is a change where
// missing one is invisible: the object silently stops being counted while the report still claims
// to be complete. Reading the text moves that risk into a test that fails by name instead.
var leftOutPhrases = regexp.MustCompile(`(?i)\b(skipped|was dropped|were dropped|left out|not imported|did not import|refused|unsupported|has no equivalent|could not be read|was ignored|ignored|not copied|were not copied|does not read)\b`)

// dropVocabulary is every word that suggests an object did not come across. It is deliberately
// wider than leftOutPhrases: anything here that the classifier does not already catch is a new way
// of saying "dropped", and the guard in the tests fails on it rather than letting the item be
// counted as something that merely needs a look.
var dropVocabulary = regexp.MustCompile(`(?i)\b(skip|skipped|drop|dropped|discard|discarded|omit|omitted|exclude|excluded|left out|not imported|did not import|refus|unsupported|no equivalent|ignored|lost|removed|not copied|not carried)\b`)

// Report summarizes what this plan would do.
func (p *Plan) Report() Report {
	r := Report{
		NeedsSecret:  len(p.Credentials),
		CreatedTotal: p.objects(),
		Suppressed:   p.suppressed,
	}
	for _, c := range []Count{
		{Kind: "projects", N: len(p.Projects)},
		{Kind: "inventories", N: len(p.Inventories)},
		{Kind: "inventory sources", N: len(p.Sources)},
		{Kind: "templates", N: len(p.Templates)},
		{Kind: "schedules", N: len(p.Schedules)},
		{Kind: "credentials", N: len(p.Credentials)},
	} {
		if c.N > 0 {
			r.Created = append(r.Created, c)
		}
	}
	sort.SliceStable(r.Created, func(i, j int) bool { return r.Created[i].N > r.Created[j].N })

	for _, w := range p.Warnings {
		// The cap notice is about the report rather than about the export, so it is not an item.
		if strings.HasPrefix(w, "more than ") && strings.Contains(w, "warnings") {
			continue
		}
		// Default to review rather than to left out. Claiming something was dropped when it was
		// carried sends an operator hunting for work that is already done; the other direction is
		// caught by the guard in the tests.
		if leftOutPhrases.MatchString(w) {
			r.LeftOut = append(r.LeftOut, w)
			continue
		}
		r.NeedsReview = append(r.NeedsReview, w)
	}
	return r
}
