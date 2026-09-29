package importer

import (
	"fmt"
	"io"
)

// Render writes the assessment as the document somebody forwards, rather than as a dump.
//
// It lives beside Assess rather than in the command because two callers render it and they must
// not diverge. The command prints it in a terminal and the browser assessment prints it in a page,
// and a web report that counted differently from the command an operator runs afterward would be
// the same failure the shared reader above exists to prevent: a document promising one thing and a
// tool doing another.
//
// source names where the export came from, a path for the command and a file name for the browser.
func Render(out io.Writer, format, source string, a Assessment) {
	fmt.Fprintf(out, "SwitchTender migration assessment\n")
	fmt.Fprintf(out, "  Source: %s export, %s\n\n", format, source)

	fmt.Fprintf(out, "What is in there\n")
	for _, c := range a.Report.Created {
		fmt.Fprintf(out, "  %-22s %6d\n", c.Kind, c.N)
	}
	fmt.Fprintf(out, "  %-22s %6d\n\n", "total", a.Report.CreatedTotal)

	fmt.Fprintf(out, "What survives the move\n")
	fmt.Fprintf(out, "  %-22s %6d\n", "comes across", a.Report.CreatedTotal)
	if a.Report.NeedsSecret > 0 {
		fmt.Fprintf(out, "  %-22s %6d   re-entered once; an export never carries secret values\n",
			"needs a secret", a.Report.NeedsSecret)
	}
	fmt.Fprintf(out, "  %-22s %6d\n", "does not come across", len(a.Report.LeftOut))
	for _, w := range capList(a.Report.LeftOut, 6) {
		fmt.Fprintf(out, "      - %s\n", w)
	}
	fmt.Fprintf(out, "  %-22s %6d\n", "worth reviewing", len(a.Report.NeedsReview))
	for _, w := range capList(a.Report.NeedsReview, 4) {
		fmt.Fprintf(out, "      - %s\n", w)
	}
	if a.Report.Suppressed > 0 {
		fmt.Fprintf(out, "  %d further warning(s) were not listed, because this export passed the "+
			"report's cap.\n", a.Report.Suppressed)
	}
	fmt.Fprintln(out)

	g := a.Governance
	fmt.Fprintf(out, "What changes about how it is governed\n")
	renderGates(out, g.ApprovalGates)
	switch {
	case g.Templates == 0 && len(g.ApprovalGates) > 0:
		// The gates above are something to govern, so the export is not one with nothing to gate.
		fmt.Fprintf(out, "  This export holds no templates to grade, so the approval gates above are\n"+
			"  the whole of what changes about how it is governed.\n\n")
		return
	case g.Templates == 0:
		fmt.Fprintf(out, "  This export holds no templates, so there is nothing here to gate. What\n"+
			"  it brings is the estate itself, which is what later runs are governed against.\n\n")
		return
	}
	fmt.Fprintf(out, "  %-22s %6d\n", "templates graded", g.Templates)
	fmt.Fprintf(out, "  %-22s %6d   cannot be undone from here\n", "irreversible", len(g.Irreversible))
	for _, n := range capList(g.Irreversible, 6) {
		fmt.Fprintf(out, "      - %s\n", n)
	}
	fmt.Fprintf(out, "  %-22s %6d   undone only by doing more work\n", "costly", len(g.Costly))
	fmt.Fprintf(out, "  %-22s %6d   carry a destructive signal\n", "high risk", len(g.HighRisk))
	for _, n := range capList(g.HighRisk, 6) {
		fmt.Fprintf(out, "      - %s\n", n)
	}
	fmt.Fprintf(out, "  %-22s %6d   templates use a credential another template also uses\n",
		"shared credentials", len(g.SharedCredentials))
	fmt.Fprintf(out, "  %-22s %6d   target no stored inventory\n", "no inventory", len(g.NoInventory))
	fmt.Fprintln(out)

	if g.Unread > 0 {
		unread := fmt.Sprintf("%d of them run", g.Unread)
		switch {
		case g.Templates == 1:
			unread = "It runs"
		case g.Unread == 1:
			unread = "1 of them runs"
		}
		fmt.Fprintf(out, "  Grades come from what each template declares: its tool, command, limit,\n"+
			"  and variables. %s an Ansible playbook whose text lives in a repository\n"+
			"  nothing has fetched yet. Reading a playbook can only raise a grade, never lower one,\n"+
			"  so every number above is a floor. The real estate is at least this destructive.\n\n",
			unread)
	}

	// The one sentence this document exists to produce. A lone template is spoken of as one,
	// because "these 1 templates" in the most quoted line reads as a document nobody checked.
	if g.Templates == 1 {
		fmt.Fprintf(out, "  Today this template runs when somebody presses the button.\n")
	} else {
		fmt.Fprintf(out, "  Today any of these %d templates runs when somebody presses the button.\n",
			g.Templates)
	}
	switch {
	case g.WouldGate > 0 && g.Templates == 1:
		fmt.Fprintf(out, "  One approval policy holding on an irreversible grade would stop it until\n"+
			"  a second person agrees, and every run would leave a receipt that verifies\n"+
			"  without us.\n")
	case g.WouldGate > 0:
		fmt.Fprintf(out, "  One approval policy holding on an irreversible grade would stop %d of\n"+
			"  them until a second person agrees, and every run would leave a receipt that\n"+
			"  verifies without us.\n", g.WouldGate)
	case g.Templates == 1:
		fmt.Fprintf(out, "  It does not grade irreversible, so an approval policy holding on that\n"+
			"  grade would not stop it. A policy on risk or on tool is the one to write here.\n")
	default:
		fmt.Fprintf(out, "  None of them grades irreversible, so an approval policy holding on that\n"+
			"  grade would stop none of them. A policy on risk or on tool is the one to write here.\n")
	}
}

