package importer

import (
	"sort"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// Assessment is what an estate looks like before anything has been imported.
//
// The migration report answers whether an import is safe to attempt. This answers the question
// somebody asks before that one: what is in there, and what changes about how it is governed.
//
// The distinction matters commercially, not only technically. A report is a tool's output and its
// reader is whoever runs the tool. An assessment is a document, and the person who forwards it to
// their security lead never touched the command line. The governance section exists because that is
// the part the second reader cares about: not that 147 templates move, but that eleven of them can
// destroy something and nobody currently has to agree before they run.
type Assessment struct {
	// Report is what moves and what does not.
	Report Report
	// Governance is what the estate gains by coming through a gate.
	Governance Governance
}

// Governance is what an import would put under control, graded from what the templates themselves
// say they run.
//
// Every count here is derived from the same graders the product uses at run time, not from a
// separate heuristic written for a sales document. A number in an assessment that a run would later
// contradict is worse than no number.
type Governance struct {
	// Templates is how many templates were graded, so every count below has a denominator.
	Templates int
	// Irreversible names templates whose effect cannot be undone from inside the product.
	Irreversible []string
	// Costly names templates that can be undone only by doing more work.
	Costly []string
	// HighRisk names templates carrying a destructive signal.
	HighRisk []string
	// WouldGate is how many templates an approval policy holding on an irreversible grade would
	// stop, which is the single rule most operators say they want and could not previously write.
	WouldGate int
	// SharedCredentials names credentials more than one template uses, with the count. A shared
	// credential is where per-object access control stops meaning anything.
	SharedCredentials []CredentialUse
	// NoInventory names templates that target no stored inventory, so what they reach is decided
	// somewhere this cannot see.
	NoInventory []string
	// Unread is how many templates run a playbook whose text this assessment could not read,
	// because it lives in a repository nothing has fetched yet. Their grades are a floor.
	Unread int
}

// CredentialUse is one credential and how many templates materialize it.
type CredentialUse struct {
	// ID is the credential's id in the plan.
	ID string `json:"id"`
	// Templates is how many templates use it.
	Templates int `json:"templates"`
}

// Assess grades a plan for what it holds and what governing it would change.
func (p *Plan) Assess() Assessment {
	a := Assessment{Report: p.Report()}
	g := Governance{Templates: len(p.Templates)}

	credUse := map[string]int{}
	for _, t := range p.Templates {
		rev := run.AssessReversibility(runShapeOf(t))
		risk := run.AssessRisk(runShapeOf(t))
		switch rev.Class {
		case run.Irreversible:
			g.Irreversible = append(g.Irreversible, t.Name)
			g.WouldGate++
		case run.ReversibleCostly:
			g.Costly = append(g.Costly, t.Name)
		}
		if risk.Level == run.RiskHigh {
			g.HighRisk = append(g.HighRisk, t.Name)
		}
		// An Ansible template keeps its work in a file, so grading it from the template alone
		// reads only what the launch declares. Counted rather than hidden: a governance number
		// that quietly rests on unread files is the kind a buyer is right to distrust.
		if run.NormalizeTool(t.Tool) == run.ToolAnsible && t.Playbook != "" {
			g.Unread++
		}
		if t.Inventory == "" && t.InventoryID == "" {
			g.NoInventory = append(g.NoInventory, t.Name)
		}
		for _, id := range t.CredentialIDs {
			credUse[id]++
		}
	}

	for id, n := range credUse {
		if n > 1 {
			g.SharedCredentials = append(g.SharedCredentials, CredentialUse{ID: id, Templates: n})
		}
	}
	sort.Slice(g.SharedCredentials, func(i, j int) bool {
		if g.SharedCredentials[i].Templates != g.SharedCredentials[j].Templates {
			return g.SharedCredentials[i].Templates > g.SharedCredentials[j].Templates
		}
		return g.SharedCredentials[i].ID < g.SharedCredentials[j].ID
	})
	for _, list := range []*[]string{&g.Irreversible, &g.Costly, &g.HighRisk, &g.NoInventory} {
		sort.Strings(*list)
	}
	a.Governance = g
	return a
}

// runShapeOf builds the run a template would launch, carrying only what the graders read.
//
// The graders take a run because a run is what they grade at execution time. Feeding them the same
// shape here is what keeps an assessment honest: the grade printed in a document somebody forwards
// is produced by the code that will grade the run, so the two cannot drift into disagreeing. A
// separate heuristic written for the document would be a second implementation of a rule that is
// only worth anything if there is exactly one.
func runShapeOf(t *template.Template) *run.Run {
	if t == nil {
		return nil
	}
	return &run.Run{
		Tool: t.Tool, Command: t.Command, Playbook: t.Playbook,
		DryRun: t.DryRun, Limit: t.Limit, ExtraVars: t.ExtraVars,
	}
}
