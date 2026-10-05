package importer

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// awxEncrypted is the value AWX writes in place of every secret it exports.
const awxEncrypted = "$encrypted$"

// awxNotificationTemplate is an AWX notification template: a named channel and its configuration.
type awxNotificationTemplate struct {
	// Name is the template's name.
	Name string `json:"name"`
	// Description says what the template is for.
	Description string `json:"description"`
	// Organization is the organization the template belongs to, which scopes its name.
	Organization awxRef `json:"organization"`
	// NotificationType is the channel: email, slack, webhook, mattermost, rocketchat, pagerduty,
	// grafana, twilio, or irc.
	NotificationType string `json:"notification_type"`
	// NotificationConfiguration holds the channel's settings, with every secret replaced by AWX's
	// "$encrypted$".
	NotificationConfiguration map[string]any `json:"notification_configuration"`
	// Messages holds custom message templates, which do not carry across.
	Messages json.RawMessage `json:"messages"`
}

// awxNotifyRelated holds the notification templates an object is attached to, one list per event,
// as awxkit writes them under the object's related block.
type awxNotifyRelated struct {
	// Started are told when a job starts.
	Started []awxRef `json:"notification_templates_started"`
	// Success are told when a job succeeds.
	Success []awxRef `json:"notification_templates_success"`
	// Error are told when a job fails.
	Error []awxRef `json:"notification_templates_error"`
	// Approvals are told when a workflow waits on an approval node.
	Approvals []awxRef `json:"notification_templates_approvals"`
}

// events returns each attached reference with the event it is attached for.
func (r *awxNotifyRelated) events() []awxNotifyEvent {
	if r == nil {
		return nil
	}
	var out []awxNotifyEvent
	for _, list := range []struct {
		// refs are one event's references.
		refs []awxRef
		// event is the event they are attached for here.
		event string
	}{
		{r.Started, notification.EventStarted}, {r.Success, notification.EventSuccess},
		{r.Error, notification.EventFailure}, {r.Approvals, notification.EventApproval},
	} {
		for _, ref := range list.refs {
			out = append(out, awxNotifyEvent{ref: ref, event: list.event})
		}
	}
	return out
}

// awxNotifyEvent is one attachment AWX records: a notification template and the event it hears.
type awxNotifyEvent struct {
	// ref names the notification template.
	ref awxRef
	// event is started, success, failure, or approval.
	event string
}

// awxProjectRelated holds a project's nested related assets.
type awxProjectRelated struct {
	awxNotifyRelated
	// Schedules are project update schedules, which have no counterpart here since a run syncs its
	// project itself.
	Schedules json.RawMessage `json:"schedules"`
}

// awxNotifyState is what the notification import keeps between reading the templates and wiring
// the objects attached to them.
type awxNotifyState struct {
	// ids maps an AWX notification template to the first target it became.
	ids awxIDs
	// extra maps a target to further targets the same AWX template became, for a Twilio template
	// that texts several numbers, so each attachment reaches all of them.
	extra map[string][]string
	// templateOrg records the AWX organization each imported template belongs to, so an
	// organization's attachments reach the templates that were in it.
	templateOrg map[string]string
	// copies maps a template to the copies its overriding schedules fire, so an attachment on the
	// template reaches the runs those schedules start.
	copies map[string][]string
}

// awxNotify returns the plan's notification import state, creating it on first use.
func (p *Plan) awxNotify() *awxNotifyState {
	if p.notify == nil {
		p.notify = &awxNotifyState{ids: awxIDs{}, extra: map[string][]string{},
			templateOrg: map[string]string{}, copies: map[string][]string{}}
	}
	return p.notify
}

