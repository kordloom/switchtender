package user_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/user"
)

// saveUser stores u and fails the test if the store refuses it.
func saveUser(t *testing.T, store user.Store, u *user.User) {
	t.Helper()
	if err := store.Save(context.Background(), u); err != nil {
		t.Fatalf("Save(%s) error = %v", u.ID, err)
	}
}

// marshal encodes v as JSON and returns it as a string, so a test can look for what leaked.
func marshal(t *testing.T, v any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(v)
	return string(raw), err
}

// admin returns an administrator account with the given id, the role every access rule turns on.
func admin(id string) *user.User {
	return &user.User{ID: id, Username: id, PasswordHash: "h", Role: user.RoleAdmin,
		CreatedAt: time.Now()}
}

// TestValidRoleAcceptsOnlyTheThreeKnownRoles pins the role allowlist.
//
// Every permission decision in the product reduces to a role comparison, so this function is the gate
// that decides whether an unrecognized role is a role at all. It must be an allowlist rather than a
// denylist: a check that only refused known-bad values would let a typo, an empty string, or a role
// asserted by a directory through as something the rest of the system then compares against, and a
// comparison against a value nobody enumerated fails in whichever direction the caller wrote it.
func TestValidRoleAcceptsOnlyTheThreeKnownRoles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Role       user.Role
		WantResult bool
	}{{ // Test 0: The three documented roles are the whole allowlist.
		Name: "admin", Role: user.RoleAdmin, WantResult: true,
	}, { // Test 1: Operator launches and cancels work.
		Name: "operator", Role: user.RoleOperator, WantResult: true,
	}, { // Test 2: Viewer reads and changes nothing.
		Name: "viewer", Role: user.RoleViewer, WantResult: true,
	}, { // Test 3: The empty role is what an unset field holds and must never be valid.
		Name: "empty", Role: "", WantResult: false,
	}, { // Test 4: An invented role is refused rather than treated as an unknown-but-fine value.
		Name: "invented", Role: "superadmin", WantResult: false,
	}, { // Test 5: Role comparison is case sensitive, so a capitalized role is not the role.
		Name: "capitalized", Role: "Admin", WantResult: false,
	}, { // Test 6: Upper case likewise, which is the form a directory attribute often arrives in.
		Name: "upper case", Role: "ADMIN", WantResult: false,
	}, { // Test 7: Surrounding whitespace is not trimmed away into a valid role.
		Name: "padded", Role: " admin ", WantResult: false,
	}, { // Test 8: A role that merely contains a valid one is not that role.
		Name: "superstring", Role: "administrator", WantResult: false,
	}, { // Test 9: A prefix of a valid role is not that role either.
		Name: "prefix", Role: "adm", WantResult: false,
	}, { // Test 10: A null byte appended does not smuggle a role past the comparison.
		Name: "null byte", Role: "admin\x00", WantResult: false,
	}, { // Test 11: A newline appended, the shape an injected header value takes.
		Name: "newline", Role: "admin\n", WantResult: false,
	}, { // Test 12: A comma separated list is not a role, so no multi-role smuggling.
		Name: "list", Role: "viewer,admin", WantResult: false,
	}, { // Test 13: A unicode look-alike is a different string and must not pass.
		Name: "cyrillic a", Role: "аdmin", WantResult: false,
	}, { // Test 14: A very long value is refused rather than truncated into a match.
		Name: "very long", Role: user.Role("admin" + strings.Repeat("x", 10000)), WantResult: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := user.ValidRole(test.Role); got != test.WantResult {
				t.Errorf("ValidRole(%q) = %v, want %v", test.Role, got, test.WantResult)
			}
		})
	}
}

// TestNewRefusesAnUnknownRoleBeforeItHashesAnything pins that account creation is gated on the role
// allowlist, and that the gate is the same one ValidRole publishes.
//
// New is where an account's permission level is set, so a role that got past here would be stored and
// then compared against by every route gate afterwards. The refusal has to be ErrBadRole so a caller
// can tell a bad role from a hashing failure and report the right thing.
func TestNewRefusesAnUnknownRoleBeforeItHashesAnything(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Role user.Role
		Want error
	}{{ // Test 0: An empty role, the value a request that omitted the field carries.
		Name: "empty", Role: "", Want: user.ErrBadRole,
	}, { // Test 1: An invented role that sounds more privileged than any real one.
		Name: "invented", Role: "root", Want: user.ErrBadRole,
	}, { // Test 2: A capitalized real role, refused because the comparison is exact.
		Name: "wrong case", Role: "Operator", Want: user.ErrBadRole,
	}, { // Test 3: The admin role itself is accepted, or nobody could ever be made one.
		Name: "admin", Role: user.RoleAdmin, Want: nil,
	}, { // Test 4: Operator is accepted.
		Name: "operator", Role: user.RoleOperator, Want: nil,
	}, { // Test 5: Viewer is accepted.
		Name: "viewer", Role: user.RoleViewer, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			u, err := user.New("someone", "a password", test.Role)
			if !errors.Is(err, test.Want) {
				t.Fatalf("New(%q) error = %v, want %v", test.Role, err, test.Want)
			}
			if test.Want != nil {
				if u != nil {
					t.Error("a refused role still produced an account")
				}
				return
			}
			if u.Role != test.Role {
				t.Errorf("Role = %q, want %q", u.Role, test.Role)
			}
		})
	}
}

// TestNewProducesADistinctAccountWithAHashedPassword pins the three properties an account depends on
// at creation: the password is never stored in the clear, the id is unpredictable, and two accounts
// never collide.
//
// A colliding id would mean one account's Save silently replaces another's, handing the second
// account whatever role the first held. A stored plaintext password would turn a database read into
// a credential dump.
func TestNewProducesADistinctAccountWithAHashedPassword(t *testing.T) {
	t.Parallel()
	const password = "correct horse battery staple"
	seen := make(map[string]bool)
	for range 32 {
		u, err := user.New("dispatcher", password, user.RoleOperator)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if !strings.HasPrefix(u.ID, "user_") {
			t.Errorf("id %q does not carry the user_ prefix", u.ID)
		}
		if seen[u.ID] {
			t.Fatalf("New() produced the id %s twice, so one account would overwrite another", u.ID)
		}
		seen[u.ID] = true
		if strings.Contains(u.PasswordHash, password) {
			t.Fatal("the password is recoverable from the stored hash")
		}
		if !strings.HasPrefix(u.PasswordHash, "$2a$") {
			t.Errorf("password hash %q is not a bcrypt hash", u.PasswordHash)
		}
		// Two accounts with the same password must not share a hash, or the database reveals which
		// accounts were given the same password.
		other, err := user.New("dispatcher", password, user.RoleOperator)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if other.PasswordHash == u.PasswordHash {
			t.Fatal("two accounts with the same password share a hash, so bcrypt is unsalted here")
		}
		if u.CreatedAt.IsZero() {
			t.Error("the account has no creation time, so List cannot order it")
		}
	}
}

