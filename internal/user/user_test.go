package user_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/crypto/bcrypt"

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

// TestAgentRoleLowersOnlyAdmin pins the ceiling an agent token runs under. An agent may launch
// and propose work but must never manage identity, access, or secrets, and must never approve its
// own held run. Every one of those is admin, so lowering admin to operator closes the whole surface
// at the door regardless of how the agent reaches the API.
func TestAgentRoleLowersOnlyAdmin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       user.Role
		WantRole user.Role
	}{{ // Test 0: Admin is the one role that is lowered.
		Name: "admin", In: user.RoleAdmin, WantRole: user.RoleOperator,
	}, { // Test 1: Operator is already at the ceiling and is unchanged.
		Name: "operator", In: user.RoleOperator, WantRole: user.RoleOperator,
	}, { // Test 2: A viewer is not raised to the ceiling, only capped by it.
		Name: "viewer", In: user.RoleViewer, WantRole: user.RoleViewer,
	}, { // Test 3: An unrecognized role is left as it is, and the server refuses it anyway.
		Name: "unknown", In: user.Role("wizard"), WantRole: user.Role("wizard"),
	}, { // Test 4: An empty role stays empty rather than becoming an operator.
		Name: "empty", In: user.Role(""), WantRole: user.Role(""),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantRole, user.AgentRole(test.In)); diff != "" {
				t.Errorf("%s: mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestUseMinCostForTestsPanicsOutsideATestBinary builds an ordinary program that asks for the lower
// cost and runs it. A re-run of this test binary cannot stand in, because the linker marks every go
// test binary and testing.Testing reports true in all of them. The program has to die on the panic,
// or a production binary could store every new password at the cheapest hash bcrypt has.
func TestUseMinCostForTestsPanicsOutsideATestBinary(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "mincost")
	build := exec.Command("go", "build", "-o", bin, "./testdata/mincost")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mincost: %v\n%s", err, out)
	}
	out, err := exec.Command(bin).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("mincost error = %v, want the exit of a panic\n%s", err, out)
	}
	if want := "panic: user: UseMinCostForTests called outside a test binary"; !strings.Contains(
		string(out), want) {
		t.Errorf("mincost output = %q, want it to contain %q", out, want)
	}
}

// TestUseMinCostForTestsRefusesOnceTheGuardIsBuilt builds the default guard and then asks for the
// lower cost, which must panic before it changes anything. A guard whose stand-in hash was made at
// the default cost, comparing for a missing username while real accounts verify at the minimum,
// would let timing alone tell the two apart.
func TestUseMinCostForTestsRefusesOnceTheGuardIsBuilt(t *testing.T) {
	t.Parallel()
	guardCost := user.DefaultGuard().Cost()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		user.UseMinCostForTests()
	}()
	if want := "user: UseMinCostForTests called after DefaultGuard was built"; recovered != want {
		t.Fatalf("UseMinCostForTests() recovered %v, want the panic %q", recovered, want)
	}
	u, err := user.New("cost-probe", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	cost, err := bcrypt.Cost([]byte(u.PasswordHash))
	if err != nil {
		t.Fatalf("bcrypt.Cost() error = %v", err)
	}
	if cost != guardCost {
		t.Errorf("user.New hashed at cost %d after the refusal, want the guard's cost %d", cost,
			guardCost)
	}
}
