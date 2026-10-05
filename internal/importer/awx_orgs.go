package importer

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
)

// strictGrantsNote is what every placement statement says about access, since placing an object in
// an organization changes who may see it on an install that runs with --strict-grants.
const strictGrantsNote = "Under --strict-grants an object in an organization is visible only to " +
	"that organization's members, and AWX memberships are not imported, so grant access in it to " +
	"everyone who needs these objects"

// awxOrg returns the organization the import plans for the AWX organization named name, planning it
// the first time anything asks for it.
//
// Every part of the import that places an object in its AWX organization asks here: a smart
// inventory and the inventories it filters, and an organization's own notification attachments with
// the templates they cover. So one AWX organization becomes one organization however many reasons
// bring it across, and every rule about it, matching by the AWX id an export references it by,
// reusing an existing organization only when exactly one has its name, and falling back on any
// ambiguity, is applied once, in resolveAWXOrgs, to everything placed in it.
func (p *Plan) awxOrg(name string, now time.Time) *org.Org {
	if p.awxOrgByName == nil {
		p.awxOrgByName = map[string]*org.Org{}
	}
	if o, ok := p.awxOrgByName[name]; ok {
		return o
	}
	o := &org.Org{ID: org.NewID(), Name: name, CreatedAt: now}
	p.awxOrgByName[name] = o
	p.Orgs = append(p.Orgs, o)
	p.nameOrg(o.ID, name)
	return o
}

// nameOrg records the name of an organization something in the plan is placed in, planned or
// already held, so the plan can say where each object landed.
func (p *Plan) nameOrg(id, name string) {
	if p.orgNames == nil {
		p.orgNames = map[string]string{}
	}
	p.orgNames[id] = name
}

// orgNotifyFor returns the organization notification plan whose attachments sit on the organization
// with id, or nil.
func (p *Plan) orgNotifyFor(id string) *orgNotifyPlan {
	for _, plan := range p.orgNotify {
		if plan.orgID == id {
			return plan
		}
	}
	return nil
}

// resolveAWXOrgs settles, against the organizations the install already holds, where each
// organization the import planned lands, before anything is written.
//
// An organization this install holds exactly one of by name is used rather than created again, and
// everything placed in the planned one moves to it. With more than one of the name the import does
// not guess between them: what was placed in it arrives with no organization, its notification
// attachments fall back to its templates, and the warnings say so. Without an organization store
// nothing can be created or matched, which falls back the same way. The organizations still planned
// afterward are the ones applyOrgs creates.
func (p *Plan) resolveAWXOrgs(ctx context.Context, store org.Store) error {
	if len(p.Orgs) == 0 {
		return nil
	}
	byName := map[string][]string{}
	if store != nil {
		held, err := store.List(ctx)
		if err != nil {
			return fmt.Errorf("read organizations to place what the import brings: %w", err)
		}
		for _, o := range held {
			byName[o.Name] = append(byName[o.Name], o.ID)
		}
	}
	now := time.Now()
	for _, o := range slices.Clone(p.Orgs) {
		plan := p.orgNotifyFor(o.ID)
		matches := byName[o.Name]
		switch {
		case store == nil:
			p.unplaceOrg(o, plan, "organizations are not enabled on this install", now)
		case len(matches) > 1:
			p.unplaceOrg(o, plan, fmt.Sprintf("%d organizations here are named %s and the import "+
				"does not guess between them", len(matches), quoteName(o.Name)), now)
		case len(matches) == 1:
			p.adoptOrg(o, matches[0], plan)
		case plan != nil:
			p.warn("%s", plan.outcome.Line())
		}
	}
	return nil
}