// TestNewRefusesAPasswordBcryptCannotHold pins that an over-long password is an error rather than a
// silent truncation.
//
// bcrypt reads at most 72 bytes. If the library truncated instead of refusing, two different long
// passwords sharing their first 72 bytes would authenticate each other, so a user who set a long
// passphrase would be authenticated by any prefix-sharing variant of it. The refusal has to reach
// the caller so the account is never created with a password its owner cannot rely on.
func TestNewRefusesAPasswordBcryptCannotHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Password   string
		WantHashed bool
	}{{ // Test 0: An empty password hashes, since refusing it is a policy decision made elsewhere.
		Name: "empty", Password: "", WantHashed: true,
	}, { // Test 1: A single character hashes.
		Name: "one byte", Password: "x", WantHashed: true,
	}, { // Test 2: Exactly the bcrypt limit is accepted.
		Name: "72 bytes", Password: strings.Repeat("a", 72), WantHashed: true,
	}, { // Test 3: One byte past the limit is refused, not quietly cut down to 72.
		Name: "73 bytes", Password: strings.Repeat("a", 73), WantHashed: false,
	}, { // Test 4: A long passphrase a person would plausibly choose is refused loudly.
		Name: "long passphrase",
		Password: "the quick brown fox jumps over the lazy dog and keeps going well past any " +
			"reasonable length", WantHashed: false,
	}, { // Test 5: Multi-byte characters count as bytes, so 24 emoji already exceed the limit.
		Name: "unicode past the byte limit", Password: strings.Repeat("\U0001f511", 24),
		WantHashed: false,
	}, { // Test 6: Eighteen of the same emoji is 72 bytes exactly and is accepted.
		Name: "unicode at the byte limit", Password: strings.Repeat("\U0001f511", 18),
		WantHashed: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			u, err := user.New("someone", test.Password, user.RoleViewer)
			if test.WantHashed {
				if err != nil {
					t.Fatalf("New() error = %v, want the password accepted", err)
				}
				// And the password it hashed is the one that authenticates.
				store := user.NewMemStore()
				saveUser(t, store, u)
				if _, aerr := user.Authenticate(context.Background(), store, "someone",
					test.Password); aerr != nil {
					t.Errorf("Authenticate() error = %v, want the stored password to work", aerr)
				}
				return
			}
			if err == nil {
				t.Fatal("an over-long password was accepted; if bcrypt truncated it, any password " +
					"sharing its first 72 bytes would authenticate this account")
			}
			if u != nil {
				t.Error("a refused password still produced an account")
			}
		})
	}
}

// TestSetPasswordRefusesWhatNewRefusesAndLeavesTheOldHash pins that a password change fails closed.
//
// SetPassword writes into an existing account. If a refused password left the field half written, or
// worse cleared, the account would either keep a hash nobody knows or accept an empty one. The old
// hash has to survive a failed change untouched, so a rejected password change is a no-op rather
// than a lockout.
func TestSetPasswordRefusesWhatNewRefusesAndLeavesTheOldHash(t *testing.T) {
	t.Parallel()
	u, err := user.New("dispatcher", "original password", user.RoleOperator)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	before := u.PasswordHash

	if err := u.SetPassword(strings.Repeat("a", 73)); err == nil {
		t.Fatal("SetPassword accepted a password bcrypt cannot hold")
	}
	if diff := cmp.Diff(before, u.PasswordHash); diff != "" {
		t.Errorf("a refused password change disturbed the stored hash (-want +got):\n%s", diff)
	}

	// A change that is accepted really does replace the hash, and the old password stops working.
	if err := u.SetPassword("a new password"); err != nil {
		t.Fatalf("SetPassword() error = %v", err)
	}
	if u.PasswordHash == before {
		t.Fatal("SetPassword left the old hash in place, so the password never changed")
	}
	store := user.NewMemStore()
	saveUser(t, store, u)
	ctx := context.Background()
	if _, err := user.Authenticate(ctx, store, "dispatcher", "a new password"); err != nil {
		t.Errorf("Authenticate() with the new password error = %v", err)
	}
	if _, err := user.Authenticate(ctx, store, "dispatcher", "original password"); !errors.Is(err,
		user.ErrBadCredentials) {
		t.Errorf("the old password still authenticates: error = %v, want ErrBadCredentials", err)
	}
}