// renderGates names the approvals the estate has today that an import does not carry.
//
// It comes before the counts because none of them would show it. A gate lost in a move is the
// largest change to how that work is governed, and it is a change for the worse, which is the one
// thing a document about governing an estate cannot leave for the reader to infer from a warning.
func renderGates(out io.Writer, gates []string) {
	if len(gates) == 0 {
		return
	}
	waits, does := "workflow waits", "That gate does"
	if len(gates) > 1 {
		waits, does = "workflows wait", "Those gates do"
	}
	fmt.Fprintf(out, "  %d %s for a person at an approval node today. %s not\n"+
		"  come across: approval here is a policy that holds a run before it starts, so write\n"+
		"  one before the workflow is rebuilt, or it runs with no gate.\n", len(gates), waits, does)
	for _, n := range capList(gates, 6) {
		fmt.Fprintf(out, "      - %s\n", n)
	}
	fmt.Fprintln(out)
}

// capList returns at most max entries, so a long estate does not bury the section that follows it.
func capList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	out := make([]string, 0, max+1)
	out = append(out, items[:max]...)
	return append(out, fmt.Sprintf("and %d more", len(items)-max))
}

// Headline is the one sentence the browser assessment leads with, and which kind of answer it is.
type Headline struct {
	// Kind is gap when something runs unasked or a gate is lost, and clear otherwise.
	Kind string
	// Text is the sentence.
	Text string
}

// HeadlineOf decides the sentence the browser assessment leads with.
//
// It lives beside Render for the reason Render does. The page wrote this sentence in its own script
// from the counts, which made it a second implementation of the report's conclusion, and it
// disagreed with the first: one template read "None of your 1 templates", and an export whose
// workflows lose their approval gates led with a sentence that never mentioned them. A gate the move
// drops is said after what the templates show, never instead of it, and never left out.
func HeadlineOf(a Assessment) Headline {
	g := a.Governance
	gates := len(g.ApprovalGates)
	if g.Templates == 0 {
		switch gates {
		case 0:
			return Headline{Kind: "clear",
				Text: "This export holds no templates, so there is nothing here to gate."}
		case 1:
			return Headline{Kind: "gap", Text: gateSentence(gates) +
				" Write an approval policy before it is rebuilt, or it runs with no gate."}
		default:
			return Headline{Kind: "gap", Text: gateSentence(gates) +
				" Write an approval policy before they are rebuilt, or they run with no gate."}
		}
	}

	var h Headline
	switch {
	case g.WouldGate > 0:
		subject, runs, them := fmt.Sprintf("%d of your %d templates", g.WouldGate, g.Templates),
			"they run", "them"
		if g.WouldGate == 1 {
			runs, them = "it runs", "it"
		}
		if g.Templates == 1 {
			subject = "Your one template"
		}
		h = Headline{Kind: "gap", Text: subject + " can do something nobody can undo, and today " +
			runs + " whenever somebody presses the button. One approval policy holds " + them +
			" until a second person agrees."}
	case g.Templates == 1:
		h = Headline{Kind: "clear", Text: "Your one template does not grade irreversible. A policy " +
			"on risk or on tool is the one to write here, not one on reversibility."}
	default:
		h = Headline{Kind: "clear", Text: fmt.Sprintf("None of your %d templates grades "+
			"irreversible. A policy on risk or on tool is the one to write here, not one on "+
			"reversibility.", g.Templates)}
	}
	if gates > 0 {
		h.Kind = "gap"
		h.Text += " " + gateSentence(gates)
	}
	return h
}

// gateSentence says how many workflows wait on an approval node that the move does not carry.
func gateSentence(n int) string {
	if n == 1 {
		return "1 workflow waits for a person at an approval node today, and that gate does not " +
			"come across."
	}
	return fmt.Sprintf("%d workflows wait for a person at an approval node today, and those gates "+
		"do not come across.", n)
}
