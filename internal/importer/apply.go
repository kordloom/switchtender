package importer

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// ApplyStores are the stores an import plan is written to.
type ApplyStores struct {
	// Projects receives the imported git projects.
	Projects project.Store
	// Inventories receives the imported stored inventories, including each dynamic source's backing
	// inventory.
	Inventories inventory.Store
	// Sources receives the imported dynamic inventory sources.
	Sources invsource.Store
	// Credentials receives the imported credential shells.
	Credentials credential.Store
	// Templates receives the imported job templates.
	Templates template.Store
	// Schedules receives the imported schedules.
	Schedules schedule.Store
}

// Apply persists the plan through the stores in dependency order and returns the number of objects
// created. Credentials arrive as shells, so their secrets must be set before templates that need
// them run. It stops at the first store error, returning what was created so far.
func (p *Plan) Apply(ctx context.Context, s ApplyStores) (int, error) {
	// Refuse before writing anything if the plan has dynamic sources but nowhere to store them, so a
	// source's backing inventory is never created without its source.
	if len(p.Sources) > 0 && s.Sources == nil {
		return 0, fmt.Errorf("cannot import %d inventory sources: inventory sources not enabled",
			len(p.Sources))
	}
	// The inventory an operator named on the command line is resolved here, against the inventories
	// this install actually holds, because the mapping stage has no store to ask. A crontab and a
	// Rundeck export name no inventory file, so the caller supplies one, and the obvious thing to type
	// is the name of a stored inventory. That value used to be written straight into the field that
	// holds a filesystem path, so the imported templates pointed at a file that does not exist and the
	// operator found out when one launched.
	if err := p.resolveInventoryNames(ctx, s.Inventories); err != nil {
		return 0, err
	}
	created := 0
	for _, pr := range p.Projects {
		if err := s.Projects.Save(ctx, pr); err != nil {
			return created, fmt.Errorf("save project %q: %w", pr.Name, err)
		}
		created++
	}
	for _, inv := range p.Inventories {
		if err := s.Inventories.Save(ctx, inv); err != nil {
			return created, fmt.Errorf("save inventory %q: %w", inv.Name, err)
		}
		created++
	}
	for _, c := range p.Credentials {
		if err := s.Credentials.Save(ctx, c); err != nil {
			return created, fmt.Errorf("save credential %q: %w", c.Name, err)
		}
		created++
	}
	for _, src := range p.Sources {
		if err := s.Sources.Save(ctx, src); err != nil {
			return created, fmt.Errorf("save inventory source %q: %w", src.Name, err)
		}
		created++
	}
	for _, t := range p.Templates {
		if err := s.Templates.Save(ctx, t); err != nil {
			return created, fmt.Errorf("save template %q: %w", t.Name, err)
		}
		created++
	}
	for _, sc := range p.Schedules {
		if err := s.Schedules.Save(ctx, sc); err != nil {
			return created, fmt.Errorf("save schedule %q: %w", sc.Name, err)
		}
		created++
	}
	return created, nil
}

// resolveInventoryNames rewrites a template's inventory path into an inventory id when the path names
// a stored inventory. Anything that matches nothing is left as a path and reported, so an operator who
// meant a file gets a file and an operator who meant a stored inventory gets the object.
//
// It runs before anything is written, and only for templates that carry a path and no id, so an import
// that already wired an inventory by id is untouched.
func (p *Plan) resolveInventoryNames(ctx context.Context, store inventory.Store) error {
	needed := map[string]bool{}
	for _, t := range p.Templates {
		if t.InventoryID == "" && t.Inventory != "" {
			needed[t.Inventory] = true
		}
	}
	if len(needed) == 0 || store == nil {
		return nil
	}
	// The plan's own inventories are not stored yet, so both they and the existing ones are consulted.
	byName := map[string]string{}
	for _, inv := range p.Inventories {
		byName[inv.Name] = inv.ID
	}
	stored, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("read inventories to resolve the named one: %w", err)
	}
	for _, inv := range stored {
		byName[inv.Name] = inv.ID
	}
	reported := map[string]bool{}
	for _, t := range p.Templates {
		if t.InventoryID != "" || t.Inventory == "" {
			continue
		}
		if id, ok := byName[t.Inventory]; ok {
			t.InventoryID = id
			t.Inventory = ""
			continue
		}
		if !reported[t.Inventory] {
			reported[t.Inventory] = true
			p.warn("no stored inventory is named %q, so it is used as a path on the server's "+
				"filesystem. Create an inventory with that name, or point the templates at one, if "+
				"that is not what you meant.", t.Inventory)
		}
	}
	return nil
}
