package dispatch

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// The webhook events for the two notices a run's status cannot express on its own.
const (
	// eventStepAwaiting is a workflow waiting at one of its approval steps, distinct from a whole run
	// held for approval so a receiver can tell which decision is being asked for.
	eventStepAwaiting = "workflow.step_awaiting_approval"
	// eventNeedsAttention is a run that has needed attention longer than its alert threshold.
	eventNeedsAttention = "run.needs_attention"
)

// notifyAttention tells the channels about work that has needed attention past its alert threshold,
// through the same channel deliveries a hold takes. An alert is a problem report rather than a
// request for a decision, so it is emailed whatever --notify-on says. The run's own targets hear it
// on the chat, webhook, ntfy, and email kinds a hold reaches, and the named targets attached for
// the attention event hear it on every kind, a pager included, since attaching one for that event
// is the request to be paged. The server-wide pager, dashboard, and text message channels report
// failed runs and stay out of it.
func (d *Dispatcher) notifyAttention(r *run.Run) {
	d.notifyWebhooks(r)
	d.notifySlack(r)
	d.notifyMattermost(r)
	d.notifyRocketChat(r)
	d.notifyDiscord(r)
	d.notifyTeams(r)
	d.notifyNtfy(r)
	d.notifyEmail(r)
	var own []run.NotifyTarget
	for _, t := range r.Notifications {
		switch t.Kind {
		case run.NotifyPagerDuty, run.NotifyGrafana, run.NotifyTwilio:
		default:
			own = append(own, t)
		}
	}
	d.deliverTargets(r, own)
	d.notifyNamed(r)
}

// stepSentences is what a notification about a workflow waiting at an approval step says after each
// channel's own rendering of the workflow's label: the step, what it asks, what each answer runs
// next, and who asked for the workflow.
func stepSentences(r *run.Run) string {
	s := r.AwaitingStep
	msg := "is waiting at approval step \"" + s.Name + "\"."
	if s.Description != "" {
		msg += " " + strings.TrimSpace(s.Description)
		if !strings.HasSuffix(msg, ".") && !strings.HasSuffix(msg, "?") {
			msg += "."
		}
	}
	msg += " " + stepNextSentence(s)
	if r.Actor != "" {
		msg += " Requested by " + r.Actor + "."
	}
	return msg
}

// stepNextSentence says what each answer to an approval step runs next.
func stepNextSentence(s *run.AwaitingStep) string {
	approve := "nothing further"
	if len(s.OnApprove) > 0 {
		approve = strings.Join(s.OnApprove, ", ")
	}
	deny := "nothing, and the workflow fails"
	if len(s.OnDeny) > 0 {
		deny = strings.Join(s.OnDeny, ", ")
	}
	msg := "Approving runs " + approve + ". Denying runs " + deny + "."
	if s.ExpiresAt != nil {
		msg += " Unanswered by " + s.ExpiresAt.UTC().Format(time.RFC3339) + ", it takes the deny path."
	}
	return msg
}

// attentionSentences is what an attention alert says in a chat channel: the alert in one line, then
// who can act and what happens next.
func attentionSentences(r *run.Run) string {
	a := r.Attention
	msg := a.Summary
	if a.WhoCanAct != "" {
		msg += " " + a.WhoCanAct
	}
	if a.Next != "" {
		msg += " " + a.Next
	}
	return msg
}

// attentionCard renders an attention alert as a Teams card's title, color, and facts.
func attentionCard(r *run.Run) (string, string, []map[string]string) {
	a := r.Attention
	facts := []map[string]string{{"title": "Alert", "value": a.Summary}}
	if a.Reason != "" {
		facts = append(facts, map[string]string{"title": "Why", "value": a.Reason})
	}
	if a.WhoCanAct != "" {
		facts = append(facts, map[string]string{"title": "Who can act", "value": a.WhoCanAct})
	}
	if a.Next != "" {
		facts = append(facts, map[string]string{"title": "Next", "value": a.Next})
	}
	return "SwitchTender alert: " + runLabel(r) + " needs attention", "Attention", facts
}

