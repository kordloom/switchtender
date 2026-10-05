// Package forgelink ties a person's GitHub or GitLab account to their SwitchTender account, so a
// pull request comment written from that forge account can act as them. A link is keyed on the
// forge's numeric user id and never on the login, because a login can be renamed and then claimed
// by somebody else, and the numeric id cannot.
package forgelink

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/trigger"
)

// Link ties one forge account to one SwitchTender account.
type Link struct {
	// ID identifies the link.
	ID string `json:"id"`
	// UserID is the SwitchTender account the forge account acts as.
	UserID string `json:"user_id"`
	// Provider is github or gitlab.
	Provider string `json:"provider"`
	// APIURL is the forge's REST API base in canonical form, the same base a review trigger names,
	// so GitHub Enterprise Server and self-managed GitLab are told apart from the public services.
	APIURL string `json:"api_url"`
	// ForgeUserID is the forge's numeric id of the account. The login is never stored.
	ForgeUserID int64 `json:"forge_user_id"`
	// CreatedAt is when the link was made.
	CreatedAt time.Time `json:"created_at"`
}

// NewID returns a fresh link id.
func NewID() string {
	return idgen.New("fl_", 12)
}

// CanonicalAPIURL returns the canonical form of a forge's REST API base: the provider's public base
// when apiURL is empty, with the scheme and host lowercased and any trailing slash removed. A link
// and a review trigger that name the same forge compare equal in this form.
func CanonicalAPIURL(provider, apiURL string) string {
	base := strings.TrimSpace(apiURL)
	if base == "" {
		base = trigger.DefaultAPIURL(provider)
	}
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

// Store persists links. Implementations must be safe for concurrent use, and Create must enforce
// both uniqueness rules atomically across every process sharing the store.
type Store interface {
	// Create stores l. It returns ErrLinked when the forge account is already linked to any
	// SwitchTender account, or when l's user already links an account on that forge.
	Create(ctx context.Context, l *Link) error
	// Lookup returns the link of the forge account forgeUserID on the forge provider at apiURL,
	// or ErrNotFound.
	Lookup(ctx context.Context, provider, apiURL string, forgeUserID int64) (*Link, error)
	// ForUser returns the links of the SwitchTender account userID, oldest first.
	ForUser(ctx context.Context, userID string) ([]*Link, error)
	// Delete removes the link id when it belongs to userID and returns what was removed, or
	// ErrNotFound when there is no such link for that account.
	Delete(ctx context.Context, userID, id string) (*Link, error)
	// DeleteUser removes every link of the SwitchTender account userID, for an account that is
	// deleted, and returns how many it removed.
	DeleteUser(ctx context.Context, userID string) (int, error)
}

// memStore is an in-memory Store guarded by a mutex. It serves a single process.
type memStore struct {
	// mu guards links.
	mu sync.Mutex
	// links maps link id to the stored link.
	links map[string]*Link
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{links: map[string]*Link{}}
}

// Create stores l when neither its forge account nor its user's account on that forge is linked.
func (m *memStore) Create(_ context.Context, l *Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.links[l.ID]; ok {
		return ErrLinked
	}
	for _, o := range m.links {
		if o.Provider != l.Provider || o.APIURL != l.APIURL {
			continue
		}
		if o.ForgeUserID == l.ForgeUserID || o.UserID == l.UserID {
			return ErrLinked
		}
	}
	cp := *l
	m.links[l.ID] = &cp
	return nil
}

// Lookup returns a copy of the link of the forge account.
func (m *memStore) Lookup(_ context.Context, provider, apiURL string, forgeUserID int64) (*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.links {
		if o.Provider == provider && o.APIURL == apiURL && o.ForgeUserID == forgeUserID {
			cp := *o
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// ForUser returns copies of the account's links, oldest first.
func (m *memStore) ForUser(_ context.Context, userID string) ([]*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Link{}
	for _, o := range m.links {
		if o.UserID == userID {
			cp := *o
			out = append(out, &cp)
		}
	}
	SortLinks(out)
	return out, nil
}

// Delete removes the link id of userID.
func (m *memStore) Delete(_ context.Context, userID, id string) (*Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.links[id]
	if !ok || o.UserID != userID {
		return nil, ErrNotFound
	}
	delete(m.links, id)
	return o, nil
}

// DeleteUser removes every link of userID.
func (m *memStore) DeleteUser(_ context.Context, userID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, o := range m.links {
		if o.UserID == userID {
			delete(m.links, id)
			n++
		}
	}
	return n, nil
}

// SortLinks orders links oldest first, by creation time and then id, the order ForUser answers in
// on every store.
func SortLinks(list []*Link) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
}

// App is a forge's OAuth application, which a person authorizes to prove which forge account is
// theirs. One App serves one forge, named by its provider and API base, so GitHub, GitHub
// Enterprise Server, GitLab, and self-managed GitLab each take their own.
type App struct {
	// Provider is github or gitlab.
	Provider string
	// WebURL is the forge's web address the person signs in at, such as https://github.com or
	// https://gitlab.example.com.
	WebURL string
	// APIURL is the forge's REST API base in canonical form, the base its review triggers name.
	APIURL string
	// ClientID is the OAuth application's client id.
	ClientID string
	// ClientSecret is the OAuth application's client secret. It is never logged or returned.
	ClientSecret string `json:"-"`
}
