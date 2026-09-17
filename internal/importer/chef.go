package importer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
)

// chefNode is one node as Chef Infra Server stores it.
//
// Only the members named here are read. Everything else in a node document, and there is a great
// deal of it, is reported by the unread scan rather than dropped quietly.
type chefNode struct {
	// Name is the node's name on the Chef server, usually its fqdn.
	Name string `json:"name"`
	// ChefEnvironment is the environment the node belongs to, which becomes a group.
	ChefEnvironment string `json:"chef_environment"`
	// RunList is what Chef applies to the node, entries like role[web] or recipe[nginx::default].
	// Roles become groups; recipes are named in a warning because nothing here applies them.
	RunList []string `json:"run_list"`
	// Automatic holds the facts ohai collected. Read as a unit and selected from, because a node's
	// automatic attributes run to hundreds of keys and carrying them all would bury the inventory.
	Automatic map[string]any `json:"automatic"`
	// Normal holds attributes set on the node itself.
	Normal map[string]any `json:"normal"`
}

// chefFacts are the ohai attributes carried onto a host, and the only ones.
//
// A node's automatic attributes are a machine inventory in their own right: kernel modules, every
// filesystem, every network interface, the full package list. Copying that into an Ansible inventory
// produces a file nobody can read and a host line thousands of characters long. These are the ones
// that identify a machine and let a play reach it.
var chefFacts = []string{"fqdn", "ipaddress", "platform", "platform_version", "os"}

// FromChef maps a Chef Infra Server node export into an inventory of the fleet it describes.
//
// It exists because Chef Infra Server's open source line reaches end of life, which puts a
// population of run-listed, role-grouped fleets in the same position AWX users are in, except with a
// date on it rather than silence.
//
// What comes across is the fleet and its shape: every node, grouped by its environment and by every
// role in its run list, with the handful of ohai facts that identify a machine and let a play reach
// it. What does not come across is the cookbooks, and that is not a gap this importer could close:
// a recipe is a program in a different language against a different model, and a converter that
// half-translated one would produce something that looks like the original and does not do what it
// does. The honest boundary is the fleet, which is exactly what somebody needs on day one to start
// governing the machines while the recipes are dealt with separately.
//
// Accepts what the Chef tools actually emit: an array of node documents, a single node document, or
// an object keyed by node name.
func FromChef(data []byte, now time.Time) (*Plan, error) {
	nodes, err := decodeChefNodes(data)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%w: no chef nodes in this document", ErrNothingRecognized)
	}

	// Said once and plainly rather than per node. Somebody importing a Chef estate is entitled to
	// know the recipes did not come with it before they read a plan that looks complete.
	plan.warn("cookbooks and recipes are not imported: a recipe is a program in another language " +
		"against another model, and a partial translation would look like the original without " +
		"doing what it does. This import brings the fleet and its grouping, so runs can be governed " +
		"while the recipes are handled separately.")

	var hosts []importHost
	byGroup := map[string][]importHost{}
	recipes := map[string]bool{}
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name, _ = n.Automatic["fqdn"].(string)
		}
		if strings.TrimSpace(name) == "" {
			plan.warn("a node with no name and no fqdn was not imported, since an inventory entry " +
				"with no host is a line that matches nothing")
			continue
		}
		host := importHost{Name: name, Variables: chefHostVars(n)}
		hosts = append(hosts, host)

		if env := strings.TrimSpace(n.ChefEnvironment); env != "" && env != "_default" {
			byGroup[env] = append(byGroup[env], host)
		}
		for _, entry := range n.RunList {
			switch kind, value := splitRunListEntry(entry); kind {
			case "role":
				byGroup[value] = append(byGroup[value], host)
			case "recipe":
				recipes[value] = true
			}
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%w: no chef node carried a usable name", ErrNothingRecognized)
	}

	if len(recipes) > 0 {
		plan.warn("%d recipe%s in these run lists %s not imported: %s",
			len(recipes), plural(len(recipes)), wasWere(len(recipes)), joinSorted(recipes, 8))
	}

	groups := make([]importGroup, 0, len(byGroup))
	for name := range byGroup {
		groups = append(groups, importGroup{Name: name, Hosts: byGroup[name]})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })

	const invName = "chef fleet"
	plan.Inventories = append(plan.Inventories, &inventory.Inventory{
		ID:      inventory.NewID(),
		Name:    invName,
		Content: buildInventoryINI(plan, invName, hosts, groups, nil),
	})
	plan.warn("%d host%s imported carrying only the facts that identify a machine (%s). A node's "+
		"other automatic attributes were not copied, since they run to hundreds of keys per host.",
		len(hosts), plural(len(hosts)), strings.Join(chefFacts, ", "))

	reportUnread(plan, data, chefDocumentShape{})
	return plan, nil
}

