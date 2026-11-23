package importer

import (
	"slices"
	"strings"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// SkipOversizedInventories takes every planned inventory whose rendered content is past
// inventory.MaxContentBytes out of the plan, with everything that names one: a composed inventory
// built on it, the dynamic source that maintains it, each template and workflow that runs against
// it, the schedules and notification attachments of those, and their callback bindings. Each
// skipped inventory gets one warning naming its size, the limit, what to do instead, and what was
// skipped with it. The rest of the plan applies.
//
// The API refuses to store content past the limit, so an import that stored it would leave an
// inventory nobody could save again, and skipping the inventory alone would leave templates and
// schedules naming one that does not exist. It is safe to call more than once.
func (p *Plan) SkipOversizedInventories() {
	root := map[string]string{}
	names := map[string]string{}
	sizes := map[string]int{}
	for _, inv := range p.Inventories {
		if len(inv.Content) > inventory.MaxContentBytes {
			root[inv.ID] = inv.ID
			names[inv.Name] = inv.ID
			sizes[inv.ID] = len(inv.Content)
		}
	}
	if len(root) == 0 {
		return
	}
	// A composed inventory resolves its inputs at every launch, so one built on a skipped input is
	// skipped with it, and so is one built on that.
	for changed := true; changed; {
		changed = false
		for _, inv := range p.Inventories {
			if _, done := root[inv.ID]; done {
				continue
			}
			for _, in := range inv.InputIDs {
				if r, ok := root[in]; ok {
					root[inv.ID] = r
					changed = true
					break
				}
			}
		}
	}
	with := map[string][]string{}
	note := func(rootID, what string) {
		with[rootID] = append(with[rootID], what)
		p.refused++
	}
	byID := map[string]string{}
	for _, inv := range p.Inventories {
		byID[inv.ID] = inv.Name
	}
	p.Inventories = slices.DeleteFunc(p.Inventories, func(inv *inventory.Inventory) bool {
		r, ok := root[inv.ID]
		if ok && r != inv.ID {
			note(r, "inventory "+quoteName(inv.Name))
		}
		return ok
	})
	p.Sources = slices.DeleteFunc(p.Sources, func(s *invsource.Source) bool {
		r, ok := root[s.InventoryID]
		if ok {
			note(r, "inventory source "+quoteName(s.Name))
		}
		return ok
	})
	templates := map[string]string{}
	for _, t := range p.Templates {
		r, ok := root[t.InventoryID]
		if !ok && t.InventoryID == "" && t.Inventory != "" {
			r, ok = names[t.Inventory]
		}
		if ok {
			templates[t.ID] = r
		}
	}
	p.Templates = slices.DeleteFunc(p.Templates, func(t *template.Template) bool {
		r, ok := templates[t.ID]
		if ok {
			kind := "template "
			if len(t.Steps) > 0 {
				kind = "workflow "
			}
			note(r, kind+quoteName(t.Name))
		}
		return ok
	})
	p.Schedules = slices.DeleteFunc(p.Schedules, func(s *schedule.Schedule) bool {
		r, ok := templates[s.TemplateID]
		if !ok && s.Inventory != "" {
			r, ok = names[s.Inventory]
		}
		if ok {
			note(r, "schedule "+quoteName(s.Name))
		}
		return ok
	})
	p.Attachments = slices.DeleteFunc(p.Attachments, func(a *notification.Attachment) bool {
		_, ok := templates[a.ObjectID]
		return ok
	})
	p.awxBindings = slices.DeleteFunc(p.awxBindings, func(b template.AWXBinding) bool {
		_, ok := templates[b.TemplateID]
		return ok
	})
	for id := range templates {
		delete(p.callbackKeys, id)
	}
	for id, r := range root {
		if id != r {
			continue
		}
		msg := "Inventory " + quoteName(byID[id]) + " renders to " + inventory.FormatMiB(sizes[id]) +
			", past the " + inventory.FormatMiB(inventory.MaxContentBytes) + " limit, so it is not " +
			"imported. Keep it as a dynamic inventory file in a project, or import it without the " +
			"synced host variables."
		if deps := with[id]; len(deps) > 0 {
			slices.Sort(deps)
			msg += " Skipped with it, since each names it: " + strings.Join(deps, ", ") + "."
		}
		p.refused++
		p.warn("%s", msg)
	}
}
