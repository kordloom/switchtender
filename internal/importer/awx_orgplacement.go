package importer

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
)

// smartUnplacedWarning is what an imported smart inventory that arrives with no organization says.
// AWX filters a smart inventory over its own organization's inventories, and here one with no
// organization filters every inventory the launching user may use, so the reach differs until it is
// placed by hand.
const smartUnplacedWarning = "smart inventory %q filtered the hosts of organization %q in AWX. " +
	"Here a smart inventory with no organization filters every inventory the launching user may " +
	"use, so place it and its inventories in one organization to keep the same reach"

// awxOrgEntry is the part of an exported AWX organization that placing a smart inventory reads: its
// name and, in an export taken from the REST API, the id a reference written as a bare integer
// names.
type awxOrgEntry struct {
	// Name is the organization's name.
	Name string `json:"name"`
	// ID is the organization's AWX id, present in an API export and absent from an awxkit one.
	ID json.RawMessage `json:"id"`
}

// awxOrgIndex holds the organizations an export carries, so a reference can be resolved to one of
// them by name or, for a reference written as an integer id, by that id.
type awxOrgIndex struct {
	// names holds the name of every organization the export carries.
	names map[string]bool
	// byID maps the id-N spelling an integer reference decodes to onto the organization's name.
	byID map[string]string
}

// newAWXOrgIndex indexes the organizations an export carries. An entry that does not decode, or
// that has no name, names nothing an inventory could be placed in, so it is not indexed.
func newAWXOrgIndex(raw []json.RawMessage) awxOrgIndex {
	x := awxOrgIndex{names: map[string]bool{}, byID: map[string]string{}}
	for _, r := range raw {
		var e awxOrgEntry
		if json.Unmarshal(r, &e) != nil || strings.TrimSpace(e.Name) == "" {
			continue
		}
		x.names[e.Name] = true
		var id json.Number
		if len(e.ID) > 0 && json.Unmarshal(e.ID, &id) == nil && id != "" {
			x.byID["id-"+id.String()] = e.Name
		}
	}
	return x
}

// canonical returns the organization a reference stands for: the name of the exported organization
// an integer reference names, or the reference as written.
func (x awxOrgIndex) canonical(ref string) string {
	if name, ok := x.byID[ref]; ok {
		return name
	}
	return ref
}

// carries reports whether the export carries the organization a canonical reference names.
func (x awxOrgIndex) carries(name string) bool {
	return name != "" && x.names[name]
}

// placeAWXSmartInventories places each imported smart inventory in the AWX organization it belongs
// to, when the export carries that organization, together with every other inventory imported from
// the same organization.
//
// An AWX smart inventory filters the hosts of its own organization's inventories. Here a smart
// inventory in an organization reads that organization's inventories and nothing else, and one in
// none reads every inventory the launching user may use. Placing the smart inventory alone would
// therefore reach nothing, and leaving it unplaced reaches too much, so the whole organization's
// inventories are placed with it, which is the reach it had in AWX. An organization the export only
// names, without carrying it, is not created on a guess: its smart inventories arrive with no
// organization and the warning says how to restore the reach by hand. Only an organization that
// owns a smart inventory is touched.
//
// The organizations are created when the plan is applied, or matched to an existing one of the same
// name, which only the store can say. The organization is the one awxOrg plans for the AWX
// organization, shared with anything else the import places there.
func (p *Plan) placeAWXSmartInventories(export awxExport, inventoryIDs awxIDs, now time.Time) {
	exported := newAWXOrgIndex(export.Organizations)
	orgOf := p.awxInventoryOrgs(export, inventoryIDs, exported)
	var order []string
	smart := map[string][]string{}
	for _, inv := range p.Inventories {
		name := orgOf[inv.ID]
		if inv.Kind != inventory.KindSmart || name == "" {
			continue
		}
		if !exported.carries(name) {
			p.warn(smartUnplacedWarning, inv.Name, oneLine(name))
			continue
		}
		if _, seen := smart[name]; !seen {
			order = append(order, name)
		}
		smart[name] = append(smart[name], inv.Name)
	}
	for _, name := range order {
		o := p.awxOrg(name, now)
		placed := 0
		for _, inv := range p.Inventories {
			if orgOf[inv.ID] == name {
				inv.OrgID = o.ID
				placed++
			}
		}
		for _, s := range smart[name] {
			others := placed - 1
			p.warn("smart inventory %q is placed in organization %q with the %d other %s imported "+
				"from that organization, so its filter reads those inventories and no others, which "+
				"is the reach it had in AWX. AWX memberships do not come across: add the "+
				"organization's members here so they keep their access", s, oneLine(name), others,
				inventoryNoun(others))
		}
	}
}

