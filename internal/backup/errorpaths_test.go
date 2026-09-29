package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// errStore is the failure a wrapped store returns, so a test can tell an injected fault from a real
// one.
var errStore = errors.New("database is gone")

// The wrappers below each fail one store operation. A backup reads thirteen stores and a restore
// writes them all, and every one of those calls can fail against a database that is mid-outage,
// which is exactly when somebody is running this command.
type (
	// failCredentials fails listing credentials.
	failCredentials struct{ credential.Store }
	// failCredentialTypes fails listing credential types.
	failCredentialTypes struct{ credential.TypeStore }
	// failProjects fails listing projects.
	failProjects struct{ project.Store }
	// failTemplates fails listing templates.
	failTemplates struct{ template.Store }
	// failInventories fails listing inventories.
	failInventories struct{ inventory.Store }
	// failInvSources fails listing inventory sources.
	failInvSources struct{ invsource.Store }
	// failSchedules fails listing schedules.
	failSchedules struct{ schedule.Store }
	// failTriggers fails listing triggers.
	failTriggers struct{ trigger.Store }
	// failUsers fails listing users.
	failUsers struct{ user.Store }
	// failTokens fails listing tokens.
	failTokens struct{ auth.Store }
	// failTeams fails listing teams.
	failTeams struct{ team.Store }
	// failOrgs fails listing organizations.
	failOrgs struct{ org.Store }
	// failGrants fails listing grants.
	failGrants struct{ grant.Store }
	// failPolicies fails listing policies.
	failPolicies struct{ policy.Store }
	// failTeamMembers lists teams but fails reading their membership.
	failTeamMembers struct{ team.Store }
	// failOrgMembers lists organizations but fails reading their membership.
	failOrgMembers struct{ org.Store }
)

// List fails so a backup cannot read this store.
func (failCredentials) List(context.Context) ([]*credential.Credential, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failCredentialTypes) List(context.Context) ([]*credential.CredentialType, error) {
	return nil, errStore
}

// List fails so a backup cannot read this store.
func (failProjects) List(context.Context) ([]*project.Project, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failTemplates) List(context.Context) ([]*template.Template, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failInventories) List(context.Context) ([]*inventory.Inventory, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failInvSources) List(context.Context) ([]*invsource.Source, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failSchedules) List(context.Context) ([]*schedule.Schedule, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failTriggers) List(context.Context) ([]*trigger.Trigger, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failUsers) List(context.Context) ([]*user.User, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failTokens) List(context.Context) ([]*auth.Token, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failTeams) List(context.Context) ([]*team.Team, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failOrgs) List(context.Context) ([]*org.Org, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failGrants) List(context.Context) ([]*grant.Grant, error) { return nil, errStore }

// List fails so a backup cannot read this store.
func (failPolicies) List(context.Context) ([]*policy.Policy, error) { return nil, errStore }

// Members fails so a backup cannot read who belongs to a team.
func (failTeamMembers) Members(context.Context, string) ([]string, error) { return nil, errStore }

// Members fails so a backup cannot read who belongs to an organization.
func (failOrgMembers) Members(context.Context, string) ([]org.Member, error) { return nil, errStore }