// chefDocumentShape is the shape the unread scan compares an export against.
//
// The document arrives in three forms and the scan needs the one that is a list of nodes, since that
// is where an unread field would hide. A single node or a name-keyed object walks the same members.
type chefDocumentShape []chefNode

// chefHostVars selects the facts carried onto a host line, including the address a play connects to.
func chefHostVars(n chefNode) map[string]any {
	vars := map[string]any{}
	for _, key := range chefFacts {
		if v, ok := n.Automatic[key]; ok && v != nil && v != "" {
			vars[key] = v
		}
	}
	// Without this a play resolves the inventory name through DNS, which is the one thing a Chef
	// estate cannot be assumed to have consistently: nodes are addressed by certname, not by a
	// record somebody maintained.
	if ip, ok := n.Automatic["ipaddress"].(string); ok && ip != "" {
		vars["ansible_host"] = ip
	}
	return vars
}

// splitRunListEntry reads a run list entry, returning its kind and its value. A bare name is a
// recipe, which is how Chef itself reads one.
func splitRunListEntry(entry string) (kind, value string) {
	entry = strings.TrimSpace(entry)
	open := strings.IndexByte(entry, '[')
	if open < 0 || !strings.HasSuffix(entry, "]") {
		if entry == "" {
			return "", ""
		}
		return "recipe", entry
	}
	return entry[:open], entry[open+1 : len(entry)-1]
}

// decodeChefNodes reads the three shapes the Chef tooling emits: an array of nodes, one node, or an
// object keyed by node name. Accepting all three matters because which one somebody has depends on
// how they dumped it, and refusing two of them reads as the importer not supporting Chef.
func decodeChefNodes(data []byte) ([]chefNode, error) {
	var list []chefNode
	if err := json.Unmarshal(data, &list); err == nil {
		return list, nil
	}
	var keyed map[string]chefNode
	if err := json.Unmarshal(data, &keyed); err == nil {
		names := make([]string, 0, len(keyed))
		for name := range keyed {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]chefNode, 0, len(keyed))
		for _, name := range names {
			node := keyed[name]
			// A name-keyed dump often omits the name inside each object, since the key carries it.
			if node.Name == "" {
				node.Name = name
			}
			out = append(out, node)
		}
		// A single node document also unmarshals into a map, and every key would become a node with
		// no run list and no environment. Telling them apart by whether anything looks like a node.
		if looksLikeChefNodes(out) {
			return out, nil
		}
	}
	var one chefNode
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, fmt.Errorf("parse chef export: %w", err)
	}
	if one.Name == "" && one.ChefEnvironment == "" && len(one.RunList) == 0 {
		return nil, fmt.Errorf("%w: this does not look like a chef node export",
			ErrNothingRecognized)
	}
	return []chefNode{one}, nil
}

// looksLikeChefNodes reports whether a name-keyed decode produced anything with a node's shape,
// which is what separates a map of nodes from one node decoded a key at a time.
func looksLikeChefNodes(nodes []chefNode) bool {
	for _, n := range nodes {
		if n.ChefEnvironment != "" || len(n.RunList) > 0 || len(n.Automatic) > 0 {
			return true
		}
	}
	return false
}

// joinSorted renders a set in a stable order, capped, saying how many it left off.
func joinSorted(set map[string]bool, max int) string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) > max {
		return strings.Join(out[:max], ", ") + fmt.Sprintf(", and %d more", len(out)-max)
	}
	return strings.Join(out, ", ")
}

// wasWere returns the verb a count needs, so one recipe was and two recipes were.
func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