// TestAuthenticateRefusesEveryWrongCredential pins the authentication boundary itself.
//
// This is the function that decides whether a request is a person. Every refusal has to report
// ErrBadCredentials and nothing else, so a caller cannot distinguish a missing account from a wrong
// password and use the difference to enumerate usernames. Each case here is a way an attacker
// probes: a near-miss password, a case-shifted username, an empty value, a very long value.
func TestAuthenticateRefusesEveryWrongCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	u, err := user.New("dispatcher", "correct horse", user.RoleOperator)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	saveUser(t, store, u)
	// An account whose stored hash is not a bcrypt hash at all, which is what a bad import or a
	// directory-provisioned account with no local password leaves behind.
	saveUser(t, store, &user.User{ID: "user_broken", Username: "broken", PasswordHash: "",
		Role: user.RoleAdmin, CreatedAt: time.Now()})
	saveUser(t, store, &user.User{ID: "user_garbage", Username: "garbage",
		PasswordHash: "not-a-hash", Role: user.RoleAdmin, CreatedAt: time.Now()})

	tests := []struct {
		Name     string
		Username string
		Password string
	}{{ // Test 0: The right user with the wrong password.
		Name: "wrong password", Username: "dispatcher", Password: "incorrect horse",
	}, { // Test 1: A password that differs only in case.
		Name: "case shifted password", Username: "dispatcher", Password: "Correct Horse",
	}, { // Test 2: A password with trailing whitespace, which is not trimmed into a match.
		Name: "padded password", Username: "dispatcher", Password: "correct horse ",
	}, { // Test 3: A prefix of the real password must not authenticate.
		Name: "password prefix", Username: "dispatcher", Password: "correct",
	}, { // Test 4: An empty password against a real account.
		Name: "empty password", Username: "dispatcher", Password: "",
	}, { // Test 5: A username that differs only in case is a different account.
		Name: "case shifted username", Username: "Dispatcher", Password: "correct horse",
	}, { // Test 6: A username with surrounding whitespace is not trimmed into a match.
		Name: "padded username", Username: " dispatcher ", Password: "correct horse",
	}, { // Test 7: An account that does not exist.
		Name: "unknown user", Username: "ghost", Password: "anything",
	}, { // Test 8: An empty username, which a request omitting the field sends.
		Name: "empty username", Username: "", Password: "correct horse",
	}, { // Test 9: Both empty, the cheapest probe there is.
		Name: "both empty", Username: "", Password: "",
	}, { // Test 10: An account whose stored hash is empty must never authenticate, least of all
		// with an empty password. This one is an admin, so failing open here hands over the install.
		Name: "empty hash with empty password", Username: "broken", Password: "",
	}, { // Test 11: The same account with any other password.
		Name: "empty hash with a password", Username: "broken", Password: "anything",
	}, { // Test 12: A stored hash that is not bcrypt at all is refused rather than compared as text.
		Name: "garbage hash matched literally", Username: "garbage", Password: "not-a-hash",
	}, { // Test 13: A very long username, which must not be truncated into a match or hang.
		Name: "very long username", Username: strings.Repeat("d", 100000), Password: "correct horse",
	}, { // Test 14: A very long password, past what bcrypt will even read.
		Name: "very long password", Username: "dispatcher", Password: strings.Repeat("c", 100000),
	}, { // Test 15: A null byte in the username does not truncate it into a real one.
		Name: "null byte username", Username: "dispatcher\x00x", Password: "correct horse",
	}, { // Test 16: A SQL wildcard is matched literally rather than as a pattern.
		Name: "wildcard username", Username: "%", Password: "correct horse",
	}, { // Test 17: A unicode look-alike username is a different account.
		Name: "unicode look-alike", Username: "dispаtcher", Password: "correct horse",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := user.Authenticate(ctx, store, test.Username, test.Password)
			if !errors.Is(err, user.ErrBadCredentials) {
				t.Fatalf("Authenticate(%q) error = %v, want ErrBadCredentials", test.Name, err)
			}
			if got != nil {
				t.Errorf("a refused authentication returned account %s with role %q",
					got.ID, got.Role)
			}
		})
	}
}

// TestAuthenticateReturnsTheStoredRoleNotACallerSuppliedOne pins that a successful authentication
// hands back the account as stored, so the role a route gate reads is the one in the database.
//
// The role is the whole permission model. If Authenticate returned anything the caller shaped, or a
// stale copy, a demoted admin would keep admin rights until something else refreshed them.
func TestAuthenticateReturnsTheStoredRoleNotACallerSuppliedOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	u, err := user.New("dispatcher", "correct horse", user.RoleAdmin)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	saveUser(t, store, u)

	got, err := user.Authenticate(ctx, store, "dispatcher", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got.Role != user.RoleAdmin {
		t.Errorf("Role = %q, want admin", got.Role)
	}

	// Demote the account in the store; the next authentication must reflect it.
	demoted := *u
	demoted.Role = user.RoleViewer
	if err := store.Update(ctx, &demoted); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	after, err := user.Authenticate(ctx, store, "dispatcher", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate() after demotion error = %v", err)
	}
	if after.Role != user.RoleViewer {
		t.Errorf("Role after demotion = %q, want viewer: a demoted account kept its admin rights",
			after.Role)
	}
	// And mutating what Authenticate handed back does not promote anybody in the store.
	after.Role = user.RoleAdmin
	fresh, err := store.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if fresh.Role != user.RoleViewer {
		t.Errorf("Role in the store = %q, want viewer: a caller promoted an account by editing a "+
			"value it was handed", fresh.Role)
	}
}