// TestWriteReportsWhichStoreFailed proves a backup that cannot read a store stops, says which one,
// and writes no file.
//
// A backup is only worth having if a partial one is refused rather than kept. The counts in the
// summary are the only thing an operator checks, and a file missing a whole store still writes,
// still reads back, and still reports a tidy set of numbers for everything it did manage to gather.
//
//nolint:funlen // Test function.
func TestWriteReportsWhichStoreFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Break       func(*Stores)
		WantMessage string
	}{{ // Test 0: Credentials.
		Name:        "credentials",
		Break:       func(s *Stores) { s.Credentials = failCredentials{s.Credentials} },
		WantMessage: "list credentials",
	}, { // Test 1: Projects.
		Name:        "projects",
		Break:       func(s *Stores) { s.Projects = failProjects{s.Projects} },
		WantMessage: "list projects",
	}, { // Test 2: Templates.
		Name:        "templates",
		Break:       func(s *Stores) { s.Templates = failTemplates{s.Templates} },
		WantMessage: "list templates",
	}, { // Test 3: Inventories.
		Name:        "inventories",
		Break:       func(s *Stores) { s.Inventories = failInventories{s.Inventories} },
		WantMessage: "list inventories",
	}, { // Test 4: Inventory sources.
		Name:        "inventory sources",
		Break:       func(s *Stores) { s.InventorySources = failInvSources{s.InventorySources} },
		WantMessage: "list inventory sources",
	}, { // Test 5: Schedules.
		Name:        "schedules",
		Break:       func(s *Stores) { s.Schedules = failSchedules{s.Schedules} },
		WantMessage: "list schedules",
	}, { // Test 6: Triggers.
		Name:        "triggers",
		Break:       func(s *Stores) { s.Triggers = failTriggers{s.Triggers} },
		WantMessage: "list triggers",
	}, { // Test 7: Accounts.
		Name:        "users",
		Break:       func(s *Stores) { s.Users = failUsers{s.Users} },
		WantMessage: "list users",
	}, { // Test 8: API tokens.
		Name:        "tokens",
		Break:       func(s *Stores) { s.Tokens = failTokens{s.Tokens} },
		WantMessage: "list tokens",
	}, { // Test 9: Teams.
		Name:        "teams",
		Break:       func(s *Stores) { s.Teams = failTeams{s.Teams} },
		WantMessage: "list teams",
	}, { // Test 10: Organizations.
		Name:        "orgs",
		Break:       func(s *Stores) { s.Orgs = failOrgs{s.Orgs} },
		WantMessage: "list orgs",
	}, { // Test 11: Grants.
		Name:        "grants",
		Break:       func(s *Stores) { s.Grants = failGrants{s.Grants} },
		WantMessage: "list grants",
	}, { // Test 12: Credential types.
		Name:        "credential types",
		Break:       func(s *Stores) { s.CredentialTypes = failCredentialTypes{s.CredentialTypes} },
		WantMessage: "list credential types",
	}, { // Test 13: Approval policies.
		Name:        "policies",
		Break:       func(s *Stores) { s.Policies = failPolicies{s.Policies} },
		WantMessage: "list policies",
	}, { // Test 14: Team membership, which is read separately from the team rows.
		Name:        "team members",
		Break:       func(s *Stores) { s.Teams = failTeamMembers{s.Teams} },
		WantMessage: "list members of team",
	}, { // Test 15: Organization membership.
		Name:        "org members",
		Break:       func(s *Stores) { s.Orgs = failOrgMembers{s.Orgs} },
		WantMessage: "list members of organization",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			src := freshStores()
			fillEverything(t, ctx, src)
			test.Break(&src)

			var buf bytes.Buffer
			sum, err := Write(ctx, src, testSealerOnce(), &buf)
			if err == nil {
				t.Fatalf("%s: a backup that could not read the store still succeeded", test.Name)
			}
			if !errors.Is(err, errStore) {
				t.Errorf("%s: error = %v, want the store failure wrapped", test.Name, err)
			}
			if !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("%s: error = %v, want it to name the store with %q", test.Name, err,
					test.WantMessage)
			}
			if sum != (Summary{}) {
				t.Errorf("%s: a failed backup reported %+v, want nothing", test.Name, sum)
			}
			if buf.Len() != 0 {
				t.Errorf("%s: a failed backup still wrote %d bytes", test.Name, buf.Len())
			}
		})
	}
}

// failSealer is a Sealer that reports itself enabled and then refuses to seal, standing in for a key
// that is present but unusable.
type failSealer struct{ Sealer }

// Enabled reports a key is configured.
func (failSealer) Enabled() bool { return true }

// Seal refuses, as a broken cipher would.
func (failSealer) Seal(string) (string, error) { return "", errStore }

// failWriter is an io.Writer that refuses every write, standing in for a full disk or a closed pipe.
type failWriter struct{}

// Write refuses.
func (failWriter) Write([]byte) (int, error) { return 0, errStore }

// TestWriteReportsSealAndWriteFailures proves the two failures that happen after every store has
// been read are still reported rather than swallowed, and that neither leaves a partial file behind
// that would later be mistaken for a backup.
func TestWriteReportsSealAndWriteFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)

	var buf bytes.Buffer
	sum, err := Write(ctx, src, failSealer{}, &buf)
	if err == nil {
		t.Fatal("a backup whose payload could not be sealed reported success")
	}
	if !strings.Contains(err.Error(), "seal") {
		t.Errorf("error = %v, want it to say the seal failed", err)
	}
	if buf.Len() != 0 {
		t.Errorf("an unsealed backup wrote %d bytes, want none: nothing unsealed reaches disk",
			buf.Len())
	}
	if sum != (Summary{}) {
		t.Errorf("a failed backup reported %+v, want nothing", sum)
	}

	sum, err = Write(ctx, src, testSealerOnce(), failWriter{})
	if err == nil {
		t.Fatal("a backup that could not be written reported success")
	}
	if !errors.Is(err, errStore) {
		t.Errorf("error = %v, want the writer failure wrapped", err)
	}
	if sum != (Summary{}) {
		t.Errorf("a failed write reported %+v, want nothing", sum)
	}
}

