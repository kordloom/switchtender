package server

import (
	"context"
	"net/http"
	"slices"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/inventory"
	"github.com/dcadolph/switchtender/internal/invsource"
	"github.com/dcadolph/switchtender/internal/project"
	"github.com/dcadolph/switchtender/internal/template"
)

// usedBy lists the human names of the objects that still reference an id, keyed by resource type, so
// a blocked delete can report exactly what to detach first.
type usedBy map[string][]string

// empty reports whether nothing references the id.
func (u usedBy) empty() bool {
	for _, names := range u {
		if len(names) > 0 {
			return false
		}
	}
	return true
}

// refChecker finds the configuration objects that reference a credential or project. Runs are never
// consulted: they are immutable history and must not keep an object they once used from being
// deleted.
type refChecker struct {
	// templates are searched for credential and project references.
	templates template.Store
	// inventories are searched for credential references.
	inventories inventory.Store
	// projects are searched for credential references.
	projects project.Store
	// invSources are searched for credential and project references.
	invSources invsource.Store
}

// credentialRefs returns the configuration objects that still use the credential id.
func (c *refChecker) credentialRefs(ctx context.Context, id string) (usedBy, error) {
	out := usedBy{}
	if c.templates != nil {
		list, err := c.templates.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			if slices.Contains(t.CredentialIDs, id) {
				out["templates"] = append(out["templates"], nameOr(t.Name, t.ID))
			}
		}
	}
	if c.inventories != nil {
		list, err := c.inventories.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, i := range list {
			if slices.Contains(i.CredentialIDs, id) {
				out["inventories"] = append(out["inventories"], nameOr(i.Name, i.ID))
			}
		}
	}
	if c.projects != nil {
		list, err := c.projects.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range list {
			if p.CredentialID == id || p.PullCredentialID == id {
				out["projects"] = append(out["projects"], nameOr(p.Name, p.ID))
			}
		}
	}
	if c.invSources != nil {
		list, err := c.invSources.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			if s.CredentialID == id {
				out["inventory_sources"] = append(out["inventory_sources"], nameOr(s.Name, s.ID))
			}
		}
	}
	return out, nil
}

// projectRefs returns the configuration objects that still use the project id.
func (c *refChecker) projectRefs(ctx context.Context, id string) (usedBy, error) {
	out := usedBy{}
	if c.templates != nil {
		list, err := c.templates.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			if t.ProjectID == id {
				out["templates"] = append(out["templates"], nameOr(t.Name, t.ID))
			}
		}
	}
	if c.invSources != nil {
		list, err := c.invSources.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			if s.ProjectID == id {
				out["inventory_sources"] = append(out["inventory_sources"], nameOr(s.Name, s.ID))
			}
		}
	}
	return out, nil
}

// nameOr returns name when set, else the id, so the report reads well even for an unnamed object.
func nameOr(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// respondInUse writes a 409 naming what still references the object, so the caller knows what to
// detach before the delete can proceed.
func respondInUse(w http.ResponseWriter, log *zap.Logger, msg string, used usedBy, pretty bool) {
	respondJSON(w, log, http.StatusConflict,
		map[string]any{"error": msg, "used_by": used}, pretty)
}