// TestAuthenticateSpendsComparableTimeOnAMissingAccount pins the timing defense.
//
// A missing account skips the bcrypt comparison entirely unless something burns equivalent time, and
// bcrypt at the default cost takes tens of milliseconds. The difference is trivially measurable over
// a network, so an attacker could enumerate valid usernames before ever guessing a password. The
// bound here is loose on purpose: it is checking that the dummy comparison happens at all, not
// racing the clock.
func TestAuthenticateSpendsComparableTimeOnAMissingAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	u, err := user.New("dispatcher", "correct horse", user.RoleOperator)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	saveUser(t, store, u)

	// Median of several attempts, so one scheduling hiccup does not decide the result.
	measure := func(username string) time.Duration {
		t.Helper()
		best := time.Hour
		for range 5 {
			start := time.Now()
			if _, aerr := user.Authenticate(ctx, store, username, "some wrong password"); !errors.Is(
				aerr, user.ErrBadCredentials) {
				t.Fatalf("Authenticate() error = %v, want ErrBadCredentials", aerr)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	existing := measure("dispatcher")
	missing := measure("ghost")

	// A missing account must not be dramatically cheaper than a real one. Anything under a tenth of
	// the real cost means the dummy comparison was skipped and usernames are enumerable.
	if missing*10 < existing {
		t.Errorf("a missing account answered in %s against %s for a real one, so a username can be "+
			"enumerated by timing alone", missing, existing)
	}
}

// TestNormalizeProfileRefusesEveryLinkThatIsNotAWebAddress pins the link allowlist.
//
// Profile links are rendered as anchors in the admin UI, so an account with edit rights choosing its
// own link is choosing what the next administrator clicks. Only http and https with a real host may
// pass. Everything else is refused, because a denylist of known-bad schemes cannot enumerate the
// ones a browser will invent next.
func TestNormalizeProfileRefusesEveryLinkThatIsNotAWebAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Link       string
		WantAccept bool
		WantStored string
	}{{ // Test 0: A javascript URL is the whole reason this check exists.
		Name: "javascript", Link: "javascript:alert(document.cookie)", WantAccept: false,
	}, { // Test 1: Mixed case does not slip past, since url.Parse lowercases the scheme first.
		Name: "javascript mixed case", Link: "JaVaScRiPt:alert(1)", WantAccept: false,
	}, { // Test 2: A data URL can carry a whole HTML document.
		Name: "data", Link: "data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		WantAccept: false,
	}, { // Test 3: A file URL points the administrator's browser at the local disk.
		Name: "file", Link: "file:///etc/passwd", WantAccept: false,
	}, { // Test 4: A vbscript URL, the other historical script scheme.
		Name: "vbscript", Link: "vbscript:msgbox(1)", WantAccept: false,
	}, { // Test 5: An internal application scheme is not a web address.
		Name: "custom scheme", Link: "switchtender://run/destroy", WantAccept: false,
	}, { // Test 6: A protocol-relative link has no scheme and is refused rather than guessed at.
		Name: "protocol relative", Link: "//evil.example.com", WantAccept: false,
	}, { // Test 7: A bare hostname is refused rather than assumed to be https.
		Name: "bare host", Link: "example.com/a", WantAccept: false,
	}, { // Test 8: An https URL with no host at all cannot be an anchor worth following.
		Name: "no host", Link: "https://", WantAccept: false,
	}, { // Test 9: An empty path with three slashes still has no host.
		Name: "empty host with path", Link: "https:///admin", WantAccept: false,
	}, { // Test 10: An opaque http URL carries no host either.
		Name: "opaque http", Link: "http:example.com", WantAccept: false,
	}, { // Test 11: A control character makes the URL unparseable and is refused.
		Name: "control character", Link: "https://example.com/\x00", WantAccept: false,
	}, { // Test 12: A newline, the shape a header injection attempt takes.
		Name: "newline", Link: "https://example.com/\njavascript:alert(1)", WantAccept: false,
	}, { // Test 13: A mailto link is not a web address, though it is harmless.
		Name: "mailto", Link: "mailto:ada@example.com", WantAccept: false,
	}, { // Test 14: A plain https link is the ordinary case and is kept exactly as written.
		Name: "https", Link: "https://wiki.example.com/team/ada", WantAccept: true,
		WantStored: "https://wiki.example.com/team/ada",
	}, { // Test 15: http is allowed too, for an internal directory with no certificate.
		Name: "http", Link: "http://intranet/ada", WantAccept: true, WantStored: "http://intranet/ada",
	}, { // Test 16: An uppercase scheme is a valid https link, since the scheme is case insensitive.
		Name: "uppercase scheme", Link: "HTTPS://example.com/a", WantAccept: true,
		WantStored: "HTTPS://example.com/a",
	}, { // Test 17: A query string with separators survives intact rather than being re-encoded.
		Name: "query string", Link: "https://example.com/p?a=1,2&b=3#frag", WantAccept: true,
		WantStored: "https://example.com/p?a=1,2&b=3#frag",
	}, { // Test 18: A port is part of an ordinary address.
		Name: "port", Link: "https://example.com:8443/a", WantAccept: true,
		WantStored: "https://example.com:8443/a",
	}, { // Test 19: A link at exactly the field limit is accepted, so the bound is inclusive.
		Name: "at the limit", Link: "https://e.example.com/" + strings.Repeat("a", 320-22),
		WantAccept: true, WantStored: "https://e.example.com/" + strings.Repeat("a", 320-22),
	}, { // Test 20: One character past the limit is refused.
		Name: "one past the limit", Link: "https://e.example.com/" + strings.Repeat("a", 320-21),
		WantAccept: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			u := user.User{Links: []string{test.Link}}
			err := u.NormalizeProfile()
			if !test.WantAccept {
				if !errors.Is(err, user.ErrBadProfile) {
					t.Fatalf("NormalizeProfile(%q) error = %v, want ErrBadProfile", test.Link, err)
				}
				// A rejection names the field, never the value: profile fields are personal and
				// this error reaches a log and an API response.
				if strings.Contains(err.Error(), test.Link) {
					t.Errorf("the refusal echoes the value back: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeProfile(%q) error = %v, want it accepted", test.Link, err)
			}
			if diff := cmp.Diff([]string{test.WantStored}, u.Links, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored link mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNormalizeProfileBoundsEveryTextField pins the length limits that keep an account from being
// used as unbounded storage in a column an admin page renders.
//
// The limits are checked in bytes rather than runes, which the cases below state plainly: a name of
// 320 characters is refused when those characters are multi-byte. That is the conservative
// direction, and pinning it means a change to rune counting is a deliberate decision rather than a
// quiet widening of what an account can store.
func TestNormalizeProfileBoundsEveryTextField(t *testing.T) {
	t.Parallel()
	const fieldMax, notesMax = 320, 2000
	tests := []struct {
		Name string
		In   user.User
		Want error
	}{{ // Test 0: A name at exactly the limit is accepted, so the bound is inclusive.
		Name: "name at the limit", In: user.User{FullName: strings.Repeat("a", fieldMax)},
	}, { // Test 1: One character past the limit is refused.
		Name: "name one past", In: user.User{FullName: strings.Repeat("a", fieldMax+1)},
		Want: user.ErrBadProfile,
	}, { // Test 2: An email at the limit is accepted, with the @ the check also requires.
		Name: "email at the limit",
		In:   user.User{Email: strings.Repeat("a", fieldMax-12) + "@example.com"},
	}, { // Test 3: An email one past the limit is refused.
		Name: "email one past",
		In:   user.User{Email: strings.Repeat("a", fieldMax-11) + "@example.com"},
		Want: user.ErrBadProfile,
	}, { // Test 4: A phone number past the limit is refused.
		Name: "phone one past", In: user.User{Phone: strings.Repeat("1", fieldMax+1)},
		Want: user.ErrBadProfile,
	}, { // Test 5: A title past the limit is refused.
		Name: "title one past", In: user.User{Title: strings.Repeat("t", fieldMax+1)},
		Want: user.ErrBadProfile,
	}, { // Test 6: Notes get their own larger bound, and the limit itself is accepted.
		Name: "notes at the limit", In: user.User{Notes: strings.Repeat("n", notesMax)},
	}, { // Test 7: One character past the notes limit is refused.
		Name: "notes one past", In: user.User{Notes: strings.Repeat("n", notesMax+1)},
		Want: user.ErrBadProfile,
	}, { // Test 8: A hugely oversized note is refused rather than truncated into the column.
		Name: "notes far past", In: user.User{Notes: strings.Repeat("n", 1<<20)},
		Want: user.ErrBadProfile,
	}, { // Test 9: The bound counts bytes, so 320 multi-byte characters exceed it.
		Name: "unicode name of 320 characters",
		In:   user.User{FullName: strings.Repeat("é", fieldMax)}, Want: user.ErrBadProfile,
	}, { // Test 10: 160 two-byte characters is exactly 320 bytes and is accepted.
		Name: "unicode name at the byte limit",
		In:   user.User{FullName: strings.Repeat("é", fieldMax/2)},
	}, { // Test 11: Whitespace is trimmed before the length is measured, so padding does not refuse
		// a value that fits.
		Name: "padded to the limit",
		In:   user.User{FullName: "   " + strings.Repeat("a", fieldMax) + "   "},
	}, { // Test 12: An email with no @ is refused whatever its length.
		Name: "email without an at sign", In: user.User{Email: "ada.example.com"},
		Want: user.ErrBadProfile,
	}, { // Test 13: An email that is only whitespace trims to empty, which is allowed since every
		// profile field is optional.
		Name: "email of whitespace only", In: user.User{Email: "   "},
	}, { // Test 14: A wholly empty profile is valid, for an account the CLI or a directory made.
		Name: "empty profile", In: user.User{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			u := test.In
			err := u.NormalizeProfile()
			if !errors.Is(err, test.Want) {
				t.Fatalf("NormalizeProfile() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil {
				return
			}
			// The refusal names which field is wrong without quoting the personal value in it.
			if strings.Contains(err.Error(), strings.Repeat("a", 40)) ||
				strings.Contains(err.Error(), strings.Repeat("n", 40)) {
				t.Errorf("the refusal echoes the field's contents: %v", err)
			}
		})
	}
}

// TestNormalizeProfileBoundsTheLinkList pins the cap on how many links one account carries, counted
// after blank links are dropped.
//
// The count has to be taken on what is kept rather than on what arrived, or a caller could be
// refused for sending trailing blanks a form produced, and the cap has to bite at all or the list is
// unbounded storage rendered on an admin page.
func TestNormalizeProfileBoundsTheLinkList(t *testing.T) {
	t.Parallel()
	link := func(n int) string { return fmt.Sprintf("https://example.com/%d", n) }
	tests := []struct {
		Name      string
		Links     []string
		WantCount int
		Want      error
	}{{ // Test 0: No links at all normalizes to nothing rather than an empty slice.
		Name: "none", Links: nil, WantCount: 0,
	}, { // Test 1: A single link is kept.
		Name: "one", Links: []string{link(0)}, WantCount: 1,
	}, { // Test 2: Exactly the cap is accepted, so the bound is inclusive.
		Name:      "at the cap",
		Links:     []string{link(0), link(1), link(2), link(3), link(4), link(5), link(6), link(7)},
		WantCount: 8,
	}, { // Test 3: One past the cap is refused.
		Name: "one past the cap",
		Links: []string{link(0), link(1), link(2), link(3), link(4), link(5), link(6), link(7),
			link(8)},
		Want: user.ErrBadProfile,
	}, { // Test 4: Blank and whitespace-only links are dropped before the cap is counted, so a form
		// that sent nine slots with one empty is accepted.
		Name: "cap counted after blanks are dropped",
		Links: []string{link(0), "", link(1), "   ", link(2), "\t", link(3), link(4), link(5),
			link(6), link(7), ""},
		WantCount: 8,
	}, { // Test 5: A list of nothing but blanks normalizes to no links rather than failing.
		Name: "all blank", Links: []string{"", "  ", "\t", "\n"}, WantCount: 0,
	}, { // Test 6: One bad link among good ones refuses the whole profile rather than dropping it
		// silently, so the caller learns their link was not stored.
		Name:  "one bad link among good",
		Links: []string{link(0), "javascript:alert(1)", link(1)}, Want: user.ErrBadProfile,
	}, { // Test 7: A very long list is refused on the cap rather than processed in full.
		Name: "far past the cap", Links: make([]string, 0), Want: user.ErrBadProfile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			links := test.Links
			if test.Name == "far past the cap" {
				for i := range 1000 {
					links = append(links, link(i))
				}
			}
			u := user.User{Links: links}
			err := u.NormalizeProfile()
			if !errors.Is(err, test.Want) {
				t.Fatalf("NormalizeProfile() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if got := len(u.Links); got != test.WantCount {
				t.Errorf("kept %d links, want %d", got, test.WantCount)
			}
			if test.WantCount == 0 && u.Links != nil {
				t.Error("an empty link list normalized to an empty slice rather than nil")
			}
		})
	}
}

// TestNormalizeProfileDoesNotTouchIdentityOrPermission pins the boundary of what this function is
// allowed to change.
//
// It is called on a user built from a request body, so if it also normalized the username, the role,
// or the password hash it would be a place a request could reach fields the account model does not
// let a caller set. Its job is the profile and nothing else.
func TestNormalizeProfileDoesNotTouchIdentityOrPermission(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	u := user.User{
		ID: "user_1", Username: "  dispatcher  ", PasswordHash: "  $2a$10$hash  ",
		Role: user.RoleViewer, Source: "  ldap  ", CreatedAt: created,
		FullName: "  Ada  ",
	}
	before := u
	if err := u.NormalizeProfile(); err != nil {
		t.Fatalf("NormalizeProfile() error = %v", err)
	}
	if diff := cmp.Diff(before.ID, u.ID); diff != "" {
		t.Errorf("id changed (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before.Username, u.Username); diff != "" {
		t.Errorf("username changed (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before.PasswordHash, u.PasswordHash); diff != "" {
		t.Errorf("password hash changed (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before.Role, u.Role); diff != "" {
		t.Errorf("role changed (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before.Source, u.Source); diff != "" {
		t.Errorf("source changed (-want +got):\n%s", diff)
	}
	if !u.CreatedAt.Equal(created) {
		t.Errorf("creation time changed to %v", u.CreatedAt)
	}
	// And the profile field it is responsible for was trimmed.
	if diff := cmp.Diff("Ada", u.FullName); diff != "" {
		t.Errorf("full name mismatch (-want +got):\n%s", diff)
	}
}

// TestNormalizeProfileIsIdempotent pins that normalizing an already normalized profile changes
// nothing.
//
// A profile round trips through save, read, edit, and save again. If a second pass altered a value
// that survived the first, a link or a name would drift each time an account was edited, and the
// bounds checked on the way in would be checked against a different value on the way out.
func TestNormalizeProfileIsIdempotent(t *testing.T) {
	t.Parallel()
	u := user.User{
		FullName: "  Ada Lovelace  ", Email: " ada@example.com ", Phone: " +1 555 0100 ",
		Title: " Platform Engineer ", Notes: "  review each quarter  ",
		Links: []string{" https://wiki.example.com/p?a=1,2&b=3 ", "", "https://oncall.example.com"},
	}
	if err := u.NormalizeProfile(); err != nil {
		t.Fatalf("NormalizeProfile() error = %v", err)
	}
	once := u
	if err := u.NormalizeProfile(); err != nil {
		t.Fatalf("second NormalizeProfile() error = %v", err)
	}
	if diff := cmp.Diff(once, u, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a second normalization changed the profile (-want +got):\n%s", diff)
	}
	// And the values really were trimmed the first time.
	if diff := cmp.Diff("Ada Lovelace", u.FullName); diff != "" {
		t.Errorf("full name mismatch (-want +got):\n%s", diff)
	}
	want := []string{"https://wiki.example.com/p?a=1,2&b=3", "https://oncall.example.com"}
	if diff := cmp.Diff(want, u.Links, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("links mismatch (-want +got):\n%s", diff)
	}
}

// TestLastAdminGuardHoldsWhenTheInstallHasOneAdminAndManyOthers pins that the guard counts
// administrators rather than accounts.
//
// An install with one admin and fifty viewers is the ordinary shape. A guard that counted users
// would see fifty survivors and allow the last admin to be deleted, leaving nobody who can reach an
// admin-gated route and no way back except a shell on the host.
func TestLastAdminGuardHoldsWhenTheInstallHasOneAdminAndManyOthers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	saveUser(t, store, admin("user_admin"))
	for i := range 50 {
		saveUser(t, store, &user.User{ID: fmt.Sprintf("user_v%d", i),
			Username: fmt.Sprintf("v%d", i), PasswordHash: "h", Role: user.RoleViewer,
			CreatedAt: time.Now()})
	}
	for i := range 10 {
		saveUser(t, store, &user.User{ID: fmt.Sprintf("user_o%d", i),
			Username: fmt.Sprintf("o%d", i), PasswordHash: "h", Role: user.RoleOperator,
			CreatedAt: time.Now()})
	}

	if ok, err := store.DeleteUnlessLastAdmin(ctx, "user_admin"); err != nil || ok {
		t.Fatalf("deleting the only admin among 60 accounts = (%v, %v), want it refused", ok, err)
	}
	demoted := &user.User{ID: "user_admin", Username: "user_admin", PasswordHash: "h",
		Role: user.RoleOperator}
	if ok, err := store.UpdateUnlessLastAdmin(ctx, demoted); err != nil || ok {
		t.Fatalf("demoting the only admin = (%v, %v), want it refused", ok, err)
	}
	got, err := store.Get(ctx, "user_admin")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Role != user.RoleAdmin {
		t.Errorf("role = %q, want admin", got.Role)
	}
	// A refused demotion must not have applied the other fields either, or a caller could rename
	// an account and change its password through a call that reported it did nothing.
	if got.Username != "user_admin" {
		t.Errorf("username = %q, want it unchanged: a refused update applied part of itself",
			got.Username)
	}
}

// TestLastAdminGuardAllowsAChangeThatKeepsTheRole pins that the guard blocks only the change that
// reaches zero administrators.
//
// The rule is about the count of admins, not about the account being an admin. The only admin must
// still be able to change their own password, name, and profile, or the guard locks the very account
// it exists to protect out of ordinary maintenance.
func TestLastAdminGuardAllowsAChangeThatKeepsTheRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	saveUser(t, store, admin("user_admin"))

	updated := &user.User{ID: "user_admin", Username: "renamed", PasswordHash: "$2a$10$new",
		Role: user.RoleAdmin, FullName: "Ada", Email: "ada@example.com"}
	ok, err := store.UpdateUnlessLastAdmin(ctx, updated)
	if err != nil || !ok {
		t.Fatalf("updating the only admin without demoting = (%v, %v), want it applied", ok, err)
	}
	got, err := store.Get(ctx, "user_admin")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Username != "renamed" || got.PasswordHash != "$2a$10$new" || got.FullName != "Ada" {
		t.Errorf("Get() = %+v, want the update applied", got)
	}
	if got.Role != user.RoleAdmin {
		t.Errorf("role = %q, want admin", got.Role)
	}
}

// TestLastAdminGuardReportsAMissingAccountRatherThanARefusal pins the difference between "there is
// no such account" and "this change would empty the install".
//
// The two answers lead a caller to different places: one is a 404 and one is a 409. Collapsing them
// would make a delete of a typo'd id read as a protected last administrator, which sends an operator
// hunting for an admin that does not exist.
func TestLastAdminGuardReportsAMissingAccountRatherThanARefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	saveUser(t, store, admin("user_admin"))

	if _, err := store.DeleteUnlessLastAdmin(ctx, "user_ghost"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("DeleteUnlessLastAdmin(ghost) error = %v, want ErrNotFound", err)
	}
	ghost := &user.User{ID: "user_ghost", Username: "ghost", Role: user.RoleViewer}
	if _, err := store.UpdateUnlessLastAdmin(ctx, ghost); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("UpdateUnlessLastAdmin(ghost) error = %v, want ErrNotFound", err)
	}
	// The empty id is a caller that sent no id at all and must not match anything.
	if _, err := store.DeleteUnlessLastAdmin(ctx, ""); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("DeleteUnlessLastAdmin(\"\") error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, ""); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("Get(\"\") error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, ""); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("Delete(\"\") error = %v, want ErrNotFound", err)
	}
	if _, err := store.FindByUsername(ctx, ""); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("FindByUsername(\"\") error = %v, want ErrNotFound", err)
	}
	// The guarded admin is still there after all of that.
	if _, err := store.Get(ctx, "user_admin"); err != nil {
		t.Errorf("the administrator went missing: %v", err)
	}
}

// TestPlainDeleteBypassesTheLastAdminGuard pins that Delete is deliberately unguarded, so a caller
// reaching for it is making that choice.
//
// The two methods exist as a pair on purpose: Delete is the primitive and DeleteUnlessLastAdmin is
// the policy. Pinning the difference means a change that quietly moved the guard into Delete, or out
// of the guarded method, is visible here rather than discovered when an API route stops protecting
// the last administrator.
func TestPlainDeleteBypassesTheLastAdminGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	saveUser(t, store, admin("user_admin"))

	if err := store.Delete(ctx, "user_admin"); err != nil {
		t.Fatalf("Delete() error = %v, want the unguarded primitive to remove the last admin", err)
	}
	if _, err := store.Get(ctx, "user_admin"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("Get() after Delete error = %v, want ErrNotFound", err)
	}
	// Update is the unguarded counterpart and demotes the last admin without complaint.
	saveUser(t, store, admin("user_admin2"))
	demoted := &user.User{ID: "user_admin2", Username: "user_admin2", PasswordHash: "h",
		Role: user.RoleViewer}
	if err := store.Update(ctx, demoted); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "user_admin2")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Role != user.RoleViewer {
		t.Errorf("role = %q, want viewer: the unguarded update did not apply", got.Role)
	}
}

// TestStoreNeverSharesMemoryWithItsCallers pins that the store copies on the way in and on the way
// out, for every method that hands back a user.
//
// The store holds live account records including roles. A caller holding a pointer into the store
// could promote itself to admin by assignment, with no call that looks like a permission change and
// nothing in the audit trail. The link slice is the subtle half: copying the struct alone still
// shares the backing array.
func TestStoreNeverSharesMemoryWithItsCallers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	links := []string{"https://a.example.com", "https://b.example.com"}
	original := &user.User{ID: "user_1", Username: "ada", PasswordHash: "h",
		Role: user.RoleViewer, Links: links, CreatedAt: time.Now()}
	saveUser(t, store, original)

	// Mutating what was saved must not reach the store.
	original.Role = user.RoleAdmin
	original.Links[0] = "https://evil.example.com"
	original.Username = "attacker"

	check := func(what string, got *user.User) {
		t.Helper()
		if got.Role != user.RoleViewer {
			t.Errorf("%s: role = %q, want viewer: a caller promoted an account by editing a "+
				"struct it held", what, got.Role)
		}
		if got.Username != "ada" {
			t.Errorf("%s: username = %q, want ada", what, got.Username)
		}
		if diff := cmp.Diff([]string{"https://a.example.com", "https://b.example.com"},
			got.Links, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s: links mismatch (-want +got):\n%s", what, diff)
		}
	}
	got, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	check("after Save then Get", got)

	byName, err := store.FindByUsername(ctx, "ada")
	if err != nil {
		t.Fatalf("FindByUsername() error = %v", err)
	}
	check("FindByUsername", byName)

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() returned %d users, want 1", len(list))
	}
	check("List", list[0])

	// Now mutate everything the store handed out and confirm none of it lands.
	got.Role = user.RoleAdmin
	byName.Role = user.RoleAdmin
	list[0].Role = user.RoleAdmin
	got.Links[0] = "https://evil.example.com"
	byName.Links[1] = "https://evil.example.com"
	list[0].Links[0] = "https://evil.example.com"

	fresh, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	check("after mutating everything handed out", fresh)

	// And an update takes a copy of the caller's link slice rather than adopting it.
	updated := &user.User{ID: "user_1", Username: "ada", PasswordHash: "h",
		Role: user.RoleViewer, Links: []string{"https://c.example.com"}}
	if err := store.Update(ctx, updated); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	updated.Links[0] = "https://evil.example.com"
	after, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff([]string{"https://c.example.com"}, after.Links,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the store adopted the caller's slice (-want +got):\n%s", diff)
	}
}

// TestStoreIsSafeForConcurrentUse pins the Store contract's own promise, under -race.
//
// The interface documents that implementations must be safe for concurrent use, and this store backs
// an HTTP server where several requests touch accounts at once. A race here is not a theoretical
// one: two requests reading and writing a role concurrently is the ordinary case on an admin page.
func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	const workers = 16
	// Two administrators, so the guard has something to keep and something to give.
	saveUser(t, store, admin("user_keep"))
	saveUser(t, store, admin("user_spare"))

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("user_w%d", i)
			u := &user.User{ID: id, Username: id, PasswordHash: "h", Role: user.RoleViewer,
				Links: []string{"https://example.com/" + id}, CreatedAt: time.Now()}
			if err := store.Save(ctx, u); err != nil {
				t.Errorf("Save() error = %v", err)
				return
			}
			if _, err := store.Get(ctx, id); err != nil {
				t.Errorf("Get() error = %v", err)
			}
			if _, err := store.FindByUsername(ctx, id); err != nil {
				t.Errorf("FindByUsername() error = %v", err)
			}
			if _, err := store.List(ctx); err != nil {
				t.Errorf("List() error = %v", err)
			}
			u.Role = user.RoleOperator
			if err := store.Update(ctx, u); err != nil {
				t.Errorf("Update() error = %v", err)
			}
			if _, err := store.UpdateUnlessLastAdmin(ctx, u); err != nil {
				t.Errorf("UpdateUnlessLastAdmin() error = %v", err)
			}
			if _, err := store.DeleteUnlessLastAdmin(ctx, id); err != nil {
				t.Errorf("DeleteUnlessLastAdmin() error = %v", err)
			}
		}(i)
	}
	// Concurrently, everybody tries to delete the same two administrators.
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, id := range []string{"user_keep", "user_spare"} {
				if _, err := store.DeleteUnlessLastAdmin(ctx, id); err != nil &&
					!errors.Is(err, user.ErrNotFound) {
					t.Errorf("DeleteUnlessLastAdmin(%s) error = %v", id, err)
				}
			}
		}()
	}
	wg.Wait()

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	admins := 0
	for _, u := range all {
		if u.Role == user.RoleAdmin {
			admins++
		}
	}
	if admins == 0 {
		t.Error("concurrent deletes emptied the install of administrators, so no admin route is " +
			"reachable and the only way back is a shell on the host")
	}
}

