package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
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

// The wrappers below each refuse one kind of write. A restore upserts into a live database with no
// transaction around it, so any one of these can fail on a disk that filled up, a constraint the
// target install already holds, or a connection that dropped halfway through the recovery.
type (
	// saveFailOrgs refuses to write an organization.
	saveFailOrgs struct{ org.Store }
	// saveFailOrgMembers writes organizations but refuses their membership.
	saveFailOrgMembers struct{ org.Store }
	// saveFailTeams refuses to write a team.
	saveFailTeams struct{ team.Store }
	// saveFailTeamMembers writes teams but refuses their membership.
	saveFailTeamMembers struct{ team.Store }
	// saveFailUsers refuses to write an account.
	saveFailUsers struct{ user.Store }
	// saveFailTokens refuses to write an API token.
	saveFailTokens struct{ auth.Store }
	// saveFailCredentialTypes refuses to write a credential type.
	saveFailCredentialTypes struct{ credential.TypeStore }
	// saveFailGrants refuses to write an access grant.
	saveFailGrants struct{ grant.Store }
	// saveFailPolicies refuses to write an approval policy.
	saveFailPolicies struct{ policy.Store }
	// saveFailProjects refuses to write a project.
	saveFailProjects struct{ project.Store }
	// saveFailCredentials refuses to write a credential.
	saveFailCredentials struct{ credential.Store }
	// saveFailInventories refuses to write an inventory.
	saveFailInventories struct{ inventory.Store }
	// saveFailInvSources refuses to write an inventory source.
	saveFailInvSources struct{ invsource.Store }
	// saveFailTemplates refuses to write a template.
	saveFailTemplates struct{ template.Store }
	// saveFailSchedules refuses to write a schedule.
	saveFailSchedules struct{ schedule.Store }
	// saveFailTriggers refuses to write a trigger.
	saveFailTriggers struct{ trigger.Store }
)

// Save refuses the write.
func (saveFailOrgs) Save(context.Context, *org.Org) error { return errStore }

// AddMember refuses the membership.
func (saveFailOrgMembers) AddMember(context.Context, string, string, org.Role) error {
	return errStore
}

// Save refuses the write.
func (saveFailTeams) Save(context.Context, *team.Team) error { return errStore }

// AddMember refuses the membership.
func (saveFailTeamMembers) AddMember(context.Context, string, string) error { return errStore }

// Save refuses the write.
func (saveFailUsers) Save(context.Context, *user.User) error { return errStore }

// Save refuses the write.
func (saveFailTokens) Save(context.Context, *auth.Token) error { return errStore }

// Save refuses the write.
func (saveFailCredentialTypes) Save(context.Context, *credential.CredentialType) error {
	return errStore
}

// Save refuses the write.
func (saveFailGrants) Save(context.Context, *grant.Grant) error { return errStore }

// Save refuses the write.
func (saveFailPolicies) Save(context.Context, *policy.Policy) error { return errStore }

// Save refuses the write.
func (saveFailProjects) Save(context.Context, *project.Project) error { return errStore }

// Save refuses the write.
func (saveFailCredentials) Save(context.Context, *credential.Credential) error { return errStore }

// Save refuses the write.
func (saveFailInventories) Save(context.Context, *inventory.Inventory) error { return errStore }

// Save refuses the write.
func (saveFailInvSources) Save(context.Context, *invsource.Source) error { return errStore }

// Save refuses the write.
func (saveFailTemplates) Save(context.Context, *template.Template) error { return errStore }

// Save refuses the write.
func (saveFailSchedules) Save(context.Context, *schedule.Schedule) error { return errStore }

// Save refuses the write.
func (saveFailTriggers) Save(context.Context, *trigger.Trigger) error { return errStore }

