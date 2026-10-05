package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/template"
)

// awxNaturalKey is how AWX names a job template without its id: its organization and its name.
type awxNaturalKey struct {
	// org is the organization's name, empty for a template that belongs to none.
	org string
	// name is the job template's name.
	name string
}

// awxTemplateIDs is AWX's job template list reduced to the id of each template by natural key, for
// an export that carries no ids of its own.
type awxTemplateIDs struct {
	// byKey maps a natural key to the AWX id the list gives it.
	byKey map[awxNaturalKey]int64
	// ambiguous marks a natural key the list names more than once, so neither id is taken.
	ambiguous map[awxNaturalKey]bool
}

// awxListedTemplate is one job template in AWX's job template list, as its API serves it.
type awxListedTemplate struct {
	// ID is the AWX job template id.
	ID json.RawMessage `json:"id"`
	// Name is the job template's name.
	Name string `json:"name"`
	// Organization is the organization, which the API writes as its id and a natural-key export
	// writes by name.
	Organization awxRef `json:"organization"`
	// SummaryFields carries the organization's name beside its id, the way the API serves it.
	SummaryFields struct {
		// Organization is the organization the template belongs to.
		Organization struct {
			// Name is the organization's name.
			Name string `json:"name"`
		} `json:"organization"`
	} `json:"summary_fields"`
}

// orgName returns the organization the listed template belongs to, by name, or empty when the list
// names it only by an id.
func (t awxListedTemplate) orgName() string {
	if n := t.SummaryFields.Organization.Name; n != "" {
		return n
	}
	if strings.HasPrefix(t.Organization.Name, "id-") {
		return ""
	}
	return t.Organization.Name
}

// FromAWXWithTemplateIDs returns a mapper that imports an AWX export the way FromAWX does, taking
// the AWX id of each job template the export does not give one from lists: AWX's job template list
// as its API serves it, one page or several. An id is taken only for an exact match of organization
// and name that the list names once, so a template is never bound to another's id by a guess.
func FromAWXWithTemplateIDs(lists ...[]byte) (func(data []byte, now time.Time) (*Plan, error), error) {
	ids, err := parseAWXTemplateIDs(lists...)
	if err != nil {
		return nil, err
	}
	return func(data []byte, now time.Time) (*Plan, error) {
		return fromAWX(data, now, ids)
	}, nil
}

// parseAWXTemplateIDs reads AWX's job template list from each document: a page with results, a bare
// list, or an export's job_templates, each entry carrying its id and name.
func parseAWXTemplateIDs(lists ...[]byte) (*awxTemplateIDs, error) {
	ids := &awxTemplateIDs{byKey: map[awxNaturalKey]int64{}, ambiguous: map[awxNaturalKey]bool{}}
	for n, doc := range lists {
		entries, err := listedTemplates(doc)
		if err != nil {
			return nil, fmt.Errorf("read the awx job template list %d: %w", n+1, err)
		}
		for _, e := range entries {
			id, ok := awxIDValue(e.ID)
			if !ok || e.Name == "" {
				continue
			}
			key := awxNaturalKey{org: e.orgName(), name: e.Name}
			if prev, seen := ids.byKey[key]; seen && prev != id {
				ids.ambiguous[key] = true
			}
			ids.byKey[key] = id
		}
	}
	return ids, nil
}

// listedTemplates decodes the entries of one job template list document.
func listedTemplates(doc []byte) ([]awxListedTemplate, error) {
	doc = bytes.TrimSpace(doc)
	if len(doc) > 0 && doc[0] == '[' {
		var entries []awxListedTemplate
		if err := json.Unmarshal(doc, &entries); err != nil {
			return nil, err
		}
		return entries, nil
	}
	var page struct {
		// Results is a page of the list, the way the API serves it.
		Results []awxListedTemplate `json:"results"`
		// JobTemplates is an export's job templates, which carry ids when the export does.
		JobTemplates []awxListedTemplate `json:"job_templates"`
	}
	if err := json.Unmarshal(doc, &page); err != nil {
		return nil, err
	}
	if page.Results == nil && page.JobTemplates == nil {
		return nil, errors.New("it holds no results and no job_templates")
	}
	return append(page.Results, page.JobTemplates...), nil
}

