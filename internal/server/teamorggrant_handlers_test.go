package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/team"
)

// errStore is the failure a stub store reports for every call that is configured to fail.
var errStore = errors.New("store is unreachable")

// stubTeams is a team.Store that answers Get from a fixed value and fails whichever calls a test
// asks it to, so a handler's store-error path is reachable without a database behind it.
type stubTeams struct {
	// present is what Get answers with, nil to answer team.ErrNotFound.
	present *team.Team
	// getErr, saveErr, listErr, deleteErr, addErr, removeErr, and membersErr replace the ordinary
	// answer of the method they name.
	getErr     error
	saveErr    error
	listErr    error
	deleteErr  error
	addErr     error
	removeErr  error
	membersErr error
}

// Save reports the configured save failure.
func (s *stubTeams) Save(context.Context, *team.Team) error { return s.saveErr }

// Get answers with the fixed team, its configured error, or team.ErrNotFound.
func (s *stubTeams) Get(context.Context, string) (*team.Team, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, team.ErrNotFound
	}
	return s.present, nil
}

// List reports the configured list failure.
func (s *stubTeams) List(context.Context) ([]*team.Team, error) { return nil, s.listErr }

// Delete reports the configured delete failure.
func (s *stubTeams) Delete(context.Context, string) error { return s.deleteErr }

// AddMember reports the configured add failure.
func (s *stubTeams) AddMember(context.Context, string, string) error { return s.addErr }

// RemoveMember reports the configured remove failure.
func (s *stubTeams) RemoveMember(context.Context, string, string) error { return s.removeErr }

// Members reports the configured members failure.
func (s *stubTeams) Members(context.Context, string) ([]string, error) { return nil, s.membersErr }

// TeamsForUser is unused by these handlers and answers nothing.
func (s *stubTeams) TeamsForUser(context.Context, string) ([]string, error) { return nil, nil }

// stubOrgs is an org.Store shaped like stubTeams, for the organization handlers.
type stubOrgs struct {
	// present is what Get answers with, nil to answer org.ErrNotFound.
	present *org.Org
	// getErr, saveErr, listErr, deleteErr, addErr, removeErr, and membersErr replace the ordinary
	// answer of the method they name.
	getErr     error
	saveErr    error
	listErr    error
	deleteErr  error
	addErr     error
	removeErr  error
	membersErr error
}

// Save reports the configured save failure.
func (s *stubOrgs) Save(context.Context, *org.Org) error { return s.saveErr }

// Get answers with the fixed organization, its configured error, or org.ErrNotFound.
func (s *stubOrgs) Get(context.Context, string) (*org.Org, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, org.ErrNotFound
	}
	return s.present, nil
}

// List reports the configured list failure.
func (s *stubOrgs) List(context.Context) ([]*org.Org, error) { return nil, s.listErr }

// Delete reports the configured delete failure.
func (s *stubOrgs) Delete(context.Context, string) error { return s.deleteErr }

// AddMember reports the configured add failure.
func (s *stubOrgs) AddMember(context.Context, string, string, org.Role) error { return s.addErr }

// RemoveMember reports the configured remove failure.
func (s *stubOrgs) RemoveMember(context.Context, string, string) error { return s.removeErr }

// Members reports the configured members failure.
func (s *stubOrgs) Members(context.Context, string) ([]org.Member, error) {
	return nil, s.membersErr
}

// OrgsForUser is unused by these handlers and answers nothing.
func (s *stubOrgs) OrgsForUser(context.Context, string) ([]org.Membership, error) {
	return nil, nil
}

// stubGrants is a grant.Store that fails whichever calls a test asks it to.
type stubGrants struct {
	// saveErr, listErr, and deleteErr replace the ordinary answer of the method they name.
	saveErr   error
	listErr   error
	deleteErr error
}

// Save reports the configured save failure.
func (s *stubGrants) Save(context.Context, *grant.Grant) error { return s.saveErr }

// Get is unused by these handlers and answers not found.
func (s *stubGrants) Get(context.Context, string) (*grant.Grant, error) {
	return nil, grant.ErrNotFound
}

// List reports the configured list failure.
func (s *stubGrants) List(context.Context) ([]*grant.Grant, error) { return nil, s.listErr }

// Delete reports the configured delete failure.
func (s *stubGrants) Delete(context.Context, string) error { return s.deleteErr }

// ForObject is unused by these handlers and answers nothing.
func (s *stubGrants) ForObject(context.Context, string) ([]*grant.Grant, error) { return nil, nil }