// TestListIsTotallyOrderedEvenWhenTimestampsCollide pins that ordering never depends on map
// iteration.
//
// Accounts created in the same batch, or restored from a backup, share a creation time to whatever
// precision the backend stores. Without a tiebreak the list order would shuffle between calls, which
// makes an access review's export differ every time it is taken and hides whether anything actually
// changed.
func TestListIsTotallyOrderedEvenWhenTimestampsCollide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	same := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"user_d", "user_b", "user_a", "user_c"} {
		saveUser(t, store, &user.User{ID: id, Username: id, PasswordHash: "h",
			Role: user.RoleViewer, CreatedAt: same})
	}
	// One account created earlier must still sort ahead of all of them.
	saveUser(t, store, &user.User{ID: "user_z", Username: "user_z", PasswordHash: "h",
		Role: user.RoleViewer, CreatedAt: same.Add(-time.Hour)})

	want := []string{"user_z", "user_a", "user_b", "user_c", "user_d"}
	for range 20 {
		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		var got []string
		for _, u := range list {
			got = append(got, u.ID)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("List() order is not stable (-want +got):\n%s", diff)
		}
	}
	// An empty store lists nothing rather than failing.
	empty, err := user.NewMemStore().List(ctx)
	if err != nil {
		t.Fatalf("List() on an empty store error = %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("List() on an empty store returned %d users", len(empty))
	}
}

