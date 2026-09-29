// Package user holds accounts and roles. A user owns API tokens; a token authenticates as its
// user and carries the user's role, which the API gate enforces per route.
package user

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Role names a user's permission level.
type Role string

const (
	// RoleAdmin manages everything, including users, tokens, credentials, and projects.
	RoleAdmin Role = "admin"
	// RoleOperator launches, cancels, and retries work, and reads everything.
	RoleOperator Role = "operator"
	// RoleViewer reads everything and changes nothing.
	RoleViewer Role = "viewer"
)

var (
	// ErrNotFound is returned when a user does not exist in the store.
	ErrNotFound = errors.New("user not found")
	// ErrBadRole is returned when a role is not recognized.
	ErrBadRole = errors.New("unknown role")
	// ErrBadCredentials is returned when a username and password pair does not authenticate.
	ErrBadCredentials = errors.New("bad credentials")
	// ErrBadProfile is returned when a profile field is unusable: too long, or a link that is not an
	// ordinary web address.
	ErrBadProfile = errors.New("invalid profile field")
)

// Profile field bounds. They are generous for real names, addresses, and notes, and exist so an
// account cannot be used to store unbounded text in a column an admin page renders.
const (
	// maxProfileField bounds a single-line profile value such as a name, address, or link.
	maxProfileField = 320
	// maxNotes bounds the free-text note on an account.
	maxNotes = 2000
	// maxLinks bounds how many addresses one account may carry.
	maxLinks = 8
)

// NormalizeProfile trims the profile fields, drops blank links, and checks what is left. It rejects a
// link that is not http or https: profile links are rendered as anchors in the admin UI, so allowing
// another scheme would let an account with edit rights plant a javascript: or data: URL for the next
// admin to click. Personal fields are never logged, so a rejection names the field and not its value.
func (u *User) NormalizeProfile() error {
	u.FullName = strings.TrimSpace(u.FullName)
	u.Email = strings.TrimSpace(u.Email)
	u.Phone = strings.TrimSpace(u.Phone)
	u.Title = strings.TrimSpace(u.Title)
	u.Notes = strings.TrimSpace(u.Notes)
	for name, value := range map[string]string{
		"full_name": u.FullName, "email": u.Email, "phone": u.Phone, "title": u.Title,
	} {
		if len(value) > maxProfileField {
			return fmt.Errorf("%w: %s is longer than %d characters", ErrBadProfile, name, maxProfileField)
		}
	}
	if u.Email != "" && !strings.Contains(u.Email, "@") {
		return fmt.Errorf("%w: email needs an @", ErrBadProfile)
	}
	if len(u.Notes) > maxNotes {
		return fmt.Errorf("%w: notes is longer than %d characters", ErrBadProfile, maxNotes)
	}
	links := make([]string, 0, len(u.Links))
	for _, link := range u.Links {
		link = strings.TrimSpace(link)
		if link == "" {
			continue
		}
		if len(link) > maxProfileField {
			return fmt.Errorf("%w: a link is longer than %d characters", ErrBadProfile, maxProfileField)
		}
		parsed, err := url.Parse(link)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("%w: a link must be an http or https address", ErrBadProfile)
		}
		links = append(links, link)
	}
	if len(links) > maxLinks {
		return fmt.Errorf("%w: more than %d links", ErrBadProfile, maxLinks)
	}
	if len(links) == 0 {
		links = nil
	}
	u.Links = links
	return nil
}

// ValidRole reports whether r names a supported role.
func ValidRole(r Role) bool {
	return r == RoleAdmin || r == RoleOperator || r == RoleViewer
}

// User is one account. The password hash never serializes to JSON. The profile fields carry who the
// account belongs to and how to reach them, which is what an on-call rotation and an access review
// both need and a username alone cannot answer. All of them are optional, so an account created by
// the CLI or provisioned over single sign-on stays valid with none of them set.
type User struct {
	// ID is the unique user identifier.
	ID string `json:"id"`
	// Username is the unique sign in name.
	Username string `json:"username"`
	// PasswordHash is the bcrypt hash of the password.
	PasswordHash string `json:"-"`
	// Role is the user's permission level.
	Role Role `json:"role"`
	// FullName is the person's name as they write it, for display beside the username.
	FullName string `json:"full_name,omitempty"`
	// Email is the address to reach the account, and the target a notification routed to a person
	// resolves to.
	Email string `json:"email,omitempty"`
	// Phone is a contact number for the account, for an escalation that cannot wait on email.
	Phone string `json:"phone,omitempty"`
	// Title is what the person does, for example Platform Engineer. It is descriptive only and
	// carries no permission of its own; Role decides what the account may do.
	Title string `json:"title,omitempty"`
	// Links are addresses that say more about the account: a directory entry, an on-call schedule,
	// a chat handle.
	Links []string `json:"links,omitempty"`
	// Notes is free text about the account, such as why it exists or when it should be reviewed.
	Notes string `json:"notes,omitempty"`
	// CreatedAt is when the user was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists users. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the user identified by u.ID.
	Save(ctx context.Context, u *User) error
	// Update changes an existing user's username, role, and password hash, preserving the creation
	// time, or returns ErrNotFound.
	Update(ctx context.Context, u *User) error
	// Get returns the user with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*User, error)
	// FindByUsername returns the user with the given username, or ErrNotFound.
	FindByUsername(ctx context.Context, username string) (*User, error)
	// List returns all users ordered by creation time, oldest first.
	List(ctx context.Context) ([]*User, error)
	// Delete removes the user with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// DeleteUnlessLastAdmin removes the user with the given id unless doing so would leave the
	// install with no administrator, reporting whether it removed them.
	//
	// Counting the admins and then deleting is two statements, and between them another request can
	// pass the same count. Two concurrent deletes of the last two admins both saw a survivor and
	// both went through, leaving an install with nobody who can reach an admin-gated route and no
	// way back in except a shell on the host. The count and the delete are one statement here so
	// only one of them can win.
	DeleteUnlessLastAdmin(ctx context.Context, id string) (bool, error)
	// UpdateUnlessLastAdmin applies Update unless it would demote the only administrator, reporting
	// whether it applied. It exists for the same reason as DeleteUnlessLastAdmin: demoting is the
	// other way to reach zero admins.
	UpdateUnlessLastAdmin(ctx context.Context, u *User) (bool, error)
}

// New builds a user with a freshly hashed password.
func New(username, password string, role Role) (*User, error) {
	if !ValidRole(role) {
		return nil, ErrBadRole
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &User{
		ID:           "user_" + hex.EncodeToString(b[:]),
		Username:     username,
		PasswordHash: string(hash),
		Role:         role,
		CreatedAt:    time.Now(),
	}, nil
}

// SetPassword replaces the user's password with a freshly hashed one.
func (u *User) SetPassword(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.PasswordHash = string(hash)
	return nil
}

// Authenticate checks a username and password against the store and returns the user.
func Authenticate(ctx context.Context, store Store, username, password string) (*User, error) {
	u, err := store.FindByUsername(ctx, username)
	if err != nil {
		// Burn comparable time so a missing user is indistinguishable from a wrong password.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0q0lyeUnlmDDXBGpZLU7wU8/CG6"), []byte(password))
		return nil, ErrBadCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, ErrBadCredentials
	}
	return u, nil
}
