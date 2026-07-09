package importer

import (
	"context"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/template"
)

// ApplyStores are the stores an import plan is written to.
type ApplyStores struct {
	// Projects receives the imported git projects.
	Projects project.Store
	// Inventories receives the imported stored inventories.
	Inventories inventory.Store
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