// TestRestoreStopsAtTheFirstWriteItCannotMake proves every write a restore makes is checked, that
// the failure names the object it could not write, and that the summary still reports what was
// committed before it.
//
// A restore has no transaction around it. The operator running it has already lost the install, and
// the only thing they can act on is the error and the counts. A swallowed write leaves an object
// silently missing from a control plane everyone believes is whole, and a summary of zeros beside a
// real error sends them to rebuild from scratch when most of the recovery had in fact landed.
//
//nolint:funlen // Test function.
func TestRestoreStopsAtTheFirstWriteItCannotMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Break       func(*Stores)
		WantMessage string
		Count       func(Summary) int
		WantCount   int
		WantEarlier bool
	}{{ // Test 0: Organizations are written first, so nothing precedes them.
		Name:        "orgs",
		Break:       func(s *Stores) { s.Orgs = saveFailOrgs{s.Orgs} },
		WantMessage: "save org",
		Count:       func(s Summary) int { return s.Orgs },
	}, { // Test 1: Teams.
		Name:        "teams",
		Break:       func(s *Stores) { s.Teams = saveFailTeams{s.Teams} },
		WantMessage: "save team",
		Count:       func(s Summary) int { return s.Teams },
		WantEarlier: true,
	}, { // Test 2: Accounts.
		Name:        "users",
		Break:       func(s *Stores) { s.Users = saveFailUsers{s.Users} },
		WantMessage: "save user",
		Count:       func(s Summary) int { return s.Users },
		WantEarlier: true,
	}, { // Test 3: API tokens, written after the accounts that own them.
		Name:        "tokens",
		Break:       func(s *Stores) { s.Tokens = saveFailTokens{s.Tokens} },
		WantMessage: "save token",
		Count:       func(s Summary) int { return s.Tokens },
		WantEarlier: true,
	}, { // Test 4: Credential types, written before the credentials that name them.
		Name:        "credential types",
		Break:       func(s *Stores) { s.CredentialTypes = saveFailCredentialTypes{s.CredentialTypes} },
		WantMessage: "save credential type",
		Count:       func(s Summary) int { return s.CredentialTypes },
		WantEarlier: true,
	}, { // Test 5: Access grants.
		Name:        "grants",
		Break:       func(s *Stores) { s.Grants = saveFailGrants{s.Grants} },
		WantMessage: "save grant",
		Count:       func(s Summary) int { return s.Grants },
		WantEarlier: true,
	}, { // Test 6: Team membership.
		Name:        "team members",
		Break:       func(s *Stores) { s.Teams = saveFailTeamMembers{s.Teams} },
		WantMessage: "to team",
		Count:       func(s Summary) int { return s.Memberships },
		WantEarlier: true,
	}, { // Test 7: Organization membership.
		Name:        "org members",
		Break:       func(s *Stores) { s.Orgs = saveFailOrgMembers{s.Orgs} },
		WantMessage: "to organization",
		Count:       func(s Summary) int { return s.Memberships },
		// The team memberships are written first and are counted; only the organization ones
		// are missing.
		WantCount:   3,
		WantEarlier: true,
	}, { // Test 8: Approval policies, the gates in front of every change.
		Name:        "policies",
		Break:       func(s *Stores) { s.Policies = saveFailPolicies{s.Policies} },
		WantMessage: "save policy",
		Count:       func(s Summary) int { return s.Policies },
		WantEarlier: true,
	}, { // Test 9: Projects.
		Name:        "projects",
		Break:       func(s *Stores) { s.Projects = saveFailProjects{s.Projects} },
		WantMessage: "save project",
		Count:       func(s Summary) int { return s.Projects },
		WantEarlier: true,
	}, { // Test 10: Credentials.
		Name:        "credentials",
		Break:       func(s *Stores) { s.Credentials = saveFailCredentials{s.Credentials} },
		WantMessage: "save credential",
		Count:       func(s Summary) int { return s.Credentials },
		WantEarlier: true,
	}, { // Test 11: Inventories.
		Name:        "inventories",
		Break:       func(s *Stores) { s.Inventories = saveFailInventories{s.Inventories} },
		WantMessage: "save inventory",
		Count:       func(s Summary) int { return s.Inventories },
		WantEarlier: true,
	}, { // Test 12: Inventory sources.
		Name:        "inventory sources",
		Break:       func(s *Stores) { s.InventorySources = saveFailInvSources{s.InventorySources} },
		WantMessage: "save inventory source",
		Count:       func(s Summary) int { return s.InventorySources },
		WantEarlier: true,
	}, { // Test 13: Templates.
		Name:        "templates",
		Break:       func(s *Stores) { s.Templates = saveFailTemplates{s.Templates} },
		WantMessage: "save template",
		Count:       func(s Summary) int { return s.Templates },
		WantEarlier: true,
	}, { // Test 14: Schedules.
		Name:        "schedules",
		Break:       func(s *Stores) { s.Schedules = saveFailSchedules{s.Schedules} },
		WantMessage: "save schedule",
		Count:       func(s Summary) int { return s.Schedules },
		WantEarlier: true,
	}, { // Test 15: Triggers.
		Name:        "triggers",
		Break:       func(s *Stores) { s.Triggers = saveFailTriggers{s.Triggers} },
		WantMessage: "save trigger",
		Count:       func(s Summary) int { return s.Triggers },
		WantEarlier: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			src := freshStores()
			fillEverything(t, ctx, src)
			var buf bytes.Buffer
			if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			dst := freshStores()
			test.Break(&dst)
			sum, err := Read(ctx, dst, testSealerOnce(), &buf)
			if err == nil {
				t.Fatalf("%s: a restore that could not write reported success", test.Name)
			}
			if !errors.Is(err, errStore) {
				t.Errorf("%s: error = %v, want the store failure wrapped", test.Name, err)
			}
			if !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("%s: error = %v, want it to name the object with %q", test.Name, err,
					test.WantMessage)
			}
			if got := test.Count(sum); got != test.WantCount {
				t.Errorf("%s: the summary counts %d of these objects, want %d: it must report "+
					"what was written, not what the file held", test.Name, got, test.WantCount)
			}
			if test.WantEarlier && sum.Orgs == 0 {
				t.Errorf("%s: the summary reports no organizations, but they are written first "+
					"and did land: a restore that reports zero at the moment it wrote something "+
					"is the report an operator acts on", test.Name)
			}
			if sum.CreatedAt.IsZero() {
				t.Errorf("%s: the failed restore does not say which snapshot it came from",
					test.Name)
			}
		})
	}
}