// inventoryNoun returns inventory or inventories for a count.
func inventoryNoun(n int) string {
	if n == 1 {
		return "inventory"
	}
	return "inventories"
}

// awxInventoryOrgs maps each inventory the plan creates to the AWX organization it was imported
// from. An inventory and a composed inventory carry the organization their reference was recorded
// under, and a dynamic source's backing inventory carries the organization of the inventory the
// source feeds.
func (p *Plan) awxInventoryOrgs(export awxExport, inventoryIDs awxIDs, exported awxOrgIndex) map[string]string {
	out := map[string]string{}
	for key, id := range inventoryIDs {
		ref, _, _ := strings.Cut(key, orgKeySep)
		out[id] = exported.canonical(ref)
	}
	bySource := awxSourceOrgs(export, exported)
	for _, src := range p.Sources {
		if name := bySource[src.Name]; name != "" {
			out[src.InventoryID] = name
		}
	}
	return out
}

// awxSourceOrgs maps a dynamic source's name to the AWX organization of the inventory it feeds,
// reading the sources the way FromAWX creates them. A source nested under an inventory takes that
// inventory's organization. A source exported at the top level takes the organization its inventory
// reference names, or the organization of the only inventory with the referenced name. A name that
// sources in two organizations share maps to nothing, because the plan's sources are told apart by
// name and guessing would place one organization's source inventory in the other.
func awxSourceOrgs(export awxExport, exported awxOrgIndex) map[string]string {
	out := map[string]string{}
	conflict := map[string]bool{}
	record := func(source, ref string) {
		if source == "" {
			return
		}
		name := exported.canonical(ref)
		if have, ok := out[source]; ok && have != name {
			conflict[source] = true
		}
		out[source] = name
	}
	byName := map[string][]string{}
	for _, inv := range slices.Concat(export.Inventory, export.Inventories) {
		if !slices.Contains(byName[inv.Name], inv.Organization.Name) {
			byName[inv.Name] = append(byName[inv.Name], inv.Organization.Name)
		}
		if inv.Related == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(inv.Kind))
		composed := kind == inventory.KindSmart || kind == inventory.KindConstructed
		for _, s := range inv.Related.InventorySources {
			// A composed inventory's constructed source became its options, not a dynamic source.
			if composed && s.constructed() {
				continue
			}
			record(s.Name, inv.Organization.Name)
		}
	}
	for _, s := range export.InventorySources {
		if s.constructed() {
			continue
		}
		ref := s.Inventory.Org
		if ref == "" {
			if orgs := byName[s.Inventory.Name]; len(orgs) == 1 {
				ref = orgs[0]
			}
		}
		record(s.Name, ref)
	}
	for source := range conflict {
		delete(out, source)
	}
	return out
}

// awxUnplacedOrganizations counts the exported organizations the import does not bring across,
// which are the ones the report still says the import does not create. An organization the import
// plans is counted as coming across, and so is one holding notification attachments of its own,
// which the organization notification import accounts for, stating what became of it.
func (p *Plan) awxUnplacedOrganizations(raw []json.RawMessage) int {
	accounted := map[string]bool{}
	for _, o := range p.Orgs {
		accounted[o.Name] = true
	}
	for _, plan := range p.orgNotify {
		accounted[plan.name] = true
	}
	n := 0
	for _, r := range raw {
		var e awxOrgEntry
		if json.Unmarshal(r, &e) == nil && accounted[e.Name] {
			continue
		}
		n++
	}
	return n
}

// awxUnplacedOrgNoun names the organizations the report still says the import does not create:
// other organizations once some came across to hold smart inventories, so the count that follows is
// not read as every organization the export holds.
func (p *Plan) awxUnplacedOrgNoun() string {
	if len(p.Orgs) > 0 {
		return "other organization"
	}
	return "organization"
}

// composedLabel names a composed inventory in the assessment: its name, its kind, and the
// organization an import places it in, when it is placed in one.
func (p *Plan) composedLabel(inv *inventory.Inventory) string {
	if name := p.InventoryOrganizations()[inv.Name]; name != "" {
		return inv.Name + " (" + inv.Kind + ", in organization " + name + ")"
	}
	return inv.Name + " (" + inv.Kind + ")"
}
