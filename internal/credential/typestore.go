package credential

import (
	"context"
	"sort"
	"sync"

	"github.com/kordloom/switchtender/internal/idgen"
)

// TypeStore persists operator-defined credential types. A type carries no secret, so unlike a
// credential it is stored in the clear. Implementations must be safe for concurrent use.
type TypeStore interface {
	// Save inserts or replaces the type identified by t.ID.
	Save(ctx context.Context, t *CredentialType) error
	// Get returns the type with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*CredentialType, error)
	// List returns every type, oldest first.
	List(ctx context.Context) ([]*CredentialType, error)
	// Delete removes the type with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// memTypeStore is an in-memory TypeStore guarded by a mutex.
type memTypeStore struct {
	// mu guards types and order.
	mu sync.RWMutex
	// types maps type id to the stored type.
	types map[string]*CredentialType
}

// NewMemTypeStore returns an empty in-memory TypeStore.
func NewMemTypeStore() TypeStore {
	return &memTypeStore{types: make(map[string]*CredentialType)}
}

// cloneType returns a deep copy so the store never shares a slice or map a caller could mutate.
func cloneType(t *CredentialType) *CredentialType {
	cp := *t
	cp.Fields = append([]Field(nil), t.Fields...)
	if t.EnvInjectors != nil {
		cp.EnvInjectors = make(map[string]string, len(t.EnvInjectors))
		for k, v := range t.EnvInjectors {
			cp.EnvInjectors[k] = v
		}
	}
	if t.ExtraVarInjectors != nil {
		cp.ExtraVarInjectors = make(map[string]string, len(t.ExtraVarInjectors))
		for k, v := range t.ExtraVarInjectors {
			cp.ExtraVarInjectors[k] = v
		}
	}
	return &cp
}

// Save inserts or replaces the type identified by t.ID.
func (m *memTypeStore) Save(_ context.Context, t *CredentialType) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.types[t.ID] = cloneType(t)
	return nil
}

// Get returns the type with the given id, or ErrNotFound.
func (m *memTypeStore) Get(_ context.Context, id string) (*CredentialType, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.types[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneType(t), nil
}

// List returns every type oldest first, tie-broken by id, so it matches the SQL backends exactly.
func (m *memTypeStore) List(_ context.Context) ([]*CredentialType, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*CredentialType, 0, len(m.types))
	for _, t := range m.types {
		out = append(out, cloneType(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Delete removes the type with the given id, or returns ErrNotFound.
func (m *memTypeStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.types[id]; !ok {
		return ErrNotFound
	}
	delete(m.types, id)
	return nil
}

// NewTypeID returns a random credential-type identifier prefixed with "ctype_".
func NewTypeID() string { return idgen.New("ctype_", 6) }
