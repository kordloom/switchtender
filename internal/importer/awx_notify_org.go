package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
)

// What became of an AWX organization's own notification attachments, as the import report states
// it.
const (
	// OrgNotifyImported is an organization the import created here, from the same export, with the
	// attachments on it.
	OrgNotifyImported = "imported"
	// OrgNotifyMatched is an organization the install already held under the same name, the only
	// one of that name, with the attachments on it.
	OrgNotifyMatched = "matched existing"
	// OrgNotifyFellBack is an organization that could not be resolved, whose attachments were made
	// to each template imported from it instead.
	OrgNotifyFellBack = "fell back"
	// OrgNotifyUnresolved is an organization that could not be resolved and had no template to fall
	// back to, so its attachments did not come across.
	OrgNotifyUnresolved = "unresolved"
)

// OrgNotification is what the import did with one AWX organization's own notification attachments.
type OrgNotification struct {
	// Organization is the AWX organization's name.
	Organization string `json:"organization"`
	// Outcome is imported, matched existing, fell back, or unresolved.
	Outcome string `json:"outcome"`
	// OrgID is the organization the attachments are on, for imported and matched existing.
	OrgID string `json:"org_id,omitempty"`
	// Templates is how many templates the attachments fell back to.
	Templates int `json:"templates,omitempty"`
	// Attachments is how many attachments the import made for the organization's list.
	Attachments int `json:"attachments"`
	// Reason says why an organization fell back or was not resolved.
	Reason string `json:"reason,omitempty"`
}

// Line states the outcome in a sentence for the import report.
func (o OrgNotification) Line() string {
	name := quoteName(o.Organization)
	switch o.Outcome {
	case OrgNotifyImported:
		return fmt.Sprintf("organization %s has notification templates attached in AWX: imported, "+
			"so its %d %s are on the organization, which the import created here from the same "+
			"export with its templates placed in it, and a template created in it later is "+
			"covered too", name,
			o.Attachments, "attachment"+plural(o.Attachments))
	case OrgNotifyMatched:
		return fmt.Sprintf("organization %s has notification templates attached in AWX: matched "+
			"existing, so its %d %s are on the organization this install already holds under that "+
			"name", name, o.Attachments, "attachment"+plural(o.Attachments))
	case OrgNotifyFellBack:
		return fmt.Sprintf("organization %s has notification templates attached in AWX: fell back "+
			"to %d %s, each attached on its own, because %s. A template created here later is not "+
			"covered until it is attached as well", name, o.Templates,
			"template"+plural(o.Templates), o.Reason)
	}
	return fmt.Sprintf("organization %s has notification templates attached in AWX: unresolved, "+
		"so those attachments were not imported, because %s", name, o.Reason)
}

// awxOrgRecord is the part of an AWX organization the organization notification import reads.
type awxOrgRecord struct {
	// ID is the organization's AWX id, when the export carries one, which REST-shaped references
	// name it by.
	ID json.RawMessage `json:"id"`
	// Name is the organization's name.
	Name string `json:"name"`
	// Related carries the notification templates attached to the organization itself.
	Related *awxNotifyRelated `json:"related"`
}

// key returns the spelling an awxRef gives a reference to this organization by its AWX id, id-N,
// or the empty string when the export carries no id.
func (o awxOrgRecord) key() string {
	raw := bytes.TrimSpace(o.ID)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil || n.String() == "" {
		return ""
	}
	return "id-" + n.String()
}

// orgAttachment is one notification target an organization's list names, for one event.
type orgAttachment struct {
	// target is the imported target.
	target string
	// event is the event it is attached for.
	event string
}

// orgNotifyPlan is one AWX organization's own notification attachments, carried from the mapping to
// the apply, which is the first point the install's existing organizations can be read.
type orgNotifyPlan struct {
	// name is the AWX organization's name.
	name string
	// orgID is the organization the attachments are on: the one the import creates until the
	// apply finds one to match or has to fall back.
	orgID string
	// attachments are the targets and events the organization's list names.
	attachments []orgAttachment
	// members are the templates imported from the organization, the copies overriding schedules
	// fire included.
	members []string
	// outcome is what the plan does with the attachments so far.
	outcome OrgNotification
}