// addNotificationTemplates maps AWX notification templates into named targets. A secret AWX exports
// only as "$encrypted$" cannot come across, so a target that needs one arrives waiting for it and
// the report says which and what to enter, the same way a credential arrives as a shell.
func (p *Plan) addNotificationTemplates(templates []awxNotificationTemplate, now time.Time) {
	state := p.awxNotify()
	name := orgQualifier(awxOrgNames(templates, func(nt awxNotificationTemplate) (string, string) {
		return nt.Organization.Name, nt.Name
	})...)
	for _, nt := range templates {
		if strings.TrimSpace(nt.Name) == "" {
			p.warn("a notification template without a name was not imported")
			p.refused++
			continue
		}
		label := name(nt.Organization.Name, nt.Name)
		targets, ok := p.awxTargets(label, nt)
		if !ok {
			p.refused++
			continue
		}
		if len(nt.Messages) > 0 && string(nt.Messages) != "null" && string(nt.Messages) != "{}" {
			p.warn("notification template %q has custom message templates, which were not copied: "+
				"its channel is told in this system's own wording", label)
		}
		var ids []string
		for i, t := range targets {
			n := &notification.Notification{
				ID: notification.NewID(), Name: label, Description: nt.Description, Kind: t.target.Kind,
				To: t.target.To, URLHint: util.MaskURL(t.target.URL), KeySet: t.target.Key != "",
				NeedsSecret: t.needsSecret, CreatedAt: now,
			}
			if len(targets) > 1 {
				n.Name = label + " (" + t.target.To + ")"
			}
			// Every part the export carried is held for the apply to seal, including the parts of a
			// target still waiting for its secret, such as a Grafana instance's address, so
			// finishing it asks for the missing secret and nothing else.
			if t.target.URL != "" || t.target.Key != "" {
				if p.notifySecrets == nil {
					p.notifySecrets = map[string]run.NotifyTarget{}
				}
				p.notifySecrets[n.ID] = t.target
			}
			p.Notifications = append(p.Notifications, n)
			ids = append(ids, n.ID)
			if i == 0 && state.ids.set(nt.Organization.Name, nt.Name, n.ID) {
				p.warn("notification template %q appears more than once; the later one is what "+
					"objects naming it are attached to", label)
			}
		}
		state.extra[ids[0]] = ids[1:]
	}
}

// awxTarget is one target an AWX notification template becomes.
type awxTarget struct {
	// target is the channel configuration.
	target run.NotifyTarget
	// needsSecret reports that the target is missing a secret the export did not carry.
	needsSecret bool
}

// awxTargets converts one AWX notification template into the targets it becomes, reporting what
// does not carry across, and false when the template cannot come across at all.
func (p *Plan) awxTargets(label string, nt awxNotificationTemplate) ([]awxTarget, bool) {
	cfg := nt.NotificationConfiguration
	str := func(key string) string {
		v := strings.TrimSpace(jsonScalarString(cfg[key]))
		if v == awxEncrypted {
			return ""
		}
		return v
	}
	held := func(key string) bool {
		return strings.TrimSpace(jsonScalarString(cfg[key])) == awxEncrypted
	}
	kind := strings.ToLower(strings.TrimSpace(nt.NotificationType))
	switch kind {
	case "webhook":
		if headers := cfg["headers"]; headers != nil && jsonScalarString(headers) != "{}" ||
			str("username") != "" || held("password") {
			p.warn("notification template %q sends custom headers or basic credentials with its "+
				"webhook, which were not copied: a webhook here posts JSON with no extra headers",
				label)
		}
		if held("url") {
			return p.heldURLTarget(label, run.NotifyWebhook), true
		}
		return p.urlTarget(label, run.NotifyWebhook, str("url"))
	case "mattermost":
		if held("mattermost_url") {
			return p.heldURLTarget(label, run.NotifyMattermost), true
		}
		return p.urlTarget(label, run.NotifyMattermost, str("mattermost_url"))
	case "rocketchat":
		if held("rocketchat_url") {
			return p.heldURLTarget(label, run.NotifyRocketChat), true
		}
		return p.urlTarget(label, run.NotifyRocketChat, str("rocketchat_url"))
	case "slack":
		// AWX posts to Slack with a bot token and a channel list. A Slack target here posts to an
		// incoming webhook, which is one channel's address, so the target arrives waiting for it.
		p.warn("notification template %q posts to Slack with a bot token, to %s. A Slack target "+
			"here posts to an incoming webhook, so it needs its secret entered: the incoming "+
			"webhook address of the channel to post to", label, listOrNone(cfg["channels"]))
		return []awxTarget{{target: run.NotifyTarget{Kind: run.NotifySlack}, needsSecret: true}},
			true
	case "pagerduty":
		key := str("service_key")
		if key == "" {
			p.warn("notification template %q needs its secret entered: the PagerDuty Events API v2 "+
				"routing key, which the export does not carry", label)
			return []awxTarget{{target: run.NotifyTarget{Kind: run.NotifyPagerDuty},
				needsSecret: true}}, true
		}
		p.warn("notification template %q pages PagerDuty with the integration key AWX held; a "+
			"PagerDuty target here sends to the Events API v2, so check the key is a v2 routing key",
			label)
		return []awxTarget{{target: run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: key}}}, true
	case "grafana":
		url := str("grafana_url")
		if url == "" {
			p.warn("notification template %q names no Grafana address, so it was not imported", label)
			return nil, false
		}
		if key := str("grafana_key"); key != "" {
			return []awxTarget{{target: run.NotifyTarget{Kind: run.NotifyGrafana, URL: url,
				Key: key}}}, true
		}
		p.warn("notification template %q needs its secret entered: the Grafana API token, which "+
			"the export does not carry", label)
		return []awxTarget{{target: run.NotifyTarget{Kind: run.NotifyGrafana, URL: url},
			needsSecret: true}}, true
	case "email":
		to := strings.Join(stringList(cfg["recipients"]), ", ")
		if to == "" {
			p.warn("notification template %q names no recipients, so it was not imported", label)
			return nil, false
		}
		if host := str("host"); host != "" {
			p.warn("notification template %q sends through the SMTP server %s, which does not carry "+
				"across: mail here goes through the server's own transport, set with --smtp-addr",
				label, host)
		}
		return []awxTarget{{target: run.NotifyTarget{Kind: run.NotifyEmail, To: to}}}, true
	case "twilio":
		numbers := stringList(cfg["to_numbers"])
		if len(numbers) == 0 {
			p.warn("notification template %q names no numbers to text, so it was not imported", label)
			return nil, false
		}
		p.warn("notification template %q texts through a Twilio account of its own. A Twilio "+
			"target here texts through the server's account, set with --notify-twilio-sid, so that "+
			"account's credentials were not copied", label)
		out := make([]awxTarget, 0, len(numbers))
		for _, n := range numbers {
			out = append(out, awxTarget{target: run.NotifyTarget{Kind: run.NotifyTwilio, To: n}})
		}
		return out, true
	case "irc":
		p.warn("notification template %q posts to IRC, which has no equivalent here, so it was "+
			"not imported", label)
		return nil, false
	default:
		p.warn("notification template %q has the type %q, which this importer does not read, so it "+
			"was not imported", label, oneLine(nt.NotificationType))
		return nil, false
	}
}

