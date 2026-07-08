// Package user holds accounts and roles. A user owns API tokens; a token authenticates as its
// user and carries the user's role, which the API gate enforces per route.
package user

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
)

// ValidRole reports whether r names a supported role.
func ValidRole(r Role) bool {
	return r == RoleAdmin || r == RoleOperator || r == RoleViewer
}

// User is one account. The password hash never serializes to JSON.
type User struct {
	// ID is the unique user identifier.
	ID string `json:"id"`
	// Username is the unique sign in name.
	Username string `json:"username"`
	// PasswordHash is the bcrypt hash of the password.
	PasswordHash string `json:"-"`
	// Role is the user's permission level.
	Role Role `json:"role"`
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