// serveWith runs one request against a server built from opts and returns the recorder.
func serveWith(t *testing.T, method, path, body string, opts ...Option) *httptest.ResponseRecorder {
	t.Helper()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, reader)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestTeamHandlerRefusals pins every way the team endpoints refuse, because the whole family sat
// under ten percent covered and each refusal is the difference between a clear answer and a 500
// that tells an operator nothing. It covers the unconfigured store, the malformed and over-declared
// body, the missing required field, the missing team, and the unreachable store on each verb.
//
//nolint:funlen // Test function.
func TestTeamHandlerRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      team.Store
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Teams are not configured, so creating one says so rather than failing obscurely.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/teams",
		Body: `{"name":"ops"}`, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: A body that is not JSON at all is a bad request, never a server error.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/teams",
		Body: `{`, Store: team.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 2: A field the server does not declare is refused, so a misspelled control cannot
		// pass silently.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/teams",
		Body: `{"name":"ops","nmae":"typo"}`, Store: team.NewMemStore(), //nolint:misspell // The typo is the fixture: it is the unknown field under test.
		WantStatus: http.StatusBadRequest,
	}, { // Test 3: A team with no name is refused rather than stored unnamed.
		Name: "create empty name", Method: http.MethodPost, Path: "/v1/teams",
		Body: `{"name":""}`, Store: team.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 4: A name of only whitespace is accepted, which the handler's empty check does not
		// catch. Pinning it records the behavior rather than assuming a trim happens.
		Name: "create whitespace name", Method: http.MethodPost, Path: "/v1/teams",
		Body: `{"name":"   "}`, Store: team.NewMemStore(), WantStatus: http.StatusCreated,
	}, { // Test 5: An unreachable store on save is a server error, not a bad request.
		Name: "create store fails", Method: http.MethodPost, Path: "/v1/teams",
		Body: `{"name":"ops"}`, Store: &stubTeams{saveErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 6: Listing without teams configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/teams",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 7: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/teams",
		Store: &stubTeams{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 8: Deleting a team that does not exist is a not found, never a success.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/teams/team_missing",
		Store: team.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 9: An unreachable store on delete is a server error.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/teams/team_1",
		Store: &stubTeams{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 10: Deleting without teams configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/teams/team_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 11: Listing the members of a team that does not exist is a not found.
		Name: "members missing team", Method: http.MethodGet, Path: "/v1/teams/team_x/members",
		Store: team.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 12: The team exists but the membership read fails, which is a server error.
		Name: "members store fails", Method: http.MethodGet, Path: "/v1/teams/team_1/members",
		Store:      &stubTeams{present: &team.Team{ID: "team_1"}, membersErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 13: Listing members without teams configured says so.
		Name: "members disabled", Method: http.MethodGet, Path: "/v1/teams/team_1/members",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 14: Adding a member to a team that does not exist is a not found, checked before the
		// body is even read.
		Name: "add member missing team", Method: http.MethodPost, Path: "/v1/teams/team_x/members",
		Body: `{"user_id":"user_1"}`, Store: team.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 15: A membership with no account named is refused.
		Name: "add member empty user", Method: http.MethodPost, Path: "/v1/teams/team_1/members",
		Body: `{"user_id":""}`, Store: &stubTeams{present: &team.Team{ID: "team_1"}},
		WantStatus: http.StatusBadRequest,
	}, { // Test 16: A malformed membership body is a bad request.
		Name: "add member bad body", Method: http.MethodPost, Path: "/v1/teams/team_1/members",
		Body: `nope`, Store: &stubTeams{present: &team.Team{ID: "team_1"}},
		WantStatus: http.StatusBadRequest,
	}, { // Test 17: An unreachable store on add is a server error.
		Name: "add member store fails", Method: http.MethodPost, Path: "/v1/teams/team_1/members",
		Body:       `{"user_id":"user_1"}`,
		Store:      &stubTeams{present: &team.Team{ID: "team_1"}, addErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 18: Adding a member without teams configured says so.
		Name: "add member disabled", Method: http.MethodPost, Path: "/v1/teams/team_1/members",
		Body: `{"user_id":"user_1"}`, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 19: An unreachable store on remove is a server error.
		Name: "remove member store fails", Method: http.MethodDelete,
		Path: "/v1/teams/team_1/members/user_1", Store: &stubTeams{removeErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 20: Removing a member without teams configured says so.
		Name: "remove member disabled", Method: http.MethodDelete,
		Path: "/v1/teams/team_1/members/user_1", Disabled: true, WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithTeams(test.Store))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestTeamRoundTrip proves the team endpoints agree with each other end to end: what create returns
// is what list and members read back, and what delete removes stays gone. A refusal test alone
// cannot catch a create that stores under one id and a read that looks under another.
func TestTeamRoundTrip(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTeams(team.NewMemStore())).Handler()
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := serve(http.MethodPost, "/v1/teams", `{"name":"platform"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	var created team.Team
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created team: %v", err)
	}
	if !strings.HasPrefix(created.ID, "team_") {
		t.Errorf("created id = %q, want a team_ prefix", created.ID)
	}
	if created.CreatedAt.IsZero() {
		t.Error("created team carries no creation time")
	}

	rec = serve(http.MethodPost, "/v1/teams/"+created.ID+"/members", `{"user_id":"user_a"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add member status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	// Adding the same member twice is documented as a no-op, so it must not turn into an error.
	rec = serve(http.MethodPost, "/v1/teams/"+created.ID+"/members", `{"user_id":"user_a"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("repeat add status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}

	rec = serve(http.MethodGet, "/v1/teams/"+created.ID+"/members", "")
	var members membersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if diff := cmp.Diff(membersResponse{Members: []string{"user_a"}, Count: 1}, members,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("members mismatch (-want +got):\n%s", diff)
	}

	rec = serve(http.MethodDelete, "/v1/teams/"+created.ID+"/members/user_a", "")
	if rec.Code != http.StatusOK {
		t.Errorf("remove member status = %d, want 200", rec.Code)
	}
	rec = serve(http.MethodGet, "/v1/teams/"+created.ID+"/members", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode members after remove: %v", err)
	}
	if members.Count != 0 {
		t.Errorf("member count after remove = %d, want 0", members.Count)
	}

	rec = serve(http.MethodDelete, "/v1/teams/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Errorf("delete status = %d, want 200", rec.Code)
	}
	rec = serve(http.MethodGet, "/v1/teams/"+created.ID+"/members", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("members after delete = %d, want 404", rec.Code)
	}
}

// TestOrgHandlerRefusals pins the organization endpoints' refusals for the same reason the team
// ones matter, plus the role check that decides what authority a membership carries. A membership
// role that is neither admin nor member must be refused rather than stored, since the authorizer
// reads it later to decide access.
//
//nolint:funlen // Test function.
func TestOrgHandlerRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      org.Store
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Organizations are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/orgs",
		Body: `{"name":"acme"}`, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/orgs",
		Body: `{"name":`, Store: org.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused rather than dropped.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/orgs",
		Body: `{"name":"acme","owner":"someone"}`, Store: org.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 3: An organization with no name is refused.
		Name: "create empty name", Method: http.MethodPost, Path: "/v1/orgs",
		Body: `{}`, Store: org.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 4: An unreachable store on save is a server error.
		Name: "create store fails", Method: http.MethodPost, Path: "/v1/orgs",
		Body: `{"name":"acme"}`, Store: &stubOrgs{saveErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 5: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/orgs",
		Store: &stubOrgs{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 6: Listing without organizations configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/orgs",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 7: Deleting an organization that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/orgs/org_missing",
		Store: org.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 8: An unreachable store on delete is a server error.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/orgs/org_1",
		Store: &stubOrgs{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 9: Listing the members of an organization that does not exist is a not found.
		Name: "members missing org", Method: http.MethodGet, Path: "/v1/orgs/org_x/members",
		Store: org.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 10: The organization exists but the membership read fails.
		Name: "members store fails", Method: http.MethodGet, Path: "/v1/orgs/org_1/members",
		Store:      &stubOrgs{present: &org.Org{ID: "org_1"}, membersErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 11: Adding a member to an organization that does not exist is a not found.
		Name: "add member missing org", Method: http.MethodPost, Path: "/v1/orgs/org_x/members",
		Body: `{"user_id":"user_1"}`, Store: org.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 12: A membership naming no account is refused.
		Name: "add member empty user", Method: http.MethodPost, Path: "/v1/orgs/org_1/members",
		Body: `{"user_id":""}`, Store: &stubOrgs{present: &org.Org{ID: "org_1"}},
		WantStatus: http.StatusBadRequest,
	}, { // Test 13: A role that is neither admin nor member is refused, so an unreadable authority
		// never reaches the store the authorizer consults.
		Name: "add member bad role", Method: http.MethodPost, Path: "/v1/orgs/org_1/members",
		Body: `{"user_id":"user_1","role":"owner"}`, Store: &stubOrgs{present: &org.Org{ID: "org_1"}},
		WantStatus: http.StatusBadRequest,
	}, { // Test 14: An omitted role defaults to member rather than being refused.
		Name: "add member default role", Method: http.MethodPost, Path: "/v1/orgs/org_1/members",
		Body: `{"user_id":"user_1"}`, Store: &stubOrgs{present: &org.Org{ID: "org_1"}},
		WantStatus: http.StatusCreated,
	}, { // Test 15: An admin membership is accepted.
		Name: "add member admin role", Method: http.MethodPost, Path: "/v1/orgs/org_1/members",
		Body: `{"user_id":"user_1","role":"admin"}`, Store: &stubOrgs{present: &org.Org{ID: "org_1"}},
		WantStatus: http.StatusCreated,
	}, { // Test 16: An unreachable store on add is a server error.
		Name: "add member store fails", Method: http.MethodPost, Path: "/v1/orgs/org_1/members",
		Body:       `{"user_id":"user_1"}`,
		Store:      &stubOrgs{present: &org.Org{ID: "org_1"}, addErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 17: An unreachable store on remove is a server error.
		Name: "remove member store fails", Method: http.MethodDelete,
		Path: "/v1/orgs/org_1/members/user_1", Store: &stubOrgs{removeErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 18: Removing a member without organizations configured says so.
		Name: "remove member disabled", Method: http.MethodDelete,
		Path: "/v1/orgs/org_1/members/user_1", Disabled: true, WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithOrgs(test.Store))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestOrgMemberEchoesTheResolvedRole pins that adding a member reports back the role that was
// actually stored, not the one the caller sent. An omitted role defaults to member, and a caller
// that cannot see which authority it just handed out has no way to notice a default it did not
// intend.
func TestOrgMemberEchoesTheResolvedRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Body     string
		WantRole org.Role
	}{{ // Test 0: No role sent, so the response names the member default.
		Name: "default", Body: `{"user_id":"user_1"}`, WantRole: org.RoleMember,
	}, { // Test 1: An explicit admin role is echoed as admin.
		Name: "admin", Body: `{"user_id":"user_1","role":"admin"}`, WantRole: org.RoleAdmin,
	}, { // Test 2: An explicit member role is echoed as member.
		Name: "member", Body: `{"user_id":"user_1","role":"member"}`, WantRole: org.RoleMember,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := org.NewMemStore()
			if err := store.Save(context.Background(),
				&org.Org{ID: "org_1", Name: "acme"}); err != nil {
				t.Fatalf("seed org: %v", err)
			}
			rec := serveWith(t, http.MethodPost, "/v1/orgs/org_1/members", test.Body,
				WithOrgs(store))
			if rec.Code != http.StatusCreated {
				t.Fatalf("%s: status = %d, want 201 (%q)", test.Name, rec.Code, rec.Body.String())
			}
			var got org.Member
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode member: %v", err)
			}
			if diff := cmp.Diff(org.Member{UserID: "user_1", Role: test.WantRole}, got); diff != "" {
				t.Errorf("%s: member mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestGrantHandlerRefusals pins what the grant endpoint accepts and refuses. Grants decide who may
// use or manage an object, so an accepted subject or object that the authorizer cannot later read
// is a silent access hole, and a refused one that should have been allowed locks a delegation out.
//
// It also pins three shapes the handler's own refusal messages contradict: an org_ subject, a
// queue: object, and the read access level are all accepted, though the messages name only users,
// teams, the four id prefixes, and use or manage.
//
//nolint:funlen // Test function.
func TestGrantHandlerRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      grant.Store
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Grants are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/grants",
		Body: `{"subject":"user_1","object":"tpl_1","access":"use"}`, Disabled: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 1: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/grants",
		Body: `[]`, Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused rather than dropped, which for a grant would mean
		// storing a narrower access than the caller believed they asked for.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"tpl_1","access":"use","expires":"never"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 3: A subject with no recognized prefix is refused.
		Name: "create bad subject", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"casey","object":"tpl_1","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 4: An empty subject is refused.
		Name: "create empty subject", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"","object":"tpl_1","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 5: An object with no recognized prefix is refused.
		Name: "create bad object", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"run_1","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 6: The bare queue prefix names no queue, so it is refused rather than standing for
		// every queue at once.
		Name: "create bare queue object", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"queue:","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 7: A queue object of only whitespace names no queue either.
		Name: "create whitespace queue object", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"queue:   ","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 8: An access level that is not one of the three is refused, so a grant can never
		// carry a level the authorizer ranks at zero.
		Name: "create bad access", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"tpl_1","access":"admin"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 9: An organization subject is accepted, though the refusal message names only users
		// and teams.
		Name: "create org subject", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"org_1","object":"tpl_1","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusCreated,
	}, { // Test 10: A named queue is accepted as an object, though the refusal message names only
		// the four stored id prefixes.
		Name: "create queue object", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"queue:prod","access":"use"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusCreated,
	}, { // Test 11: The read level is accepted, though the refusal message names only use and manage.
		Name: "create read access", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"cred_1","access":"read"}`,
		Store: grant.NewMemStore(), WantStatus: http.StatusCreated,
	}, { // Test 12: An unreachable store on save is a server error.
		Name: "create store fails", Method: http.MethodPost, Path: "/v1/grants",
		Body:  `{"subject":"user_1","object":"tpl_1","access":"use"}`,
		Store: &stubGrants{saveErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 13: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/grants",
		Store: &stubGrants{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 14: Listing without grants configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/grants",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 15: Deleting a grant that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/grants/grant_missing",
		Store: grant.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 16: An unreachable store on delete is a server error, so a revocation that did not
		// happen is never reported as one that did.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/grants/grant_1",
		Store: &stubGrants{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 17: Deleting without grants configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/grants/grant_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithGrants(test.Store, false))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestGrantRoundTrip proves a created grant reads back with the exact subject, object, and access
// it was given. A grant that stores a different access than it reports is the quietest possible
// privilege bug, since the response looks correct and the authorizer reads something else.
func TestGrantRoundTrip(t *testing.T) {
	t.Parallel()
	store := grant.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithGrants(store, false)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/grants",
		strings.NewReader(`{"subject":"team_1","object":"proj_9","access":"manage"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	var created grant.Grant
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created grant: %v", err)
	}
	if created.Subject != "team_1" || created.Object != "proj_9" ||
		created.Access != grant.AccessManage {
		t.Errorf("created grant = %+v, want subject team_1, object proj_9, access manage", created)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/grants", nil))
	var listed listGrantsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode grant list: %v", err)
	}
	if listed.Count != 1 {
		t.Fatalf("grant count = %d, want 1", listed.Count)
	}
	if diff := cmp.Diff(created.Access, listed.Grants[0].Access); diff != "" {
		t.Errorf("stored access mismatch (-want +got):\n%s", diff)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/grants/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("delete status = %d, want 200", rec.Code)
	}
	if _, err := store.Get(context.Background(), created.ID); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("grant still present after delete: %v", err)
	}
}

// TestMembershipReadsDoNotSwallowAStoreFailure demonstrates that the four membership handlers drop a
// store error that is not a missing record and carry on as though the record existed.
//
// Each one writes "if _, err := store.Get(...); errors.Is(err, ErrNotFound) { 404 }" with no branch
// for any other error, so an unreachable database falls straight through. Reading a team's members
// then answers 200 with an empty list, which tells an administrator running an access review that
// the team has nobody in it when the truth is that nobody can see who is in it. Adding a member
// answers 201 without ever confirming the team exists.
//
// The read is the one that matters: "there are no members" and "I cannot check" are opposite answers
// to an access review, and only one of them is safe to act on.
func TestMembershipReadsDoNotSwallowAStoreFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Opt        Option
		WantStatus int
	}{{ // Test 0: An unreachable team store must not read as an empty team.
		Name: "team members", Method: http.MethodGet, Path: "/v1/teams/team_1/members",
		Opt: WithTeams(&stubTeams{getErr: errStore}), WantStatus: http.StatusInternalServerError,
	}, { // Test 1: Adding a member must not be reported as done when the team could not be read.
		Name: "add team member", Method: http.MethodPost, Path: "/v1/teams/team_1/members",
		Body: `{"user_id":"user_1"}`, Opt: WithTeams(&stubTeams{getErr: errStore}),
		WantStatus: http.StatusInternalServerError,
	}, { // Test 2: The organization read has the same shape and the same consequence.
		Name: "org members", Method: http.MethodGet, Path: "/v1/orgs/org_1/members",
		Opt: WithOrgs(&stubOrgs{getErr: errStore}), WantStatus: http.StatusInternalServerError,
	}, { // Test 3: And so does adding an organization member.
		Name: "add org member", Method: http.MethodPost, Path: "/v1/orgs/org_1/members",
		Body: `{"user_id":"user_1"}`, Opt: WithOrgs(&stubOrgs{getErr: errStore}),
		WantStatus: http.StatusInternalServerError,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := serveWith(t, test.Method, test.Path, test.Body, test.Opt)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}