// attachAWXOrganizations carries each AWX organization's own notification attachments to the
// organization they belong on. An organization in the same export, found by its AWX id and name, is
// the one awxOrg plans, shared with anything else the import places there, and the attachments go
// on it, with the templates imported from it placed in it, so a target attached to it hears about
// every run of those templates and of any template created in it later, as in AWX. When the apply
// finds the install already holds an organization of that name it uses that one instead of creating
// a second. Only an organization that cannot be resolved falls back to attaching each of its
// templates, and never both.
func (p *Plan) attachAWXOrganizations(raw []json.RawMessage, now time.Time) {
	state := p.awxNotify()
	for _, r := range raw {
		var o awxOrgRecord
		if err := json.Unmarshal(r, &o); err != nil || o.Related == nil {
			continue
		}
		var attachments []orgAttachment
		for _, e := range o.Related.events() {
			id, ok := state.ids.get(e.ref)
			if !ok {
				p.warn("organization %s is attached in AWX to notification template %s, which "+
					"did not come across, so that attachment was not imported", quoteName(o.Name),
					state.ids.unresolved(e.ref))
				continue
			}
			for _, target := range append([]string{id}, state.extra[id]...) {
				attachments = append(attachments, orgAttachment{target: target, event: e.event})
			}
		}
		if len(attachments) == 0 {
			continue
		}
		plan := &orgNotifyPlan{name: o.Name, attachments: attachments,
			members: p.orgMembers(o)}
		p.orgNotify = append(p.orgNotify, plan)
		if strings.TrimSpace(o.Name) == "" {
			p.fallBack(plan, "the export does not name the organization", now)
			continue
		}
		placed := p.awxOrg(o.Name, now)
		plan.orgID = placed.ID
		for _, a := range attachments {
			p.attach(a.target, notification.KindOrg, placed.ID, a.event, now)
		}
		p.placeMembers(plan, "", placed.ID)
		plan.outcome = OrgNotification{Organization: o.Name, Outcome: OrgNotifyImported,
			OrgID: placed.ID, Attachments: p.orgAttachmentCount(placed.ID)}
		p.warn("organization %s has notification templates attached in AWX: its %d %s go on that "+
			"organization, with the %d %s imported from it. The report from the apply states "+
			"whether the organization was imported, matched to the one this install already holds "+
			"under that name, or could not be resolved, in which case the attachments fall back "+
			"to those templates", quoteName(o.Name), plan.outcome.Attachments,
			"attachment"+plural(plan.outcome.Attachments), len(plan.members),
			"template"+plural(len(plan.members)))
	}
}

// orgMembers returns the templates imported from an AWX organization, matched by its AWX id or its
// name the way a template's own reference names it, with the copies their overriding schedules
// fire, sorted.
func (p *Plan) orgMembers(o awxOrgRecord) []string {
	state := p.awxNotify()
	key := o.key()
	var members []string
	for id, ref := range state.templateOrg {
		if ref == "" || (ref != o.Name && ref != key) {
			continue
		}
		members = append(members, id)
		members = append(members, state.copies[id]...)
	}
	slices.Sort(members)
	return slices.Compact(members)
}

// placeMembers moves the organization's imported templates from one owning organization to another,
// leaving a template the import placed somewhere else alone.
func (p *Plan) placeMembers(plan *orgNotifyPlan, from, to string) {
	for _, t := range p.Templates {
		if slices.Contains(plan.members, t.ID) && t.OrgID == from {
			t.OrgID = to
		}
	}
}

// orgAttachmentCount counts the plan's attachments on one organization.
func (p *Plan) orgAttachmentCount(orgID string) int {
	n := 0
	for _, a := range p.Attachments {
		if a.ObjectKind == notification.KindOrg && a.ObjectID == orgID {
			n++
		}
	}
	return n
}

// fallBack attaches an unresolved organization's list to each template imported from it, or,
// when none came across, records that the attachments could not be imported. why says what could
// not be resolved.
func (p *Plan) fallBack(plan *orgNotifyPlan, why string, now time.Time) {
	if plan.orgID != "" {
		p.dropOrg(plan.orgID)
		p.placeMembers(plan, plan.orgID, "")
		plan.orgID = ""
	}
	before := len(p.Attachments)
	for _, member := range plan.members {
		for _, a := range plan.attachments {
			p.attach(a.target, notification.KindTemplate, member, a.event, now)
		}
	}
	plan.outcome = OrgNotification{Organization: plan.name, Outcome: OrgNotifyFellBack,
		Templates: len(plan.members), Attachments: len(p.Attachments) - before, Reason: why}
	if len(plan.members) == 0 {
		plan.outcome = OrgNotification{Organization: plan.name, Outcome: OrgNotifyUnresolved,
			Reason: why + ", and none of its templates came across to attach them to instead"}
	}
	p.warn("%s", plan.outcome.Line())
}

// dropOrg takes a planned organization and the attachments on it out of the plan.
func (p *Plan) dropOrg(orgID string) {
	p.Orgs = slices.DeleteFunc(p.Orgs, func(o *org.Org) bool { return o.ID == orgID })
	p.Attachments = slices.DeleteFunc(p.Attachments, func(a *notification.Attachment) bool {
		return a.ObjectKind == notification.KindOrg && a.ObjectID == orgID
	})
}

// orgNotifications returns what became of every AWX organization's own notification attachments, in
// export order. Before an apply an imported outcome is the plan's intent, which the apply may turn
// into matched existing or a fall back.
func (p *Plan) orgNotifications() []OrgNotification {
	out := make([]OrgNotification, 0, len(p.orgNotify))
	for _, plan := range p.orgNotify {
		out = append(out, plan.outcome)
	}
	return out
}

// applyOrgs stores the organizations the plan creates and returns how many it created.
func (p *Plan) applyOrgs(ctx context.Context, store org.Store) (int, error) {
	if len(p.Orgs) == 0 {
		return 0, nil
	}
	if store == nil {
		return 0, fmt.Errorf("cannot import %d organizations: organizations not enabled",
			len(p.Orgs))
	}
	for i, o := range p.Orgs {
		if err := store.Save(ctx, o); err != nil {
			return i, fmt.Errorf("save organization %q: %w", o.Name, err)
		}
	}
	return len(p.Orgs), nil
}