// stepCard renders a workflow waiting at an approval step as a Teams card's title, color, and
// facts.
func stepCard(r *run.Run) (string, string, []map[string]string) {
	s := r.AwaitingStep
	facts := []map[string]string{{"title": "Approval step", "value": s.Name}}
	if s.Description != "" {
		facts = append(facts, map[string]string{"title": "Asks", "value": s.Description})
	}
	approve, deny := "Nothing further", "Nothing, and the workflow fails"
	if len(s.OnApprove) > 0 {
		approve = strings.Join(s.OnApprove, ", ")
	}
	if len(s.OnDeny) > 0 {
		deny = strings.Join(s.OnDeny, ", ")
	}
	facts = append(facts, map[string]string{"title": "Approving runs", "value": approve},
		map[string]string{"title": "Denying runs", "value": deny})
	if s.ExpiresAt != nil {
		facts = append(facts, map[string]string{"title": "Takes the deny path at",
			"value": s.ExpiresAt.UTC().Format(time.RFC3339)})
	}
	if r.Actor != "" {
		facts = append(facts, map[string]string{"title": "Requested by", "value": r.Actor})
	}
	return "SwitchTender workflow " + runLabel(r) + " is waiting at approval step " + s.Name,
		"Warning", facts
}

// writeAttentionEmail writes the plain-text body of an attention alert's email.
func writeAttentionEmail(b *strings.Builder, r *run.Run) {
	a := r.Attention
	b.WriteString(a.Summary + "\n\n")
	b.WriteString("Run: " + r.ID + " (" + runLabel(r) + ")\n")
	if a.ScheduleID != "" {
		b.WriteString("Schedule: " + a.ScheduleName + " (" + a.ScheduleID + ")\n")
	}
	if a.Reason != "" {
		b.WriteString("Why: " + a.Reason + "\n")
	}
	if a.WhoCanAct != "" {
		b.WriteString("Who can act: " + a.WhoCanAct + "\n")
	}
	if a.Next != "" {
		b.WriteString("Next: " + a.Next + "\n")
	}
	if a.Path != "" {
		b.WriteString("\nSee everything that needs attention at " + a.Path + " on the server.\n")
	}
}

// writeStepEmail writes the plain-text body of a workflow waiting at an approval step: the step,
// what it asks, what each answer runs next, and how to decide it.
func writeStepEmail(b *strings.Builder, r *run.Run) {
	s := r.AwaitingStep
	b.WriteString("Workflow " + r.ID + " (" + runLabel(r) + ") is waiting at approval step \"" +
		s.Name + "\".\n\n")
	if s.Description != "" {
		b.WriteString("It asks: " + s.Description + "\n")
	}
	b.WriteString(stepNextSentence(s) + "\n")
	if r.Actor != "" {
		b.WriteString("Requested by: " + r.Actor + "\n")
	}
	b.WriteString("\nDecide it from the workflow's page, or with POST /v1/runs/" + s.ID +
		"/approve or POST /v1/runs/" + s.ID + "/reject under an admin token.\n")
}

// awaitingStep describes the approval step a workflow waits at, for its notification. What each
// answer runs next is read from the workflow's graph and its step records, the way the approval
// list reads it. A step whose state cannot be rebuilt is still announced, without them, since a
// person who is never told cannot decide at all.
func (d *Dispatcher) awaitingStep(parent, node *run.Run) *run.AwaitingStep {
	s := &run.AwaitingStep{ID: node.ID, Name: node.StepName}
	if node.StepIndex != nil && *node.StepIndex >= 0 && *node.StepIndex < len(parent.Steps) {
		s.Description = parent.Steps[*node.StepIndex].Description
	}
	if node.Timeout > 0 {
		at := node.CreatedAt.Add(time.Duration(node.Timeout) * time.Second)
		s.ExpiresAt = &at
	}
	state, err := outcome.StepStateOf(context.Background(), d.store, parent, node)
	if err != nil {
		d.log.Warn("dispatch: read what an approval step releases: "+err.Error(),
			zap.String("run_id", node.ID))
		return s
	}
	s.OnApprove = append([]string(nil), state.OnApprove...)
	s.OnDeny = append([]string(nil), state.OnDeny...)
	return s
}