// adoptOrg moves everything the plan placed in the planned organization o to the organization the
// install already holds under its name, existing, and drops o, which is then not created.
func (p *Plan) adoptOrg(o *org.Org, existing string, plan *orgNotifyPlan) {
	p.Orgs = slices.DeleteFunc(p.Orgs, func(x *org.Org) bool { return x.ID == o.ID })
	p.nameOrg(existing, o.Name)
	inventories := 0
	for _, inv := range p.Inventories {
		if inv.OrgID == o.ID {
			inv.OrgID = existing
			inventories++
		}
	}
	for _, t := range p.Templates {
		if t.OrgID == o.ID {
			t.OrgID = existing
		}
	}
	for _, a := range p.Attachments {
		if a.ObjectKind == notification.KindOrg && a.ObjectID == o.ID {
			a.ObjectID = existing
		}
	}
	if inventories > 0 {
		p.warn("organization %q already exists here, so the inventories imported from that AWX "+
			"organization were placed in it. Its smart inventories also read any inventory that "+
			"organization already held, so review their reach", oneLine(o.Name))
	}
	if plan != nil {
		plan.orgID = existing
		plan.outcome.Outcome, plan.outcome.OrgID = OrgNotifyMatched, existing
		p.warn("%s", plan.outcome.Line())
	}
}

// unplaceOrg takes everything the plan placed in organization o back out of it, because o cannot be
// resolved, and drops o. A smart inventory among what was placed says why, since that is the
// inventory whose reach changes, and the organization's notification attachments fall back to its
// templates.
func (p *Plan) unplaceOrg(o *org.Org, plan *orgNotifyPlan, why string, now time.Time) {
	for _, inv := range p.Inventories {
		if inv.OrgID != o.ID {
			continue
		}
		inv.OrgID = ""
		if inv.Kind == inventory.KindSmart {
			p.warn(smartUnplacedWarning+". It arrives with no organization because %s", inv.Name,
				oneLine(o.Name), why)
		}
	}
	if plan != nil {
		p.fallBack(plan, why, now)
	}
	for _, t := range p.Templates {
		if t.OrgID == o.ID {
			t.OrgID = ""
		}
	}
	p.dropOrg(o.ID)
}

// reportAWXPlacement states, for each organization the import plans, which templates and
// inventories it places there, and what that does to access, so the placement is read before the
// import is applied rather than discovered as a permission problem afterward.
func (p *Plan) reportAWXPlacement() {
	for _, o := range p.Orgs {
		var templates, inventories []string
		for _, t := range p.Templates {
			if t.OrgID == o.ID {
				templates = append(templates, t.Name)
			}
		}
		for _, inv := range p.Inventories {
			if inv.OrgID == o.ID {
				inventories = append(inventories, inv.Name)
			}
		}
		if len(templates)+len(inventories) == 0 {
			continue
		}
		p.warn("organization %s: the import places %s in it. It is created here unless exactly one "+
			"organization of that name already exists, which is used instead. %s",
			quoteName(o.Name), placedList(templates, inventories), strictGrantsNote)
	}
}

// placedList names the templates and inventories placed in an organization, sorted, with a count
// for each.
func placedList(templates, inventories []string) string {
	var parts []string
	if n := len(templates); n > 0 {
		sort.Strings(templates)
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n, "template"+plural(n),
			strings.Join(templates, ", ")))
	}
	if n := len(inventories); n > 0 {
		sort.Strings(inventories)
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n, inventoryNoun(n),
			strings.Join(inventories, ", ")))
	}
	return strings.Join(parts, " and ")
}

// TemplateOrganizations maps the name of each template the plan places in an organization to that
// organization's name, so a preview and the result of an import can say where each one landed.
func (p *Plan) TemplateOrganizations() map[string]string {
	out := map[string]string{}
	for _, t := range p.Templates {
		if name, ok := p.orgNames[t.OrgID]; ok && t.OrgID != "" {
			out[t.Name] = name
		}
	}
	return out
}

// InventoryOrganizations maps the name of each inventory the plan places in an organization to that
// organization's name, so a preview and the result of an import can say where each one landed.
func (p *Plan) InventoryOrganizations() map[string]string {
	out := map[string]string{}
	for _, inv := range p.Inventories {
		if name, ok := p.orgNames[inv.OrgID]; ok && inv.OrgID != "" {
			out[inv.Name] = name
		}
	}
	return out
}