// TestSaveReplacesByIDAndUpdateDoesNotResurrect pins the two write paths against each other.
//
// Save is an upsert keyed on id and Update refuses an id it does not hold. If Update inserted, a
// request naming a deleted account would quietly bring it back with whatever role the request
// carried, which is account creation through an endpoint that reads as an edit.
func TestSaveReplacesByIDAndUpdateDoesNotResurrect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	saveUser(t, store, &user.User{ID: "user_1", Username: "ada", PasswordHash: "h",
		Role: user.RoleViewer, CreatedAt: created})

	// Save replaces wholesale, creation time included, which is what makes it an upsert.
	later := created.Add(24 * time.Hour)
	saveUser(t, store, &user.User{ID: "user_1", Username: "ada2", PasswordHash: "h2",
		Role: user.RoleOperator, Source: "ldap", CreatedAt: later})
	got, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Username != "ada2" || got.Role != user.RoleOperator || !got.CreatedAt.Equal(later) {
		t.Errorf("Get() = %+v, want the replacement", got)
	}
	if got.Source != "ldap" {
		t.Errorf("source = %q, want ldap", got.Source)
	}

	// Update preserves the creation time rather than taking the caller's.
	if err := store.Update(ctx, &user.User{ID: "user_1", Username: "ada3", PasswordHash: "h3",
		Role: user.RoleViewer, CreatedAt: time.Time{}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err = store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CreatedAt.Equal(later) {
		t.Errorf("CreatedAt = %v, want the preserved %v", got.CreatedAt, later)
	}
	// Source is not something an update may change, so a directory identity cannot claim a local
	// account by editing it.
	if got.Source != "ldap" {
		t.Errorf("source = %q after an update that left it empty, want the stored ldap", got.Source)
	}

	// Deleting and then updating must not bring the account back.
	if err := store.Delete(ctx, "user_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Update(ctx, &user.User{ID: "user_1", Username: "ada", PasswordHash: "h",
		Role: user.RoleAdmin}); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("Update() after Delete error = %v, want ErrNotFound; an update that inserts is "+
			"account creation through an endpoint that reads as an edit", err)
	}
	if _, err := store.Get(ctx, "user_1"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("the account came back: Get() error = %v, want ErrNotFound", err)
	}
}

