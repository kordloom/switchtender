package run

import "slices"

// RunAuth is everything that decides who may read a run, and nothing else.
//
// Derived records outlive the runs that produced them on purpose: host summaries, task summaries,
// drift, and host state history all answer questions about a fleet over time, and a run is deleted
// once its own detail stops being worth keeping. Readability is decided by resolving the governing
// run, so those two facts collide by design. Ask far enough back and the run is gone, and every
// derived row it governs becomes unreadable to a grant-restricted caller: not refused, not
// explained, simply absent.
//
// Keeping this when the run is purged is what closes that. It is deliberately the minimal set the
// readability rule reads, so it stays small enough to retain for far longer than a run and carries
// nothing that would make it a second copy of the run itself.
type RunAuth struct {
	// ID is the run this governs.
	ID string `json:"id"`
	// OrgID is the organization the run was stamped with, which scopes a run that names no stored
	// object at all.
	OrgID string `json:"org_id,omitempty"`
	// ProjectID is the project the run drew its content from.
	ProjectID string `json:"project_id,omitempty"`
	// InventoryID is the inventory the run targeted.
	InventoryID string `json:"inventory_id,omitempty"`
	// PullCredentialID is the registry credential the run used to fetch its execution image.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// CredentialIDs are the credentials the run materialized.
	CredentialIDs []string `json:"credential_ids,omitempty"`
}

// AuthOf reduces a run to what decides its readability.
//
// Every readability decision in the product goes through this, whether the run is live or long
// deleted, so a live run and its tombstone cannot be judged by different rules. Two authorization
// paths that are meant to agree are a divergence waiting to happen, and the divergence would be
// silent and in the direction of showing somebody something.
func AuthOf(r *Run) *RunAuth {
	if r == nil {
		return nil
	}
	return &RunAuth{
		ID: r.ID, OrgID: r.OrgID, ProjectID: r.ProjectID, InventoryID: r.InventoryID,
		PullCredentialID: r.PullCredentialID, CredentialIDs: slices.Clone(r.CredentialIDs),
	}
}

// Objects lists the stored objects a run's readability is scoped by, in a stable order.
//
// A run that names objects is readable only to a caller who may use every one of them. A run that
// names none has nothing for that check to filter on and is scoped by its organization instead.
func (a *RunAuth) Objects() []string {
	if a == nil {
		return nil
	}
	objs := make([]string, 0, 3+len(a.CredentialIDs))
	if a.ProjectID != "" {
		objs = append(objs, a.ProjectID)
	}
	if a.InventoryID != "" {
		objs = append(objs, a.InventoryID)
	}
	if a.PullCredentialID != "" {
		objs = append(objs, a.PullCredentialID)
	}
	return append(objs, a.CredentialIDs...)
}
