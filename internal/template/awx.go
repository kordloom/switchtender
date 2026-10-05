package template

import (
	"context"
	"sort"
	"strconv"
	"time"
)

// The provisioning callback limit modes a template may set in CallbackLimit.
const (
	// CallbackLimitIntersect launches a provisioning callback only when the calling host also falls
	// within the template's own limit, and refuses it otherwise. It is the default.
	CallbackLimitIntersect = "intersect"
	// CallbackLimitReplace launches a provisioning callback against the calling host whatever the
	// template's limit says, the way AWX does.
	CallbackLimitReplace = "replace"
)

// NormalizeCallbackLimit returns the callback limit mode mode stands for, mapping the empty mode to
// the default, CallbackLimitIntersect.
func NormalizeCallbackLimit(mode string) string {
	if mode == "" {
		return CallbackLimitIntersect
	}
	return mode
}

// ValidCallbackLimit reports whether mode is a callback limit mode a template may set: empty,
// intersect, or replace.
func ValidCallbackLimit(mode string) bool {
	switch mode {
	case "", CallbackLimitIntersect, CallbackLimitReplace:
		return true
	}
	return false
}

// AWXCallbackPath returns the AWX-compatible callback address of an AWX job template id: the path
// AWX serves that template's provisioning callback on, which boot scripts written for AWX call.
func AWXCallbackPath(awxID int64) string {
	return "/api/v2/job_templates/" + strconv.FormatInt(awxID, 10) + "/callback/"
}

// AWXBinding ties an AWX job template id to the template an import created from that AWX job
// template, so a host whose boot script still posts to AWX's callback address reaches it.
//
// A binding is never handed to anything else. Only an import of the same AWX object, by
// organization and name, points it at another template, and a different AWX object claiming the
// same id fails the import. When its template is deleted the binding keeps the deleted template's
// id, which is never reused, so the address answers gone rather than reaching whatever is created
// next.
type AWXBinding struct {
	// AWXID is the AWX job template id the address carries.
	AWXID int64 `json:"awx_id"`
	// TemplateID is the template the address reaches, or the deleted template it reached.
	TemplateID string `json:"template_id"`
	// Organization is the AWX organization the job template belongs to, empty when it belongs to
	// none. With Name it is how a later import tells the same AWX object from a different one.
	Organization string `json:"awx_organization,omitempty"`
	// Name is the AWX job template's name.
	Name string `json:"awx_name"`
	// CreatedAt is when the id was first bound.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when an import last pointed the binding at a template.
	UpdatedAt time.Time `json:"updated_at"`
	// LastCalledAt is when a host last called through the AWX-compatible address with the right
	// key, nil when none ever has.
	LastCalledAt *time.Time `json:"last_called_at,omitempty"`
}

// SameObject reports whether the binding names the AWX job template called name in the
// organization org.
func (b AWXBinding) SameObject(org, name string) bool {
	return b.Organization == org && b.Name == name
}

// BindAWX records that an AWX job template id reaches the template b names, or returns
// ErrAWXConflict when the id is bound to a different AWX object.
func (m *memStore) BindAWX(_ context.Context, b AWXBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.awx == nil {
		m.awx = map[int64]AWXBinding{}
	}
	existing, ok := m.awx[b.AWXID]
	if !ok {
		m.awx[b.AWXID] = cloneBinding(b)
		return nil
	}
	if !existing.SameObject(b.Organization, b.Name) {
		return ErrAWXConflict
	}
	existing.TemplateID = b.TemplateID
	existing.UpdatedAt = b.UpdatedAt
	m.awx[b.AWXID] = existing
	return nil
}

// UnbindAWX removes the binding of b's id while it still names b's AWX object and template.
func (m *memStore) UnbindAWX(_ context.Context, b AWXBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.awx[b.AWXID]; ok && held.SameObject(b.Organization, b.Name) &&
		held.TemplateID == b.TemplateID {
		delete(m.awx, b.AWXID)
	}
	return nil
}

// AWXBindingFor returns the binding of an AWX job template id, or ErrNotFound.
func (m *memStore) AWXBindingFor(_ context.Context, awxID int64) (*AWXBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.awx[awxID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := cloneBinding(b)
	return &cp, nil
}

// AWXBindings returns every binding ordered by AWX id.
func (m *memStore) AWXBindings(_ context.Context) ([]AWXBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWXBinding, 0, len(m.awx))
	for _, b := range m.awx {
		out = append(out, cloneBinding(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AWXID < out[j].AWXID })
	return out, nil
}

// MarkAWXCalled records when a host last called through the AWX-compatible address of awxID.
func (m *memStore) MarkAWXCalled(_ context.Context, awxID int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.awx[awxID]
	if !ok {
		return ErrNotFound
	}
	b.LastCalledAt = &at
	m.awx[awxID] = b
	return nil
}

// cloneBinding copies a binding so a caller cannot reach stored state through its time pointer.
func cloneBinding(b AWXBinding) AWXBinding {
	if b.LastCalledAt != nil {
		at := *b.LastCalledAt
		b.LastCalledAt = &at
	}
	return b
}