// TestReadReportsWhatItWroteWhenApplyFails proves a restore that dies partway returns the counts it
// actually committed together with the error.
//
// A restore is not atomic across stores. Reporting an empty summary alongside the error told the
// operator that nothing had happened at the exact moment something had, and the next thing they do
// is decide whether to retry, restore an older file, or start again from an empty database.
func TestReadReportsWhatItWroteWhenApplyFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	dst := freshStores()
	// Users are written third, after organizations and teams, so failing the first account save
	// leaves those already committed.
	dst.Users = &failingUserStore{Store: user.NewMemStore(), failOn: 1}
	sum, err := Read(ctx, dst, testSealerOnce(), &buf)
	if err == nil {
		t.Fatal("a restore that could not write an account reported success")
	}
	if sum.Orgs == 0 || sum.Teams == 0 {
		t.Errorf("the summary reports %d orgs and %d teams, want the rows the restore really "+
			"wrote before it failed", sum.Orgs, sum.Teams)
	}
	if sum.Users != 0 {
		t.Errorf("Summary.Users = %d, want 0: no account was written", sum.Users)
	}
	if sum.CreatedAt.IsZero() {
		t.Error("the failed restore does not say which snapshot it came from")
	}
	// The stores agree with the summary, so the operator can trust the numbers they were given.
	orgs, err := dst.Orgs.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(orgs) != sum.Orgs {
		t.Errorf("the summary claims %d orgs and the store holds %d", sum.Orgs, len(orgs))
	}
}

// TestCheckRefusesAProfileThatBreachesItsLimits proves a restore applies the same profile bounds the
// API applies, at every boundary.
//
// The users page renders these values, and the limits are what keep a restored profile from
// carrying an address the server would never have accepted or a field large enough to break the
// page that draws it. A restore was the one write path that skipped them.
//
//nolint:funlen // Test function.
func TestCheckRefusesAProfileThatBreachesItsLimits(t *testing.T) {
	t.Parallel()
	longField := strings.Repeat("a", 321)
	tests := []struct {
		Name        string
		User        user.User
		WantRefused bool
	}{{ // Test 0: A full name at the limit is accepted.
		Name: "full name at the limit",
		User: user.User{ID: "u0", Username: "u0", Role: user.RoleViewer,
			FullName: strings.Repeat("a", 320)},
	}, { // Test 1: One character over the limit is refused.
		Name:        "full name over the limit",
		User:        user.User{ID: "u1", Username: "u1", Role: user.RoleViewer, FullName: longField},
		WantRefused: true,
	}, { // Test 2: An address with no at sign is refused.
		Name:        "email without an at sign",
		User:        user.User{ID: "u2", Username: "u2", Role: user.RoleViewer, Email: "not-an-address"},
		WantRefused: true,
	}, { // Test 3: Notes over the limit are refused.
		Name: "notes over the limit",
		User: user.User{ID: "u3", Username: "u3", Role: user.RoleViewer,
			Notes: strings.Repeat("n", 2001)},
		WantRefused: true,
	}, { // Test 4: Eight links is the limit and is accepted.
		Name: "links at the limit",
		User: user.User{ID: "u4", Username: "u4", Role: user.RoleViewer,
			Links: testLinks(8)},
	}, { // Test 5: Nine links is refused.
		Name: "links over the limit",
		User: user.User{ID: "u5", Username: "u5", Role: user.RoleViewer,
			Links: testLinks(9)},
		WantRefused: true,
	}, { // Test 6: A link with no host is refused.
		Name: "link without a host",
		User: user.User{ID: "u6", Username: "u6", Role: user.RoleViewer,
			Links: []string{"https://"}},
		WantRefused: true,
	}, { // Test 7: A file scheme link is refused.
		Name: "file scheme link",
		User: user.User{ID: "u7", Username: "u7", Role: user.RoleViewer,
			Links: []string{"file:///etc/passwd"}},
		WantRefused: true,
	}, { // Test 8: An empty role is not a role.
		Name:        "empty role",
		User:        user.User{ID: "u8", Username: "u8"},
		WantRefused: true,
	}, { // Test 9: A role differing only in case is not a role.
		Name:        "role in the wrong case",
		User:        user.User{ID: "u9", Username: "u9", Role: user.Role("Admin")},
		WantRefused: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := &payload{Users: []userDTO{{User: test.User}}}
			err := check(t.Context(), freshStores(), p)
			switch {
			case test.WantRefused && err == nil:
				t.Errorf("%s: a profile the API would refuse was accepted for restore", test.Name)
			case test.WantRefused && !errors.Is(err, ErrFormat):
				t.Errorf("%s: error = %v, want ErrFormat", test.Name, err)
			case !test.WantRefused && err != nil:
				t.Errorf("%s: a valid profile was refused: %v", test.Name, err)
			}
		})
	}
}

