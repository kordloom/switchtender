package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// stubUsers is a user.Store that answers reads from fixed values and fails whichever calls a test
// asks it to, so the identity handlers' error paths are reachable without a database.
type stubUsers struct {
	// byID is what Get answers with, nil to answer user.ErrNotFound.
	byID *user.User
	// byName is what FindByUsername answers with, nil to answer user.ErrNotFound.
	byName *user.User
	// getErr, findErr, listErr, saveErr, updateErr, and deleteErr replace the ordinary answer of the
	// method they name.
	getErr    error
	findErr   error
	listErr   error
	saveErr   error
	updateErr error
	deleteErr error
	// updateApplied is what UpdateUnlessLastAdmin reports when updateErr is unset.
	updateApplied bool
	// deleteApplied is what DeleteUnlessLastAdmin reports when deleteErr is unset.
	deleteApplied bool
}

// Save reports the configured save failure.
func (s *stubUsers) Save(context.Context, *user.User) error { return s.saveErr }

// Update reports the configured update failure.
func (s *stubUsers) Update(context.Context, *user.User) error { return s.updateErr }

// Get answers with the fixed account, its configured error, or user.ErrNotFound.
func (s *stubUsers) Get(context.Context, string) (*user.User, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.byID == nil {
		return nil, user.ErrNotFound
	}
	return s.byID, nil
}

// FindByUsername answers with the fixed account, its configured error, or user.ErrNotFound.
func (s *stubUsers) FindByUsername(context.Context, string) (*user.User, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.byName == nil {
		return nil, user.ErrNotFound
	}
	return s.byName, nil
}

// List reports the configured list failure.
func (s *stubUsers) List(context.Context) ([]*user.User, error) { return nil, s.listErr }

// Delete reports the configured delete failure.
func (s *stubUsers) Delete(context.Context, string) error { return s.deleteErr }

// DeleteUnlessLastAdmin reports the configured outcome of a guarded delete.
func (s *stubUsers) DeleteUnlessLastAdmin(context.Context, string) (bool, error) {
	return s.deleteApplied, s.deleteErr
}

// UpdateUnlessLastAdmin reports the configured outcome of a guarded update.
func (s *stubUsers) UpdateUnlessLastAdmin(context.Context, *user.User) (bool, error) {
	return s.updateApplied, s.updateErr
}

// stubTokens is an auth.Store that fails whichever calls a test asks it to, and counts as
// unconfigured so the gate leaves the API open.
type stubTokens struct {
	// listErr, saveErr, and deleteErr replace the ordinary answer of the method they name.
	listErr   error
	saveErr   error
	deleteErr error
}

// Save reports the configured save failure.
func (s *stubTokens) Save(context.Context, *auth.Token) error { return s.saveErr }

// List reports the configured list failure.
func (s *stubTokens) List(context.Context) ([]*auth.Token, error) { return nil, s.listErr }

// Touch is a note about a request that already happened and never fails here.
func (s *stubTokens) Touch(context.Context, string, time.Time) error { return nil }

// Delete reports the configured delete failure.
func (s *stubTokens) Delete(context.Context, string) error { return s.deleteErr }

// FindByHash answers not found, so no bearer credential ever authenticates against this store.
func (s *stubTokens) FindByHash(context.Context, string) (*auth.Token, error) {
	return nil, auth.ErrNotFound
}

// Count reports no tokens, which keeps the gate out of enforcing mode for these handler tests.
func (s *stubTokens) Count(context.Context) (int, error) { return 0, nil }

