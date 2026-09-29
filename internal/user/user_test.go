package user_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/usertest"
)

func TestNormalizeProfile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		In        user.User
		WantLinks []string
		WantName  string
		Want      error
	}{{ // Test 0: Whitespace is trimmed and a blank link is dropped.
		Name: "trims",
		In: user.User{
			FullName: "  Ada Lovelace  ", Email: " ada@example.com ", Title: " Engineer ",
			Links: []string{" https://example.com/a ", "", "   "},
		},
		WantName: "Ada Lovelace", WantLinks: []string{"https://example.com/a"},
	}, { // Test 1: No links at all normalizes to nil rather than an empty slice.
		Name:     "no links",
		In:       user.User{FullName: "Ada", Links: []string{}},
		WantName: "Ada",
	}, { // Test 2: A javascript URL is refused, since the admin page renders links as anchors.
		Name: "script link",
		In:   user.User{Links: []string{"javascript:alert(1)"}},
		Want: user.ErrBadProfile,
	}, { // Test 3: A data URL is refused for the same reason.
		Name: "data link",
		In:   user.User{Links: []string{"data:text/html,<script>alert(1)</script>"}},
		Want: user.ErrBadProfile,
	}, { // Test 4: A scheme-less link is refused rather than guessed at.
		Name: "no scheme",
		In:   user.User{Links: []string{"example.com/a"}},
		Want: user.ErrBadProfile,
	}, { // Test 5: An http link with a host is accepted.
		Name:      "http link",
		In:        user.User{Links: []string{"http://example.com/a"}},
		WantLinks: []string{"http://example.com/a"},
	}, { // Test 6: An address with no @ is refused.
		Name: "bad email",
		In:   user.User{Email: "ada.example.com"},
		Want: user.ErrBadProfile,
	}, { // Test 7: An over-long field is refused rather than stored.
		Name: "long name",
		In:   user.User{FullName: strings.Repeat("a", 400)},
		Want: user.ErrBadProfile,
	}, { // Test 8: More links than allowed is refused.
		Name: "too many links",
		In: user.User{Links: []string{
			"https://a.example.com", "https://b.example.com", "https://c.example.com",
			"https://d.example.com", "https://e.example.com", "https://f.example.com",
			"https://g.example.com", "https://h.example.com", "https://i.example.com",
		}},
		Want: user.ErrBadProfile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			u := test.In
			err := u.NormalizeProfile()
			if !errors.Is(err, test.Want) {
				t.Fatalf("NormalizeProfile() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if diff := cmp.Diff(test.WantLinks, u.Links, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("links mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantName, u.FullName); diff != "" {
				t.Errorf("full name mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

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
