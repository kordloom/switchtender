package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
)

// puppetNode is one node as PuppetDB reports it from /pdb/query/v4/nodes.
type puppetNode struct {
	// Certname is the node's certificate name, which is how Puppet addresses a machine.
	Certname string `json:"certname"`
	// CatalogEnvironment is the environment the node's catalog was compiled in.
	CatalogEnvironment string `json:"catalog_environment"`
	// ReportEnvironment is the environment the node last reported in, used when no catalog
	// environment is present.
	ReportEnvironment string `json:"report_environment"`
	// LatestReportStatus is the node's last run outcome: changed, unchanged, or failed. Carried as a
	// host variable because a fleet's last-known state is the thing somebody migrating wants first.
	LatestReportStatus string `json:"latest_report_status"`
	// Deactivated is set when the node was deactivated in PuppetDB, which means it is not part of
	// the fleet any more.
	Deactivated string `json:"deactivated"`
	// Expired is set when PuppetDB aged the node out.
	Expired string `json:"expired"`
}

// puppetFact is one fact row from /pdb/query/v4/facts, which is how PuppetDB returns facts: one row
// per node per fact rather than a document per node.
type puppetFact struct {
	// Certname is the node the fact belongs to.
	Certname string `json:"certname"`
	// Name is the fact's name.
	Name string `json:"name"`
	// Value is the fact's value.
	Value any `json:"value"`
}

// puppetFacts are the facts carried onto a host line, and the only ones.
//
// A Puppet node reports hundreds of facts, including every interface, every partition, and the full
// output of any custom fact somebody wrote. These identify a machine and let a play reach it.
var puppetFacts = []string{"fqdn", "ipaddress", "osfamily", "operatingsystem",
	"operatingsystemrelease", "kernel"}

// FromPuppet maps a Puppet fleet into an inventory of the machines it manages.
//
// Puppet's open source line is where Chef's is: a large installed base with no supported path
// forward that does not involve buying something. What that population has is a fleet, grouped by
// environment, addressed by certname.
//
// What comes across is that fleet. What does not is the manifests, for the same reason Chef's
// recipes do not: a manifest is a program in another language against another model, and a partial
// translation of one would look right and behave differently. Naming that boundary is the honest
// thing to do and it is also the useful one, because the fleet is what somebody needs to start
// governing runs on day one.
//
// Accepts a PuppetDB nodes query, a PuppetDB facts query, or the plain list of certnames that
// `puppet node list` prints, because which of those somebody has depends on what they can reach.
func FromPuppet(data []byte, now time.Time) (*Plan, error) {
	plan := &Plan{}
	nodes, facts, err := decodePuppet(data)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%w: no puppet nodes in this document", ErrNothingRecognized)
	}

	plan.warn("manifests and modules are not imported: a manifest is a program in another language " +
		"against another model, and a partial translation would look like the original without " +
		"doing what it does. This import brings the fleet and its grouping, so runs can be governed " +
		"while the manifests are handled separately.")

	var hosts []importHost
	byGroup := map[string][]importHost{}
	skipped := 0
	for _, n := range nodes {
		if n.Deactivated != "" || n.Expired != "" {
			// A deactivated node is not part of the fleet. Importing it would put a machine in the
			// inventory that Puppet itself stopped managing, and a play targeting all would reach
			// for it.
			skipped++
			continue
		}
		name := strings.TrimSpace(n.Certname)
		if name == "" {
			continue
		}
		vars := map[string]any{}
		for key, value := range facts[name] {
			vars[key] = value
		}
		if n.LatestReportStatus != "" {
			vars["puppet_last_run"] = n.LatestReportStatus
		}
		if ip, ok := vars["ipaddress"].(string); ok && ip != "" {
			vars["ansible_host"] = ip
		}
		host := importHost{Name: name, Variables: vars}
		hosts = append(hosts, host)

		env := n.CatalogEnvironment
		if env == "" {
			env = n.ReportEnvironment
		}
		if env = strings.TrimSpace(env); env != "" {
			byGroup[env] = append(byGroup[env], host)
		}
	}
	if skipped > 0 {
		plan.warn("%d node%s deactivated or expired in PuppetDB %s not imported, since Puppet "+
			"itself stopped managing %s", skipped, plural(skipped), wasWere(skipped),
			itOrThem(skipped))
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%w: every puppet node in this document is deactivated or unnamed",
			ErrNothingRecognized)
	}

	groups := make([]importGroup, 0, len(byGroup))
	for name := range byGroup {
		groups = append(groups, importGroup{Name: name, Hosts: byGroup[name]})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })

	const invName = "puppet fleet"
	plan.Inventories = append(plan.Inventories, &inventory.Inventory{
		ID:      inventory.NewID(),
		Name:    invName,
		Content: buildInventoryINI(plan, invName, hosts, groups, nil),
	})
	if len(facts) == 0 {
		plan.warn("this document carried no facts, so hosts import with no variables and no " +
			"address. Export a PuppetDB facts query alongside the nodes to carry them.")
	} else {
		plan.warn("%d host%s imported carrying only the facts that identify a machine (%s). The "+
			"rest of each node's facts were not copied, since a node reports hundreds.",
			len(hosts), plural(len(hosts)), strings.Join(puppetFacts, ", "))
	}
	return plan, nil
}

// decodePuppet reads the three shapes a Puppet fleet arrives in, returning the nodes and any facts
// found keyed by certname.
func decodePuppet(data []byte) ([]puppetNode, map[string]map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("%w: this document is empty", ErrNothingRecognized)
	}
	// A plain certname list is not JSON, and it is what `puppet node list` prints. Reading it means
	// somebody who can reach the CA but not PuppetDB still has a path.
	if trimmed[0] != '[' && trimmed[0] != '{' {
		return decodePuppetNodeList(trimmed), nil, nil
	}

	// A facts query and a nodes query are both arrays of objects. They are told apart by their
	// members rather than by a flag, since the caller has no way to say which they ran.
	var rows []json.RawMessage
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, nil, fmt.Errorf("parse puppet export: %w", err)
	}
	var nodes []puppetNode
	facts := map[string]map[string]any{}
	wanted := map[string]bool{}
	for _, f := range puppetFacts {
		wanted[f] = true
	}
	for _, row := range rows {
		var fact puppetFact
		if json.Unmarshal(row, &fact) == nil && fact.Name != "" && fact.Certname != "" {
			if wanted[fact.Name] {
				if facts[fact.Certname] == nil {
					facts[fact.Certname] = map[string]any{}
				}
				facts[fact.Certname][fact.Name] = fact.Value
			}
			continue
		}
		var node puppetNode
		if json.Unmarshal(row, &node) == nil && node.Certname != "" {
			nodes = append(nodes, node)
		}
	}
	// A facts-only export still names every node it carries facts for, which is a fleet.
	if len(nodes) == 0 && len(facts) > 0 {
		names := make([]string, 0, len(facts))
		for name := range facts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			nodes = append(nodes, puppetNode{Certname: name})
		}
	}
	return nodes, facts, nil
}

// decodePuppetNodeList reads the certname per line that `puppet node list` prints, ignoring the
// status suffix the command appends when it is run with a format that includes one.
func decodePuppetNodeList(data []byte) []puppetNode {
	var nodes []puppetNode
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `puppet node list` prints "certname (SHA256) ..." in some versions; the certname is the
		// first field either way.
		nodes = append(nodes, puppetNode{Certname: strings.Fields(line)[0]})
	}
	return nodes
}

// itOrThem returns the pronoun a count needs, so one node is it and two nodes are them.
func itOrThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
