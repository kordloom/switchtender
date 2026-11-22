package importer

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	// aliasNodeCost is what each YAML node counts toward an expansion budget beyond its own text. A
	// node decodes to a map entry, a list element, or an interface value, and each costs memory
	// whatever it holds.
	aliasNodeCost = 16
	// minExpansionBudget is the least an import's YAML may decode to, so a small export with
	// ordinary anchors is never refused.
	minExpansionBudget = 1 << 20
	// expansionFactor is how many times its own length an import's YAML may decode to.
	expansionFactor = 8
)

// expansionBudget bounds how far YAML aliases may expand one import, across every YAML value in
// it. An alias costs nothing to decode and becomes a full copy once decoded, so an 80 KB variables
// string of nested aliases decoded to more than 12 GiB of inventory text and took the server down,
// and the import preview needs no sign-in on the public demo. The budget is shared by the whole
// import, since one per value let an export of ten thousand small hosts expand each one to the
// limit.
type expansionBudget struct {
	// total is the budget the import started with.
	total int
	// left is how many bytes the import's YAML may still decode to.
	left int
	// err is the refusal, set once the budget is spent and returned for every value after it.
	err error
}

// newExpansionBudget returns the budget for an import whose input is rawLen bytes long: the larger
// of minExpansionBudget and expansionFactor times rawLen.
func newExpansionBudget(rawLen int) *expansionBudget {
	total := max(minExpansionBudget, expansionFactor*rawLen)
	return &expansionBudget{total: total, left: total}
}

// charge counts n toward the budget, following every alias to the node it names, and returns the
// refusal once the budget is spent. field names what n was read from, for the refusal's sentence.
// It walks with a stack of its own, since aliases can chain deeper than a call stack should grow.
func (b *expansionBudget) charge(n *yaml.Node, field string) error {
	if b.err != nil {
		return b.err
	}
	stack := []*yaml.Node{n}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil {
			continue
		}
		b.left -= aliasNodeCost + len(n.Value)
		if b.left < 0 {
			b.err = fmt.Errorf("%w: %s uses YAML aliases that expand the export past %d bytes, "+
				"more than %d times its size. Write the repeated values out in place of the aliases, "+
				"or split the export", ErrExpansion, field, b.total, expansionFactor)
			return b.err
		}
		if n.Kind == yaml.AliasNode {
			stack = append(stack, n.Alias)
			continue
		}
		stack = append(stack, n.Content...)
	}
	return nil
}

// expansion returns the plan's expansion budget, starting one sized for rawLen bytes when the plan
// has none yet.
func (p *Plan) expansion(rawLen int) *expansionBudget {
	if p.budget == nil {
		p.budget = newExpansionBudget(rawLen)
	}
	return p.budget
}