// TestMemStoreEnforcesUsernameUniqueness pins that two accounts cannot share a sign-in name.
//
// It fails. Both SQL backends carry a unique index on username, and the in-memory store carries no
// such rule, so it accepts a duplicate and then resolves FindByUsername by whichever account Go's
// randomized map iteration reaches first. This is the store the shared contract holds up as the
// reference implementation, and Authenticate resolves an incoming username through exactly this
// call, so the role a session is granted would be decided by map order rather than by the account.
func TestMemStoreEnforcesUsernameUniqueness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := user.NewMemStore()
	saveUser(t, store, &user.User{ID: "user_viewer", Username: "shared", PasswordHash: "h",
		Role: user.RoleViewer, CreatedAt: time.Now()})

	err := store.Save(ctx, &user.User{ID: "user_admin", Username: "shared", PasswordHash: "h",
		Role: user.RoleAdmin, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("a second account claimed a username already in use, so which account a sign in " +
			"resolves to is decided by map iteration order")
	}
}

// TestErrorsAreDistinctSentinels pins that the package's errors can be told apart.
//
// Callers map these onto status codes and onto very different messages: not found is a 404, bad
// credentials is a 401 that must not say why, bad role and bad profile are 400s that name the
// problem. If any two compared equal, a caller matching on one would catch the other and report the
// wrong thing, and the credentials error is the one that must never be widened.
func TestErrorsAreDistinctSentinels(t *testing.T) {
	t.Parallel()
	all := []error{user.ErrNotFound, user.ErrBadRole, user.ErrBadCredentials, user.ErrBadProfile}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) is true, so a caller matching one catches the other", a, b)
			}
		}
	}
	// Each wraps recognizably once it has context attached, which is how they travel.
	for _, base := range all {
		wrapped := fmt.Errorf("handling request: %w", base)
		if !errors.Is(wrapped, base) {
			t.Errorf("a wrapped %v no longer matches itself", base)
		}
	}
	// And a bad credentials message never says which half was wrong.
	for _, word := range []string{"password", "username", "not found", "no such"} {
		if strings.Contains(strings.ToLower(user.ErrBadCredentials.Error()), word) {
			t.Errorf("the credentials error mentions %q, which distinguishes a missing account "+
				"from a wrong password", word)
		}
	}
}