// TestReadRefusesBeforeWritingAnything proves a file that fails validation is refused with the
// stores untouched, end to end through Read rather than through check alone.
//
// Users are written near the front of a restore, so the collision this guards against used to be
// found only after the identity tables had been rewritten. Checking through the public entry point
// is what proves the guard is actually wired in front of the write, rather than merely present.
func TestReadRefusesBeforeWritingAnything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// The target already uses one of the backup's account names under a different id, which is what
	// a restore into a rebuilt install looks like.
	dst := freshStores()
	if err := dst.Users.Save(ctx, &user.User{
		ID: "user_rebuilt", Username: "ada", Role: user.RoleAdmin, CreatedAt: testFillTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	before := testObjectCount(t, ctx, dst)

	sum, err := Read(ctx, dst, testSealerOnce(), &buf)
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("Read() error = %v, want ErrFormat", err)
	}
	if !strings.Contains(err.Error(), "collide") {
		t.Errorf("error = %v, want it to explain the collision", err)
	}
	if sum != (Summary{}) {
		t.Errorf("a refused restore reported %+v, want nothing", sum)
	}
	if got := testObjectCount(t, ctx, dst); got != before {
		t.Errorf("a refused restore changed the install: %d objects before, %d after", before, got)
	}
}

// TestWriteRefusesAnObjectItCannotEncode proves a backup that cannot encode its payload fails
// outright rather than writing a shorter file. A truncated snapshot that still parses is the one
// outcome worse than no snapshot, because it restores.
func TestWriteRefusesAnObjectItCannotEncode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	// A value JSON cannot represent. Extra vars are free-form, so the encoder is the only thing
	// standing between an unrepresentable value and a half-written file.
	if err := src.Templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "bad", Playbook: "site.yml",
		ExtraVars: map[string]any{"ratio": math.NaN()}, CreatedAt: testFillTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var buf bytes.Buffer
	sum, err := Write(ctx, src, testSealerOnce(), &buf)
	if err == nil {
		t.Fatal("a payload that cannot be encoded was written as a backup")
	}
	if !strings.Contains(err.Error(), "encode payload") {
		t.Errorf("error = %v, want it to say the payload could not be encoded", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a failed backup wrote %d bytes, want none", buf.Len())
	}
	if sum != (Summary{}) {
		t.Errorf("a failed backup reported %+v, want nothing", sum)
	}
}