// TestUserHandlerRefusals pins every refusal on the account endpoints. An account is an identity
// with a role, so a create that stores an unvalidated role or a duplicate username is an access
// decision made by accident, and a profile field accepted without bounds is unbounded personal data
// written on an admin's behalf.
//
//nolint:funlen // Test function.
func TestUserHandlerRefusals(t *testing.T) {
	t.Parallel()
	// admin returns a fresh administrator for each table row. The update handler writes the
	// request's fields into the account the store hands back, so a single shared pointer would be
	// mutated by every parallel row that reaches the handler, which the race detector reports and
	// which makes CI a coin flip. Each row gets its own.
	admin := func() *user.User {
		return &user.User{ID: "user_1", Username: "casey", Role: user.RoleAdmin}
	}
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      user.Store
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Accounts are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":"a","password":"b","role":"viewer"}`, Disabled: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 1: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":`, Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused. A misspelled role field would otherwise be
		// dropped and the account created at whatever the zero role validates as.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/users",
		Body:  `{"username":"a","password":"b","rol":"admin"}`,
		Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 3: A username is required.
		Name: "create no username", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"password":"b","role":"viewer"}`, Store: user.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: A password is required, so an account can never be created with an empty one that
		// some other path might then accept.
		Name: "create no password", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":"a","role":"viewer"}`, Store: user.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 5: A role outside the three is refused rather than stored as something roleAllows
		// ranks at zero.
		Name: "create bad role", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":"a","password":"b","role":"superuser"}`, Store: user.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 6: An omitted role is not a viewer default, it is refused, so nobody creates an
		// account without deciding what it may do.
		Name: "create missing role", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":"a","password":"b"}`, Store: user.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 7: A username already taken is a conflict, never a silent overwrite of the existing
		// account.
		Name: "create duplicate username", Method: http.MethodPost, Path: "/v1/users",
		Body:  `{"username":"casey","password":"b","role":"viewer"}`,
		Store: &stubUsers{byName: admin()}, WantStatus: http.StatusConflict,
	}, { // Test 8: An email with no @ is refused by profile normalization.
		Name: "create bad email", Method: http.MethodPost, Path: "/v1/users",
		Body:  `{"username":"a","password":"b","role":"viewer","email":"not-an-address"}`,
		Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 9: A link that is not an http or https address is refused, so a javascript or data
		// URL cannot be stored and later rendered.
		Name: "create bad link", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":"a","password":"b","role":"viewer",` +
			`"links":["javascript:alert(1)"]}`,
		Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 10: An unbounded profile field is refused rather than stored.
		Name: "create oversized name", Method: http.MethodPost, Path: "/v1/users",
		Body: `{"username":"a","password":"b","role":"viewer","full_name":"` +
			strings.Repeat("x", 5000) + `"}`,
		Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 11: An unreachable store on save is a server error.
		Name: "create store fails", Method: http.MethodPost, Path: "/v1/users",
		Body:  `{"username":"a","password":"b","role":"viewer"}`,
		Store: &stubUsers{saveErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 12: Listing without accounts configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/users",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 13: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/users",
		Store: &stubUsers{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 14: Updating without accounts configured says so.
		Name: "update disabled", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body: `{"username":"a","role":"viewer"}`, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 15: A malformed update body is a bad request.
		Name: "update malformed body", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body: `nope`, Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 16: An update with no username is refused.
		Name: "update no username", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body: `{"role":"viewer"}`, Store: user.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 17: An update to a role outside the three is refused.
		Name: "update bad role", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body: `{"username":"a","role":"root"}`, Store: user.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 18: Updating an account that does not exist is a not found.
		Name: "update missing", Method: http.MethodPut, Path: "/v1/users/user_missing",
		Body: `{"username":"a","role":"viewer"}`, Store: &stubUsers{},
		WantStatus: http.StatusNotFound,
	}, { // Test 19: A read failure that is not a missing account is a server error.
		Name: "update read fails", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body: `{"username":"a","role":"viewer"}`, Store: &stubUsers{getErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 20: Renaming an account onto a username another account already holds is a conflict.
		Name: "update duplicate username", Method: http.MethodPut, Path: "/v1/users/user_2",
		Body:       `{"username":"casey","role":"viewer"}`,
		Store:      &stubUsers{byID: &user.User{ID: "user_2"}, byName: admin()},
		WantStatus: http.StatusConflict,
	}, { // Test 21: An update carrying an invalid profile is refused before it is written.
		Name: "update bad profile", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body:  `{"username":"a","role":"viewer","email":"nope"}`,
		Store: &stubUsers{byID: &user.User{ID: "user_1"}}, WantStatus: http.StatusBadRequest,
	}, { // Test 22: Demoting the only administrator is refused as a conflict, because that is the
		// other way to reach an install nobody can administer.
		Name: "update demotes last admin", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body:  `{"username":"casey","role":"viewer"}`,
		Store: &stubUsers{byID: admin(), updateApplied: false}, WantStatus: http.StatusConflict,
	}, { // Test 23: An unreachable store on the guarded update is a server error.
		Name: "update store fails", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body:       `{"username":"casey","role":"viewer"}`,
		Store:      &stubUsers{byID: admin(), updateErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 24: A successful update reports 200.
		Name: "update applied", Method: http.MethodPut, Path: "/v1/users/user_1",
		Body:       `{"username":"casey","role":"admin"}`,
		Store:      &stubUsers{byID: admin(), byName: admin(), updateApplied: true},
		WantStatus: http.StatusOK,
	}, { // Test 25: Deleting without accounts configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/users/user_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 26: Deleting the only administrator is refused as a conflict.
		Name: "delete last admin", Method: http.MethodDelete, Path: "/v1/users/user_1",
		Store: &stubUsers{deleteApplied: false}, WantStatus: http.StatusConflict,
	}, { // Test 27: Deleting an account that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/users/user_missing",
		Store: &stubUsers{deleteErr: user.ErrNotFound}, WantStatus: http.StatusNotFound,
	}, { // Test 28: An unreachable store on delete is a server error, so an account that still has
		// access is never reported as removed.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/users/user_1",
		Store: &stubUsers{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 29: A successful delete reports 200.
		Name: "delete applied", Method: http.MethodDelete, Path: "/v1/users/user_1",
		Store: &stubUsers{deleteApplied: true}, WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithUsers(test.Store))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestUserResponsesNeverCarryPasswordMaterial pins that no account endpoint ever serializes a
// password hash. The hash is marked out of JSON on the model, but that is one struct tag standing
// between an admin-readable list and every account's bcrypt hash going out over the wire, so the
// guarantee is checked at the endpoint rather than assumed from the tag.
func TestUserResponsesNeverCarryPasswordMaterial(t *testing.T) {
	t.Parallel()
	store := user.NewMemStore()
	created, err := user.New("casey", "correct-horse-battery", user.RoleAdmin)
	if err != nil {
		t.Fatalf("build user: %v", err)
	}
	if err := store.Save(context.Background(), created); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if created.PasswordHash == "" {
		t.Fatal("the seeded account carries no hash, so this test would prove nothing")
	}

	for _, path := range []string{"/v1/users"} {
		rec := serveWith(t, http.MethodGet, path, "", WithUsers(store))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, created.PasswordHash) {
			t.Errorf("%s: the response carries the password hash", path)
		}
		if strings.Contains(body, "correct-horse-battery") {
			t.Errorf("%s: the response carries the password", path)
		}
		if strings.Contains(body, "password") {
			t.Errorf("%s: the response mentions a password field: %s", path, body)
		}
	}

	// Creating an account echoes it back, which is the other place a hash could escape.
	rec := serveWith(t, http.MethodPost, "/v1/users",
		`{"username":"drew","password":"another-long-secret","role":"viewer"}`, WithUsers(store))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "another-long-secret") {
		t.Error("the create response echoes the password back")
	}
	if strings.Contains(rec.Body.String(), "$2a$") || strings.Contains(rec.Body.String(), "$2b$") {
		t.Error("the create response carries a bcrypt hash")
	}
}

// TestCreatedAccountIsMarkedLocal pins that an account made by an administrator through the API
// records itself as local. The source is what stops a directory identity that asserts the same
// username from later being handed this administrator's account and role, so an account created
// without it is one an identity provider can claim.
func TestCreatedAccountIsMarkedLocal(t *testing.T) {
	t.Parallel()
	store := user.NewMemStore()
	rec := serveWith(t, http.MethodPost, "/v1/users",
		`{"username":"casey","password":"a-long-enough-secret","role":"admin"}`, WithUsers(store))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	stored, err := store.FindByUsername(context.Background(), "casey")
	if err != nil {
		t.Fatalf("read back account: %v", err)
	}
	if diff := cmp.Diff("local", stored.Source); diff != "" {
		t.Errorf("source mismatch (-want +got):\n%s", diff)
	}
}

// TestUpdateWithABlankPasswordKeepsTheCurrentOne pins the documented behavior that an update sending
// no password leaves the login alone. Treating a blank field as "set the password to empty" would
// silently lock a person out, or worse, leave an account whose password is the empty string.
func TestUpdateWithABlankPasswordKeepsTheCurrentOne(t *testing.T) {
	t.Parallel()
	store := user.NewMemStore()
	first, err := user.New("casey", "the-original-password", user.RoleAdmin)
	if err != nil {
		t.Fatalf("build first user: %v", err)
	}
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatalf("seed first user: %v", err)
	}
	// A second admin exists so the guarded update is not refused for emptying the admins.
	second, err := user.New("drew", "another-password", user.RoleAdmin)
	if err != nil {
		t.Fatalf("build second user: %v", err)
	}
	if err := store.Save(context.Background(), second); err != nil {
		t.Fatalf("seed second user: %v", err)
	}

	rec := serveWith(t, http.MethodPut, "/v1/users/"+first.ID,
		`{"username":"casey","role":"operator","title":"platform"}`, WithUsers(store))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	after, err := store.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("read back account: %v", err)
	}
	if after.PasswordHash != first.PasswordHash {
		t.Error("a blank password field changed the stored password")
	}
	if after.Role != user.RoleOperator {
		t.Errorf("role = %q, want operator", after.Role)
	}
	if _, err := user.Authenticate(context.Background(), store, "casey",
		"the-original-password"); err != nil {
		t.Errorf("the original password stopped working after the update: %v", err)
	}
}

// TestTokenHandlerRefusals pins every refusal on the token endpoints. A token is a durable
// credential, so a mint that skips the account binding produces an unattributable admin credential
// over the network, and a revocation reported as done when it failed leaves live access behind.
//
//nolint:funlen // Test function.
func TestTokenHandlerRefusals(t *testing.T) {
	t.Parallel()
	viewer := &user.User{ID: "user_1", Username: "casey", Role: user.RoleViewer}
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Tokens     auth.Store
		Users      user.Store
		NoTokens   bool
		NoUsers    bool
		WantStatus int
	}{{ // Test 0: Tokens are not configured, so listing says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/tokens",
		NoTokens: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/tokens",
		Tokens: &stubTokens{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 2: Minting needs both a token store and an account store, since a token must name
		// the account it acts as.
		Name: "create without accounts", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"casey"}`, NoUsers: true, WantStatus: http.StatusNotFound,
	}, { // Test 3: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":`, WantStatus: http.StatusBadRequest,
	}, { // Test 4: An undeclared field is refused, so a misspelled kind cannot mint a person's token
		// where an agent's capped one was intended.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"casey","knid":"agent"}`, WantStatus: http.StatusBadRequest,
	}, { // Test 5: A token nobody can identify is a token nobody will revoke, so a name is required.
		Name: "create no name", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"username":"casey"}`, WantStatus: http.StatusBadRequest,
	}, { // Test 6: A token bound to no account would act as admin with nobody behind it, so the API
		// refuses to issue one.
		Name: "create no username", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci"}`, WantStatus: http.StatusBadRequest,
	}, { // Test 7: A kind other than agent is refused rather than stored as a person's token.
		Name: "create bad kind", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"casey","kind":"robot"}`, WantStatus: http.StatusBadRequest,
	}, { // Test 8: A negative lifetime is refused rather than quietly producing a token that is
		// already expired, which is the opposite of a short-lived credential.
		Name: "create negative ttl", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"casey","ttl_hours":-1}`, WantStatus: http.StatusBadRequest,
	}, { // Test 9: Binding to an account that does not exist is a bad request, not a token bound to
		// nothing.
		Name: "create unknown account", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"ghost"}`, Users: &stubUsers{},
		WantStatus: http.StatusBadRequest,
	}, { // Test 10: A read failure on the account store is a server error.
		Name: "create account read fails", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"casey"}`, Users: &stubUsers{findErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 11: An unreachable token store on save is a server error, so a plaintext is never
		// handed out for a token that was not stored.
		Name: "create save fails", Method: http.MethodPost, Path: "/v1/tokens",
		Body: `{"name":"ci","username":"casey"}`, Tokens: &stubTokens{saveErr: errStore},
		Users: &stubUsers{byName: viewer}, WantStatus: http.StatusInternalServerError,
	}, { // Test 12: Revoking without tokens configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/tokens/tok_1",
		NoTokens: true, WantStatus: http.StatusNotFound,
	}, { // Test 13: Revoking a token that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/tokens/tok_missing",
		Tokens: &stubTokens{deleteErr: auth.ErrNotFound}, WantStatus: http.StatusNotFound,
	}, { // Test 14: An unreachable store on revoke is a server error, so live access is never
		// reported as taken back.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/tokens/tok_1",
		Tokens: &stubTokens{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.NoTokens {
				tokens := test.Tokens
				if tokens == nil {
					tokens = &stubTokens{}
				}
				opts = append(opts, WithTokens(tokens))
			}
			if !test.NoUsers {
				users := test.Users
				if users == nil {
					users = &stubUsers{byName: viewer}
				}
				opts = append(opts, WithUsers(users))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestMintedAgentTokenReportsTheCappedRole pins that minting an agent token against an admin account
// reports operator, not admin. The cap is applied at the gate on every request, but the response
// here is what an operator reads to know what they just handed an agent, and a response claiming
// admin would describe a credential that does not exist.
func TestMintedAgentTokenReportsTheCappedRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		AccountRole user.Role
		Kind        string
		WantRole    string
	}{{ // Test 0: An agent token on an admin account is capped to operator.
		Name: "agent on admin", AccountRole: user.RoleAdmin, Kind: `,"kind":"agent"`,
		WantRole: string(user.RoleOperator),
	}, { // Test 1: A person's token on an admin account keeps admin.
		Name: "person on admin", AccountRole: user.RoleAdmin, WantRole: string(user.RoleAdmin),
	}, { // Test 2: An agent token on a viewer account is not raised by the cap.
		Name: "agent on viewer", AccountRole: user.RoleViewer, Kind: `,"kind":"agent"`,
		WantRole: string(user.RoleViewer),
	}, { // Test 3: An agent token on an operator account stays operator.
		Name: "agent on operator", AccountRole: user.RoleOperator, Kind: `,"kind":"agent"`,
		WantRole: string(user.RoleOperator),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			users := &stubUsers{
				byName: &user.User{ID: "user_1", Username: "casey", Role: test.AccountRole},
			}
			body := `{"name":"agent-token","username":"casey"` + test.Kind + `}`
			rec := serveWith(t, http.MethodPost, "/v1/tokens", body,
				WithTokens(auth.NewMemStore()), WithUsers(users))
			if rec.Code != http.StatusCreated {
				t.Fatalf("%s: status = %d, want 201 (%q)", test.Name, rec.Code, rec.Body.String())
			}
			var got createTokenResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode token response: %v", err)
			}
			if diff := cmp.Diff(test.WantRole, got.Role); diff != "" {
				t.Errorf("%s: role mismatch (-want +got):\n%s", test.Name, diff)
			}
			if got.Token == "" {
				t.Errorf("%s: no plaintext returned, so the token is unusable", test.Name)
			}
		})
	}
}