// heldURLTarget is a target whose address the export held back as a secret: it comes across with
// everything else it has and waits for the address to be entered, the way a Slack target does.
func (p *Plan) heldURLTarget(label, kind string) []awxTarget {
	p.warn("notification template %q needs its secret entered: its address, which the export does "+
		"not carry", label)
	return []awxTarget{{target: run.NotifyTarget{Kind: kind}, needsSecret: true}}
}

// urlTarget is a target configured by one address, refused when the export names none.
func (p *Plan) urlTarget(label, kind, url string) ([]awxTarget, bool) {
	if url == "" {
		p.warn("notification template %q names no address, so it was not imported", label)
		return nil, false
	}
	if err := run.ValidateNotifyTarget(run.NotifyTarget{Kind: kind, URL: url}); err != nil {
		p.warn("notification template %q was not imported: %v", label, err)
		return nil, false
	}
	return []awxTarget{{target: run.NotifyTarget{Kind: kind, URL: url}}}, true
}

// stringList reads a JSON list of strings, or one string, into a list without blanks.
func stringList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if s := strings.TrimSpace(jsonScalarString(item)); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for part := range strings.SplitSeq(t, ",") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// listOrNone renders a configured list for a warning, or says it was empty.
func listOrNone(v any) string {
	list := stringList(v)
	if len(list) == 0 {
		return "no channel"
	}
	return oneLine(strings.Join(list, ", "))
}

// attachAWXNotifications attaches the targets an AWX object names to the template it imported as,
// and to every copy of it an overriding schedule fires.
func (p *Plan) attachAWXNotifications(owner string, related *awxNotifyRelated, templateID string,
	now time.Time) {
	state := p.awxNotify()
	objects := append([]string{templateID}, state.copies[templateID]...)
	for _, e := range related.events() {
		id, ok := state.ids.get(e.ref)
		if !ok {
			p.warn("%s is attached in AWX to notification template %s, which did not come "+
				"across, so that attachment was not imported", owner, state.ids.unresolved(e.ref))
			continue
		}
		for _, target := range append([]string{id}, state.extra[id]...) {
			for _, object := range objects {
				p.attach(target, notification.KindTemplate, object, e.event, now)
			}
		}
	}
}

// attach adds one attachment to the plan unless the same one is already there, which an
// organization's attachment repeating a template's own produces.
func (p *Plan) attach(target, kind, object, event string, now time.Time) {
	for _, a := range p.Attachments {
		if a.NotificationID == target && a.ObjectKind == kind && a.ObjectID == object &&
			a.Event == event {
			return
		}
	}
	p.Attachments = append(p.Attachments, &notification.Attachment{
		ID: notification.NewAttachmentID(), NotificationID: target, ObjectKind: kind,
		ObjectID: object, Event: event, CreatedAt: now,
	})
}

// reportProjectNotifications names the notification templates attached to projects, which do not
// come across: AWX tells them about project updates, and a run here syncs its project itself, so
// there is no project update to hear about.
func (p *Plan) reportProjectNotifications(projects []awxProject) {
	for _, pr := range projects {
		if pr.Related == nil || len(pr.Related.events()) == 0 {
			continue
		}
		p.warn("project %q has notification templates attached in AWX, which hear about project "+
			"updates. A run here syncs its project itself, so those attachments were not imported; "+
			"attach the targets to the templates that use the project instead", pr.Name)
	}
}