// TestUserNeverSerializesItsPasswordHash pins that the account model cannot leak a credential
// through an encoder.
//
// User is returned by API routes and rendered on admin pages, so any encoder reaching it must not
// emit the hash. A bcrypt hash is not a plaintext password, but it is offline-crackable material tied
// to a named account, and publishing it on a route that lists users would hand an attacker every
// credential in the install to work on at their leisure.
func TestUserNeverSerializesItsPasswordHash(t *testing.T) {
	t.Parallel()
	const hash = "$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0q0lyeUnlmDDXBGpZLU7wU8SENTINEL"
	u := user.User{ID: "user_1", Username: "ada", PasswordHash: hash, Role: user.RoleAdmin,
		FullName: "Ada", CreatedAt: time.Now()}
	for _, v := range []any{u, &u, []user.User{u}, []*user.User{&u},
		map[string]user.User{"user": u}, struct {
			Account user.User `json:"account"`
		}{Account: u}} {
		raw, err := marshal(t, v)
		if err != nil {
			t.Fatalf("Marshal(%T) error = %v", v, err)
		}
		if strings.Contains(raw, hash) {
			t.Errorf("encoding %T published the password hash: %s", v, raw)
		}
		if strings.Contains(raw, "password") {
			t.Errorf("encoding %T carries a password member: %s", v, raw)
		}
		// The fields a caller does need are still there, or hiding the hash broke the response.
		if !strings.Contains(raw, `"role":"admin"`) {
			t.Errorf("encoding %T lost the role: %s", v, raw)
		}
		if !strings.Contains(raw, `"username":"ada"`) {
			t.Errorf("encoding %T lost the username: %s", v, raw)
		}
	}
	// The optional profile fields stay out of the encoding when unset, so a bare account does not
	// render a row of empty values on an admin page.
	bare, err := marshal(t, user.User{ID: "user_2", Username: "bot", Role: user.RoleViewer})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, member := range []string{"full_name", "email", "phone", "title", "links", "notes",
		"source"} {
		if strings.Contains(bare, member) {
			t.Errorf("an unset %s was still encoded: %s", member, bare)
		}
	}
}