// awxIDValue reads an AWX id written as a number or a string holding one, reporting false for
// anything that is not a whole number above zero.
func awxIDValue(raw json.RawMessage) (int64, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0, false
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		text = strings.TrimSpace(s)
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// mapAWXCallback decides what an imported template that accepted callbacks in AWX does with its
// AWX callback address and its limit, and says so in the report.
//
// The template keeps its limit on callbacks, the default, and a template that had a limit in AWX is
// named, since AWX launched for the calling host whatever its limit said. When the AWX id of the
// template is known, from the export or from AWX's template list, the id is bound to the template
// and its AWX-compatible address is on, so boot scripts that still post there keep working.
func (p *Plan) mapAWXCallback(jt awxJobTemplate, tpl *template.Template, name string) {
	if !jt.AllowCallbacks {
		return
	}
	if limit := strings.TrimSpace(jt.Limit); limit != "" {
		p.warn("template %q accepts provisioning callbacks in AWX and has the limit %q. AWX "+
			"launches a callback for the calling host whatever the limit says. Here the template "+
			"keeps its limit on callbacks (callback_limit intersect), so only a calling host %q "+
			"selects gets a run. To match AWX, set callback_limit to replace on the template, in "+
			"its dialog or with PUT /v1/templates/{id}", name, limit, limit)
	}
	id, ok := p.awxIDFor(jt, name)
	if !ok {
		return
	}
	if others, claimed := p.awxClaims()[id]; claimed {
		p.awxConflicts = append(p.awxConflicts, fmt.Sprintf("the export gives AWX job template id "+
			"%d to both %q and %q", id, others, name))
		p.warn("the export gives AWX job template id %d to both %q and %q. An id reaches one AWX "+
			"object, so applying this import is refused until the export is corrected", id, others,
			name)
		return
	}
	tpl.AWXCallback = true
	p.awxBindings = append(p.awxBindings, template.AWXBinding{AWXID: id, TemplateID: tpl.ID,
		Organization: jt.Organization.Name, Name: jt.Name, CreatedAt: tpl.CreatedAt,
		UpdatedAt: tpl.CreatedAt})
	p.warn("template %q also answers provisioning callbacks at the AWX-compatible address %s, so "+
		"a boot script that still posts there keeps working once the AWX hostname resolves to this "+
		"server. Move images to the template's own address, /v1/templates/<id>/callback, when you "+
		"can. The AWX address can be turned off on the template", name, template.AWXCallbackPath(id))
}

// awxIDFor returns the AWX id of an exported job template, from the export or from AWX's template
// list, or false when neither gives one it can trust, which the report then explains.
func (p *Plan) awxIDFor(jt awxJobTemplate, name string) (int64, bool) {
	own, hasOwn := awxIDValue(jt.ID)
	if len(bytes.TrimSpace(jt.ID)) > 0 && string(bytes.TrimSpace(jt.ID)) != "null" && !hasOwn {
		p.warn("template %q carries the AWX id %s, which is not a whole number above zero, so it "+
			"answers provisioning callbacks only at its own address, /v1/templates/<id>/callback",
			name, string(jt.ID))
		return 0, false
	}
	key := awxNaturalKey{org: jt.Organization.Name, name: jt.Name}
	var listed int64
	if p.awxIDs != nil {
		if p.awxIDs.ambiguous[key] {
			p.warn("AWX's template list names %q more than once, so template %q answers "+
				"provisioning callbacks only at its own address, /v1/templates/<id>/callback",
				jt.Name, name)
			return 0, false
		}
		listed = p.awxIDs.byKey[key]
	}
	switch {
	case hasOwn && listed != 0 && listed != own:
		p.warn("template %q carries the AWX id %d and AWX's template list gives it %d, so it "+
			"answers provisioning callbacks only at its own address, /v1/templates/<id>/callback, "+
			"until the two agree", name, own, listed)
		return 0, false
	case hasOwn:
		return own, true
	case listed != 0:
		return listed, true
	}
	p.warn("template %q accepts provisioning callbacks in AWX, and the import does not know its "+
		"AWX id, so it answers only at its own address, /v1/templates/<id>/callback. Point the hosts' "+
		"boot scripts there, or give the import AWX's job template list with --awx-template-ids so "+
		"it answers at the AWX-compatible address too", name)
	return 0, false
}

// awxClaims returns the name of the template each AWX id in the plan is bound to.
func (p *Plan) awxClaims() map[int64]string {
	names := make(map[string]string, len(p.Templates))
	for _, t := range p.Templates {
		names[t.ID] = t.Name
	}
	out := make(map[int64]string, len(p.awxBindings))
	for _, b := range p.awxBindings {
		out[b.AWXID] = names[b.TemplateID]
	}
	return out
}

// refuseAWXConflicts refuses, before anything is written, an import that would bind an AWX job
// template id to a different AWX object than the one this install already binds it to, or that
// gives one id to two templates. An id reaches one AWX object for good, so a boot script calling it
// never reaches something else. A binding of the same object moves to the template this import
// creates, and the report says so when it moves away from a template that still exists.
func (p *Plan) refuseAWXConflicts(ctx context.Context, templates template.Store) error {
	if len(p.awxConflicts) > 0 {
		return fmt.Errorf("%w: %s", template.ErrAWXConflict, strings.Join(p.awxConflicts, "; "))
	}
	if len(p.awxBindings) == 0 || templates == nil {
		return nil
	}
	var clashes []string
	for _, b := range p.awxBindings {
		existing, err := templates.AWXBindingFor(ctx, b.AWXID)
		if errors.Is(err, template.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read the binding of AWX job template %d: %w", b.AWXID, err)
		}
		if !existing.SameObject(b.Organization, b.Name) {
			clashes = append(clashes, fmt.Sprintf("AWX job template id %d is bound to %s, and this "+
				"export gives it to %s", b.AWXID, awxObjectName(existing.Organization, existing.Name),
				awxObjectName(b.Organization, b.Name)))
			continue
		}
		if existing.TemplateID == b.TemplateID {
			continue
		}
		if prior, gerr := templates.Get(ctx, existing.TemplateID); gerr == nil {
			p.warn("the AWX-compatible address of AWX job template %d moves from template %q to the "+
				"template this import creates for it", b.AWXID, prior.Name)
		}
	}
	if len(clashes) > 0 {
		return fmt.Errorf("%w: %s. An AWX job template id reaches one AWX object for good, so a "+
			"boot script that calls it never reaches something else. If this export comes from "+
			"another AWX install, import it without its AWX ids", template.ErrAWXConflict,
			strings.Join(clashes, "; "))
	}
	return nil
}

// claimAWX claims the plan's AWX job template ids before anything else is written and returns the
// release that gives back what it claimed. An id another import bound after refuseAWXConflicts
// read the bindings refuses the import here, with nothing written.
func (p *Plan) claimAWX(ctx context.Context, templates template.Store) (func(context.Context) error,
	error) {
	if templates == nil || len(p.awxBindings) == 0 {
		return func(context.Context) error { return nil }, nil
	}
	release, err := template.ClaimAWX(ctx, templates, p.awxBindings)
	if err != nil {
		return nil, fmt.Errorf("%w. Another import bound the id while this one ran, so nothing "+
			"was written", err)
	}
	return release, nil
}

// bindAWX records the plan's AWX bindings once the templates they reach are stored.
func (p *Plan) bindAWX(ctx context.Context, templates template.Store) error {
	if templates == nil {
		return nil
	}
	for _, b := range p.awxBindings {
		if err := templates.BindAWX(ctx, b); err != nil {
			return fmt.Errorf("bind AWX job template %d: %w", b.AWXID, err)
		}
	}
	return nil
}

// awxObjectName names an AWX job template by its organization and name, the way a person finds it
// in AWX.
func awxObjectName(org, name string) string {
	if org == "" {
		return strconv.Quote(name)
	}
	return strconv.Quote(name) + " in organization " + strconv.Quote(org)
}