// TestMintedTokenPlaintextIsNeverStored pins the promise the response type makes: the plaintext
// appears in the mint response and nowhere else, ever again. If the stored record carried it, the
// token list, a database backup, and every export would all carry live credentials.
func TestMintedTokenPlaintextIsNeverStored(t *testing.T) {
	t.Parallel()
	store := auth.NewMemStore()
	users := &stubUsers{byName: &user.User{ID: "user_1", Username: "casey", Role: user.RoleOperator}}
	rec := serveWith(t, http.MethodPost, "/v1/tokens", `{"name":"ci","username":"casey"}`,
		WithTokens(store), WithUsers(users))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	var minted createTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if minted.Token == "" {
		t.Fatal("no plaintext returned, so this test would prove nothing")
	}
	stored, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("encode stored tokens: %v", err)
	}
	if strings.Contains(string(encoded), minted.Token) {
		t.Error("the stored token record carries the plaintext")
	}
	// The listing endpoint is the other place it could escape.
	listed := serveWith(t, http.MethodGet, "/v1/tokens", "", WithTokens(store), WithUsers(users))
	if strings.Contains(listed.Body.String(), minted.Token) {
		t.Error("the token list carries the plaintext")
	}
	// The credential still has to work, which is what proves the hash was stored rather than nothing.
	if _, err := store.FindByHash(context.Background(), auth.HashToken(minted.Token)); err != nil {
		t.Errorf("the minted token does not resolve by hash: %v", err)
	}
}
