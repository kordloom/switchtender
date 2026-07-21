package user_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/usertest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	usertest.Contract(t, func() user.Store { return user.NewMemStore() })
}

func TestNewAndAuthenticate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()

	u, err := user.New("dispatcher", "correct horse", user.RoleOperator)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if u.PasswordHash == "correct horse" || u.PasswordHash == "" {
		t.Fatal("password stored without hashing")
	}
	if err := store.Save(ctx, u); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := user.Authenticate(ctx, store, "dispatcher", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("Authenticate() = %s, want %s", got.ID, u.ID)
	}

	if _, err := user.Authenticate(ctx, store, "dispatcher", "wrong"); !errors.Is(err, user.ErrBadCredentials) {
		t.Errorf("wrong password error = %v, want ErrBadCredentials", err)
	}
	if _, err := user.Authenticate(ctx, store, "ghost", "any"); !errors.Is(err, user.ErrBadCredentials) {
		t.Errorf("unknown user error = %v, want ErrBadCredentials", err)
	}

	if _, err := user.New("x", "y", user.Role("emperor")); !errors.Is(err, user.ErrBadRole) {
		t.Errorf("bad role error = %v, want ErrBadRole", err)
	}
}
