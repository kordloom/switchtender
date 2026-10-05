package importer

import (
	"encoding/json"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/inventory"
)

// addComposed maps one AWX smart or constructed inventory into a composed inventory, wiring a
// constructed inventory's inputs by id. constructedSources holds the source AWX keeps behind each
// constructed inventory, keyed by organization and name, which carries its options and limit when
// the export does not flatten them onto the inventory.
//
// Each is checked the way the API checks one a person creates, and refused with the reason when it
// would not resolve: a filter this cannot read, options the constructed plugin is not allowed, or
// a constructed inventory none of whose inputs came across. Creating one anyway would leave an
// inventory that refuses every launch, which is a migration that looks complete and is not.
func (p *Plan) addComposed(inv awxInventory, name string, now time.Time, inventoryIDs awxIDs,
	constructedSources map[string]awxInventorySource) {
	kind := strings.ToLower(strings.TrimSpace(inv.Kind))
	obj := &inventory.Inventory{ID: inventory.NewID(), Name: name, Kind: kind, CreatedAt: now}
	switch kind {
	case inventory.KindSmart:
		// Its organization is settled in placeAWXSmartInventories, once every inventory exists.
		obj.HostFilter = strings.TrimSpace(inv.HostFilter)
	case inventory.KindConstructed:
		src, hasSource := constructedSources[inv.Organization.Name+orgKeySep+inv.Name]
		if !hasSource {
			// A top-level source may name its inventory without the organization.
			src, hasSource = constructedSources[orgKeySep+inv.Name]
		}
		obj.SourceVars = sourceVarsText(inv.SourceVars)
		if obj.SourceVars == "" && hasSource {
			obj.SourceVars = sourceVarsText(src.SourceVars)
		}
		obj.Limit = strings.TrimSpace(inv.Limit)
		if obj.Limit == "" && hasSource {
			obj.Limit = strings.TrimSpace(src.Limit)
		}
		for _, ref := range inv.inputInventories() {
			id, ok := inventoryIDs.get(ref)
			if !ok {
				p.warn("constructed inventory %q names input inventory %s, which is not in this "+
					"export or is itself smart or constructed, so it was left out of the inputs", name,
					inventoryIDs.unresolved(ref))
				continue
			}
			obj.InputIDs = append(obj.InputIDs, id)
		}
	}
	if err := inventory.Validate(obj); err != nil {
		p.warn("%s inventory %q was not imported: %v", kind, name, err)
		p.refused++
		return
	}
	if inventoryIDs.set(inv.Organization.Name, inv.Name, obj.ID) {
		p.warn("inventory %q appears more than once; the later one is what templates naming it "+
			"will use", name)
	}
	p.Inventories = append(p.Inventories, obj)
}

// sourceVarsText renders AWX source_vars as the YAML text a constructed inventory keeps. AWX stores
// them as a YAML or JSON string, and an export built from the API may carry them as an object.
func sourceVarsText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "{}" || text == "---" {
			return ""
		}
		return text
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || len(obj) == 0 {
		return ""
	}
	out, err := yaml.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(out)
}
