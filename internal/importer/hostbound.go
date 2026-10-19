package importer

import (
	"fmt"
	"strconv"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/util"
)

// CheckHostBounds returns an error wrapping inventory.ErrTooManyHosts that names the first planned
// inventory naming more hosts than inventory.MaxHosts, or nil when none does. Apply refuses such a
// plan before writing anything: every run against the inventory would be refused, and templates,
// schedules, and sources name it, so leaving it out would leave them naming nothing.
func (p *Plan) CheckHostBounds() error {
	for _, inv := range p.Inventories {
		if err := inventory.CheckHostBound(inv.Content); err != nil {
			return fmt.Errorf("the inventory %s: %w",
				strconv.Quote(util.Clip(util.SafeText(inv.Name), textNameCap)), err)
		}
	}
	return nil
}