// testLinks returns n distinct valid profile links.
func testLinks(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("https://example.com/%d", i))
	}
	return out
}

// TestCheckRefusesAGrantObjectTheAPIWouldRefuse proves the object side of a grant is bounded on
// restore, including the queue case whose bare prefix would otherwise stand for every queue at once.
//
// A grant names what it opens. An object nothing recognizes fails closed, but a queue grant written
// as the bare prefix is the one shape that would widen: it reads as a queue object and matches no
// single queue, so it is refused at the door rather than stored to be interpreted later.
func TestCheckRefusesAGrantObjectTheAPIWouldRefuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Grant       *grant.Grant
		WantRefused bool
	}{{ // Test 0: A credential object is accepted.
		Name:  "credential object",
		Grant: &grant.Grant{ID: "g0", Subject: "user_1", Object: "cred_1", Access: grant.AccessUse},
	}, { // Test 1: A named queue is accepted.
		Name:  "named queue",
		Grant: &grant.Grant{ID: "g1", Subject: "team_1", Object: "queue:prod", Access: grant.AccessUse},
	}, { // Test 2: The bare queue prefix names no queue and is refused.
		Name:        "bare queue prefix",
		Grant:       &grant.Grant{ID: "g2", Subject: "team_1", Object: "queue:", Access: grant.AccessUse},
		WantRefused: true,
	}, { // Test 3: A queue of nothing but spaces is refused too.
		Name:        "blank queue name",
		Grant:       &grant.Grant{ID: "g3", Subject: "team_1", Object: "queue:   ", Access: grant.AccessUse},
		WantRefused: true,
	}, { // Test 4: An organization is a valid grant subject.
		Name:  "org subject",
		Grant: &grant.Grant{ID: "g4", Subject: "org_1", Object: "proj_1", Access: grant.AccessRead},
	}, { // Test 5: An empty subject is refused.
		Name:        "empty subject",
		Grant:       &grant.Grant{ID: "g5", Object: "proj_1", Access: grant.AccessRead},
		WantRefused: true,
	}, { // Test 6: An empty object is refused.
		Name:        "empty object",
		Grant:       &grant.Grant{ID: "g6", Subject: "user_1", Access: grant.AccessRead},
		WantRefused: true,
	}, { // Test 7: An empty access level is refused.
		Name:        "empty access",
		Grant:       &grant.Grant{ID: "g7", Subject: "user_1", Object: "proj_1"},
		WantRefused: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := check(t.Context(), freshStores(), &payload{Grants: []*grant.Grant{test.Grant}})
			switch {
			case test.WantRefused && err == nil:
				t.Errorf("%s: a grant the API would refuse was accepted for restore", test.Name)
			case !test.WantRefused && err != nil:
				t.Errorf("%s: a valid grant was refused: %v", test.Name, err)
			}
		})
	}
}

// failingFindUserStore reports a fault when the restore looks an account up by name, which is the
// lookup the collision check depends on. A restore must stop there rather than carry on and write
// accounts it could not check.
type failingFindUserStore struct{ user.Store }

// FindByUsername fails.
func (failingFindUserStore) FindByUsername(context.Context, string) (*user.User, error) {
	return nil, errStore
}

// TestCheckStopsWhenItCannotCheck proves a restore refuses when the account collision lookup itself
// fails, rather than treating an unreadable answer as no collision. This is the fail-closed case:
// the check exists precisely because the write it guards cannot be undone.
func TestCheckStopsWhenItCannotCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := freshStores()
	stores.Users = failingFindUserStore{user.NewMemStore()}
	p := &payload{Users: []userDTO{{User: user.User{
		ID: "user_1", Username: "ada", Role: user.RoleAdmin,
	}}}}
	err := check(ctx, stores, p)
	if err == nil {
		t.Fatal("the restore went ahead without being able to check for a name collision")
	}
	if !errors.Is(err, errStore) {
		t.Errorf("error = %v, want the store failure wrapped", err)
	}
	if got, lerr := stores.Users.List(ctx); lerr != nil {
		t.Fatalf("List() error = %v", lerr)
	} else if len(got) != 0 {
		t.Errorf("%d accounts were written despite the check failing", len(got))
	}
}
