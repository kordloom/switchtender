package sqlitestore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

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

// TestEveryDeleteReportsAMissingRow pins that deleting something that is not there is an error
// rather than a success on every store that has a delete. The API turns these into a 404, so a
// store that reported success would answer 200 for deleting an object that never existed, which
// hides a typo in an id and makes an idempotent-looking client silently wrong about what it removed.
func TestEveryDeleteReportsAMissingRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openStore(t)

	tests := []struct {
		Name string
		Call func() error
		Want error
	}{{ // Test 0: A schedule.
		Name: "schedule", Want: schedule.ErrNotFound,
		Call: func() error { return db.Schedules().Delete(ctx, "ghost") },
	}, { // Test 1: An API token.
		Name: "token", Want: auth.ErrNotFound,
		Call: func() error { return db.Tokens().Delete(ctx, "ghost") },
	}, { // Test 2: An execution secret.
		Name: "credential", Want: credential.ErrNotFound,
		Call: func() error { return db.Credentials().Delete(ctx, "ghost") },
	}, { // Test 3: An operator-defined credential type.
		Name: "credential type", Want: credential.ErrNotFound,
		Call: func() error { return db.CredentialTypes().Delete(ctx, "ghost") },
	}, { // Test 4: A git project.
		Name: "project", Want: project.ErrNotFound,
		Call: func() error { return db.Projects().Delete(ctx, "ghost") },
	}, { // Test 5: A job template.
		Name: "template", Want: template.ErrNotFound,
		Call: func() error { return db.Templates().Delete(ctx, "ghost") },
	}, { // Test 6: An account.
		Name: "user", Want: user.ErrNotFound,
		Call: func() error { return db.Users().Delete(ctx, "ghost") },
	}, { // Test 7: A stored inventory.
		Name: "inventory", Want: inventory.ErrNotFound,
		Call: func() error { return db.Inventories().Delete(ctx, "ghost") },
	}, { // Test 8: A dynamic inventory source.
		Name: "inventory source", Want: invsource.ErrNotFound,
		Call: func() error { return db.InventorySources().Delete(ctx, "ghost") },
	}, { // Test 9: A webhook trigger.
		Name: "trigger", Want: trigger.ErrNotFound,
		Call: func() error { return db.Triggers().Delete(ctx, "ghost") },
	}, { // Test 10: A team.
		Name: "team", Want: team.ErrNotFound,
		Call: func() error { return db.Teams().Delete(ctx, "ghost") },
	}, { // Test 11: An organization.
		Name: "org", Want: org.ErrNotFound,
		Call: func() error { return db.Orgs().Delete(ctx, "ghost") },
	}, { // Test 12: An access grant.
		Name: "grant", Want: grant.ErrNotFound,
		Call: func() error { return db.Grants().Delete(ctx, "ghost") },
	}, { // Test 13: An approval policy.
		Name: "policy", Want: policy.ErrNotFound,
		Call: func() error { return db.Policies().Delete(ctx, "ghost") },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Call(); !errors.Is(err, test.Want) {
				t.Errorf("deleting a missing %s = %v, want %v", test.Name, err, test.Want)
			}
		})
	}
}

// TestEveryUpdateRefusesARowThatIsGone pins the difference between the upsert Save makes and the
// narrow Update beside it. Update exists so an edit racing a delete cannot re-create what was
// deleted, which is what Save would do. A store where Update quietly inserted would resurrect an
// object somebody removed, and for a credential or a policy that is an access decision coming back
// from the dead.
func TestEveryUpdateRefusesARowThatIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openStore(t)

	tests := []struct {
		Name string
		Call func() error
		Want error
	}{{ // Test 0: A schedule.
		Name: "schedule", Want: schedule.ErrNotFound,
		Call: func() error {
			return db.Schedules().Update(ctx, &schedule.Schedule{ID: "ghost", Cron: "0 0 * * *"})
		},
	}, { // Test 1: An execution secret.
		Name: "credential", Want: credential.ErrNotFound,
		Call: func() error {
			return db.Credentials().Update(ctx,
				&credential.Credential{ID: "ghost", Kind: "ssh", Secret: "x"})
		},
	}, { // Test 2: A git project.
		Name: "project", Want: project.ErrNotFound,
		Call: func() error {
			return db.Projects().Update(ctx, &project.Project{ID: "ghost", RepoURL: "https://x"})
		},
	}, { // Test 3: A job template.
		Name: "template", Want: template.ErrNotFound,
		Call: func() error {
			return db.Templates().Update(ctx, &template.Template{ID: "ghost", Playbook: "p.yml"})
		},
	}, { // Test 4: An account.
		Name: "user", Want: user.ErrNotFound,
		Call: func() error {
			return db.Users().Update(ctx, &user.User{ID: "ghost", Username: "x", Role: user.RoleAdmin})
		},
	}, { // Test 5: A stored inventory.
		Name: "inventory", Want: inventory.ErrNotFound,
		Call: func() error {
			return db.Inventories().Update(ctx, &inventory.Inventory{ID: "ghost", Content: "x"})
		},
	}, { // Test 6: A dynamic inventory source.
		Name: "inventory source", Want: invsource.ErrNotFound,
		Call: func() error {
			return db.InventorySources().Update(ctx, &invsource.Source{ID: "ghost", Source: "aws_ec2"})
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Call(); !errors.Is(err, test.Want) {
				t.Errorf("updating a missing %s = %v, want %v", test.Name, err, test.Want)
			}
		})
	}

	// Nothing was inserted by any of the refusals, which is the guarantee that matters.
	if got, err := db.Users().List(ctx); err != nil || len(got) != 0 {
		t.Errorf("Users().List() = (%d, %v), want the refused update to have written nothing",
			len(got), err)
	}
	if got, err := db.Templates().List(ctx); err != nil || len(got) != 0 {
		t.Errorf("Templates().List() = (%d, %v), want nothing written", len(got), err)
	}
}

// TestTemplateUpdateSilentlyDiscardsTheHostLimit demonstrates an edit that answers success and
// changes nothing.
//
// A template's limit is the pattern every launch from it is narrowed to, so it is the blast radius
// of the saved spec. Save writes the limit_pattern column and Update's SET list omits it, so an
// operator narrowing a template from the whole fleet to one host, or repointing it from one group
// to another, gets a 200 and a template that still launches against what it launched against
// before. Nothing in the response says the field was dropped: the handler re-reads the row and
// returns the stored, unchanged value.
func TestTemplateUpdateSilentlyDiscardsTheHostLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openStore(t).Templates()

	if err := store.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml", Limit: "all",
		CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Update(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml", Limit: "web01",
		CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Limit != "web01" {
		t.Errorf("the stored limit is %q after an edit that set it to %q: every launch from this "+
			"template still targets the hosts the operator meant to stop targeting",
			got.Limit, "web01")
	}
}

// TestTemplateUpdateWritesEveryOtherColumnSaveDoes is the control beside the limit finding: the
// rest of the editable columns do land, so the gap is one column rather than a broken Update.
func TestTemplateUpdateWritesEveryOtherColumnSaveDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Templates()

	if err := store.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "old", Playbook: "old.yml", CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	want := &template.Template{
		ID: "tpl_1", Name: "new", ProjectID: "proj_1", Playbook: "new.yml",
		Inventory: "hosts.ini", InventoryID: "inv_1", Shards: 3, Queue: "batch", Timeout: 900,
		Tool: "terraform", Command: "infra/prod", DryRun: true, Image: "ghcr.io/acme/ee:9",
		PullCredentialID: "cred_pull", OrgID: "org_1",
		CredentialIDs: []string{"cred_1"}, SelectableCredentialIDs: []string{"cred_2"},
		ExtraVars: map[string]any{"env": "prod"},
		Tags:      []string{"deploy"}, SkipTags: []string{"slow"},
		Verbosity: 2, Forks: 10, DiffMode: true, ConfirmOnLaunch: true, CreatedAt: baseTime,
	}
	if err := store.Update(ctx, want); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() after Update (-want +got):\n%s", diff)
	}
}

// TestLastAdminCannotBeDeletedOrDemoted pins the guard that keeps an install administrable. Both
// deletion and demotion reach zero administrators, and both are guarded inside the statement rather
// than by a count taken beforehand, because a count leaves a window two concurrent requests both
// pass. Losing the last admin has no recovery through the product at all: it needs a shell on the
// host.
func TestLastAdminCannotBeDeletedOrDemoted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Users()

	admin := &user.User{ID: "u_admin", Username: "root", PasswordHash: "h",
		Role: user.RoleAdmin, CreatedAt: baseTime}
	member := &user.User{ID: "u_member", Username: "sam", PasswordHash: "h",
		Role: user.RoleOperator, CreatedAt: baseTime}
	for _, u := range []*user.User{admin, member} {
		if err := store.Save(ctx, u); err != nil {
			t.Fatalf("Save(%s) error = %v", u.ID, err)
		}
	}

	// Deleting the only administrator is refused, and refused as "not allowed" rather than as
	// "not found", which is a different answer to the caller.
	ok, err := store.DeleteUnlessLastAdmin(ctx, "u_admin")
	if err != nil {
		t.Fatalf("DeleteUnlessLastAdmin(last admin) error = %v", err)
	}
	if ok {
		t.Fatal("the only administrator was deleted, leaving an install nobody can administer")
	}
	if _, err := store.Get(ctx, "u_admin"); err != nil {
		t.Fatalf("the refused delete removed the row anyway: %v", err)
	}

	// Demoting the only administrator is the other route to zero and is refused the same way.
	demoted := *admin
	demoted.Role = user.RoleOperator
	ok, err = store.UpdateUnlessLastAdmin(ctx, &demoted)
	if err != nil {
		t.Fatalf("UpdateUnlessLastAdmin(demote last admin) error = %v", err)
	}
	if ok {
		t.Fatal("the only administrator demoted themselves, leaving nobody who can administer")
	}
	still, err := store.Get(ctx, "u_admin")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if still.Role != user.RoleAdmin {
		t.Errorf("the refused demotion landed anyway: role is %q", still.Role)
	}

	// Editing the last administrator without touching the role is allowed, or nobody could ever
	// change their own password.
	renamed := *admin
	renamed.Username = "root2"
	renamed.PasswordHash = "h2"
	ok, err = store.UpdateUnlessLastAdmin(ctx, &renamed)
	if err != nil || !ok {
		t.Fatalf("UpdateUnlessLastAdmin(same role) = (%v, %v), want it allowed", ok, err)
	}

	// A non-administrator is deletable and demotable, since neither reaches zero.
	ok, err = store.UpdateUnlessLastAdmin(ctx, &user.User{ID: "u_member", Username: "sam",
		PasswordHash: "h", Role: user.RoleViewer, CreatedAt: baseTime})
	if err != nil || !ok {
		t.Fatalf("UpdateUnlessLastAdmin(non-admin) = (%v, %v), want it allowed", ok, err)
	}
	ok, err = store.DeleteUnlessLastAdmin(ctx, "u_member")
	if err != nil || !ok {
		t.Fatalf("DeleteUnlessLastAdmin(non-admin) = (%v, %v), want it allowed", ok, err)
	}
}

// TestLastAdminGuardDistinguishesRefusalFromAbsence pins the two different falses these guards can
// return. A caller has to tell "you may not do that" from "there is nothing there", because the
// first is a 409 and the second a 404, and answering the wrong one sends an operator hunting for a
// row that exists.
func TestLastAdminGuardDistinguishesRefusalFromAbsence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Users()

	if err := store.Save(ctx, &user.User{ID: "u_admin", Username: "root", PasswordHash: "h",
		Role: user.RoleAdmin, CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	tests := []struct {
		Name    string
		Call    func() (bool, error)
		WantOK  bool
		Want    error
		WantNil bool
	}{{ // Test 0: Deleting a user that does not exist reports absence.
		Name: "delete missing", Want: user.ErrNotFound,
		Call: func() (bool, error) { return store.DeleteUnlessLastAdmin(ctx, "u_ghost") },
	}, { // Test 1: Deleting the last admin is a refusal, not an absence.
		Name: "delete last admin", WantNil: true,
		Call: func() (bool, error) { return store.DeleteUnlessLastAdmin(ctx, "u_admin") },
	}, { // Test 2: Updating a user that does not exist reports absence.
		Name: "update missing", Want: user.ErrNotFound,
		Call: func() (bool, error) {
			return store.UpdateUnlessLastAdmin(ctx, &user.User{ID: "u_ghost", Username: "x",
				Role: user.RoleViewer})
		},
	}, { // Test 3: Demoting the last admin is a refusal, not an absence.
		Name: "demote last admin", WantNil: true,
		Call: func() (bool, error) {
			return store.UpdateUnlessLastAdmin(ctx, &user.User{ID: "u_admin", Username: "root",
				PasswordHash: "h", Role: user.RoleViewer, CreatedAt: baseTime})
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ok, err := test.Call()
			if ok != test.WantOK {
				t.Errorf("%s applied = %v, want %v", test.Name, ok, test.WantOK)
			}
			if test.WantNil {
				if err != nil {
					t.Errorf("%s error = %v, want nil so the caller reads a refusal not a 404",
						test.Name, err)
				}
				return
			}
			if !errors.Is(err, test.Want) {
				t.Errorf("%s error = %v, want %v", test.Name, err, test.Want)
			}
		})
	}
}

// TestConcurrentLastAdminRemovalsCannotBothWin pins the reason the guard lives in the statement.
// Two requests to remove the last two administrators, taken at the same moment, both saw a survivor
// when the count was read beforehand and both went through. The refusal has to be decided by the
// database on the row it is about to change.
func TestConcurrentLastAdminRemovalsCannotBothWin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Users()

	for _, id := range []string{"u_a", "u_b"} {
		if err := store.Save(ctx, &user.User{ID: id, Username: id, PasswordHash: "h",
			Role: user.RoleAdmin, CreatedAt: baseTime}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		removed int
	)
	for _, id := range []string{"u_a", "u_b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ok, err := store.DeleteUnlessLastAdmin(ctx, id)
			if err != nil {
				t.Errorf("DeleteUnlessLastAdmin(%s) error = %v", id, err)
				return
			}
			if ok {
				mu.Lock()
				removed++
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()

	if removed != 1 {
		t.Errorf("%d of the two administrators were removed, want exactly one", removed)
	}
	left, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	admins := 0
	for _, u := range left {
		if u.Role == user.RoleAdmin {
			admins++
		}
	}
	if admins != 1 {
		t.Errorf("%d administrators remain, want one: the install has to stay administrable",
			admins)
	}
}

// TestUserProfileLinksSurviveEverySeparator pins why profile links are stored as JSON rather than
// joined into one column. A URL may legally contain any character a join would pick as a
// separator, so a joined column splits somebody's link in half on read.
func TestUserProfileLinksSurviveEverySeparator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Users()

	links := []string{
		"https://example.com/a,b",
		"https://example.com/path?q=1&r=2",
		"https://example.com/#anchor|pipe",
		"https://пример.рф/путь",
		"",
	}
	if err := store.Save(ctx, &user.User{
		ID: "u_1", Username: "sam", PasswordHash: "h", Role: user.RoleOperator,
		CreatedAt: baseTime, Links: links, Notes: "line one\nline two",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "u_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(links, got.Links); diff != "" {
		t.Errorf("profile links (-want +got):\n%s", diff)
	}

	// No links at all comes back as none rather than as one empty link.
	if err := store.Save(ctx, &user.User{ID: "u_2", Username: "pat", PasswordHash: "h",
		Role: user.RoleViewer, CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err = store.Get(ctx, "u_2")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.Links) != 0 {
		t.Errorf("a user with no links came back with %v", got.Links)
	}
}

// TestUsernameUniquenessIsEnforced pins the constraint that keeps a login unambiguous. Two accounts
// with one username means the lookup that authenticates a person returns whichever row the database
// happens to reach first, which is an authentication decision made by accident.
func TestUsernameUniquenessIsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Users()

	if err := store.Save(ctx, &user.User{ID: "u_1", Username: "sam", PasswordHash: "h1",
		Role: user.RoleAdmin, CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &user.User{ID: "u_2", Username: "sam", PasswordHash: "h2",
		Role: user.RoleViewer, CreatedAt: baseTime}); err == nil {
		t.Error("a second account took the same username, so a login resolves to whichever row " +
			"the database reaches first")
	}
	// Renaming an account onto a taken username is refused for the same reason.
	if err := store.Update(ctx, &user.User{ID: "u_1", Username: "sam", PasswordHash: "h1",
		Role: user.RoleAdmin}); err != nil {
		t.Errorf("renaming an account to its own username = %v, want it allowed", err)
	}
	found, err := store.FindByUsername(ctx, "sam")
	if err != nil {
		t.Fatalf("FindByUsername() error = %v", err)
	}
	if found.ID != "u_1" || found.PasswordHash != "h1" {
		t.Errorf("FindByUsername() resolved to %+v, want the only account holding the name", found)
	}
	if _, err := store.FindByUsername(ctx, "SAM"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("FindByUsername(different case) = %v, want ErrNotFound: the lookup is exact", err)
	}
}

// TestTokenHashUniquenessIsEnforced pins the same reasoning for API tokens. The hash is what a
// presented token is looked up by, so two rows holding one hash would make the authenticated
// identity depend on row order.
func TestTokenHashUniquenessIsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Tokens()

	if err := store.Save(ctx, &auth.Token{ID: "tok_1", Name: "ci", Hash: "same",
		CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &auth.Token{ID: "tok_2", Name: "other", Hash: "same",
		CreatedAt: baseTime}); err == nil {
		t.Error("a second token took the same hash, so the identity a presented token resolves " +
			"to depends on row order")
	}
	if n, err := store.Count(ctx); err != nil || n != 1 {
		t.Errorf("Count() = (%d, %v), want the refused token not to have been written", n, err)
	}
}

// TestTouchNeverResurrectsARevokedToken pins that recording a token's last use cannot insert. A
// request authenticated a moment before the token was revoked still has a touch in flight, and an
// upsert there would put the revoked credential back in the table and make it valid again.
func TestTouchNeverResurrectsARevokedToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Tokens()

	if err := store.Save(ctx, &auth.Token{ID: "tok_1", Name: "ci", Hash: "h",
		CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Delete(ctx, "tok_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Touch(ctx, "tok_1", baseTime.Add(time.Hour)); err != nil {
		t.Errorf("Touch(revoked token) = %v, want a quiet no-op: the note is about a request "+
			"that already happened", err)
	}
	if _, err := store.FindByHash(ctx, "h"); !errors.Is(err, auth.ErrNotFound) {
		t.Error("touching a revoked token brought it back, so a credential somebody revoked " +
			"authenticates again")
	}
	if n, err := store.Count(ctx); err != nil || n != 0 {
		t.Errorf("Count() = (%d, %v), want zero", n, err)
	}
	// Touching an id that never existed is also quiet.
	if err := store.Touch(ctx, "tok_never", baseTime); err != nil {
		t.Errorf("Touch(unknown token) = %v, want a quiet no-op", err)
	}
}

// TestTriggerTokenHashUniquenessIsEnforced pins the third place a presented secret is resolved by
// hash. A webhook trigger's token decides which template fires, so two triggers holding one hash
// means an inbound webhook launches whichever the database reaches first.
func TestTriggerTokenHashUniquenessIsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Triggers()

	if err := store.Save(ctx, &trigger.Trigger{ID: "trg_1", Name: "deploy",
		TemplateID: "tpl_1", TokenHash: "same", CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &trigger.Trigger{ID: "trg_2", Name: "other",
		TemplateID: "tpl_2", TokenHash: "same", CreatedAt: baseTime}); err == nil {
		t.Error("a second trigger took the same token hash, so one webhook could launch either " +
			"template depending on row order")
	}
	got, err := store.FindByTokenHash(ctx, "same")
	if err != nil {
		t.Fatalf("FindByTokenHash() error = %v", err)
	}
	if got.TemplateID != "tpl_1" {
		t.Errorf("FindByTokenHash() resolved to template %q, want the only holder", got.TemplateID)
	}
	if _, err := store.FindByTokenHash(ctx, ""); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("FindByTokenHash(empty) = %v, want ErrNotFound rather than the first row", err)
	}
}

// TestRecordFireWritesOnlyWhatAFireOwns pins the narrow schedule write. A fire lands while an
// operator may be editing the same schedule, so writing the row whole would revert their edit, and
// an empty run id has to keep the stored one rather than blanking the link to the last run.
func TestRecordFireWritesOnlyWhatAFireOwns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Schedules()

	next := baseTime.Add(time.Hour)
	sc := &schedule.Schedule{
		ID: "sc_1", Name: "nightly", Cron: "0 0 * * *", Playbook: "site.yml", Enabled: true,
		CreatedAt: baseTime, NextRunAt: &next, Timezone: "UTC", OrgID: "org_1",
		CreatedBy: "sam",
	}
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	firedAt := baseTime.Add(30 * time.Minute)
	if err := store.RecordFire(ctx, "sc_1", firedAt, "run_1"); err != nil {
		t.Fatalf("RecordFire() error = %v", err)
	}
	got, err := store.Get(ctx, "sc_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastRunID != "run_1" || got.LastRunAt == nil || !got.LastRunAt.Equal(firedAt) {
		t.Errorf("after a fire the schedule reads run %q at %v, want run_1 at %v", got.LastRunID,
			got.LastRunAt, firedAt)
	}
	// Nothing a fire does not own moved.
	if got.Name != "nightly" || got.Cron != "0 0 * * *" || got.Timezone != "UTC" ||
		got.OrgID != "org_1" || got.CreatedBy != "sam" || !got.Enabled ||
		got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Errorf("a fire rewrote columns it does not own: %+v", got)
	}

	// A fire with no run id keeps the last one, because the fire happened even if the launch
	// produced no run to point at.
	later := firedAt.Add(time.Hour)
	if err := store.RecordFire(ctx, "sc_1", later, ""); err != nil {
		t.Fatalf("RecordFire(no run id) error = %v", err)
	}
	got, err = store.Get(ctx, "sc_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastRunID != "run_1" {
		t.Errorf("LastRunID = %q, want the stored link kept when a fire carries none", got.LastRunID)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(later) {
		t.Errorf("LastRunAt = %v, want the newer fire time %v", got.LastRunAt, later)
	}

	// Recording a fire against a schedule somebody deleted is not an error and does not recreate it.
	if err := store.Delete(ctx, "sc_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.RecordFire(ctx, "sc_1", later, "run_2"); err != nil {
		t.Errorf("RecordFire(deleted schedule) = %v, want a quiet no-op", err)
	}
	if _, err := store.Get(ctx, "sc_1"); !errors.Is(err, schedule.ErrNotFound) {
		t.Error("recording a fire brought a deleted schedule back")
	}
}

// TestClaimDueIsAnExactTextCompareAndSwap pins the scheduler's mutual exclusion. The claim compares
// the stored next_run_at for exact equality, so only one caller can advance a schedule from a given
// time. The scheduler reads a lost claim as another node having won, so a claim that could be won
// twice fires one schedule twice, and a claim that can never be won stops it firing at all with
// nothing logged either way.
func TestClaimDueIsAnExactTextCompareAndSwap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Schedules()

	next := baseTime
	if err := store.Save(ctx, &schedule.Schedule{ID: "sc_1", Cron: "0 0 * * *",
		Playbook: "site.yml", CreatedAt: baseTime, NextRunAt: &next}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	advanced := baseTime.Add(24 * time.Hour)
	won, err := store.ClaimDue(ctx, "sc_1", next, advanced)
	if err != nil || !won {
		t.Fatalf("ClaimDue(matching time) = (%v, %v), want the claim won", won, err)
	}
	// A second caller holding the old time loses, which is what stops a double fire.
	won, err = store.ClaimDue(ctx, "sc_1", next, advanced)
	if err != nil {
		t.Fatalf("ClaimDue(second) error = %v", err)
	}
	if won {
		t.Error("two callers both won the same claim, so the schedule fires twice")
	}
	// A schedule that is gone is a lost race, not an error, since another node may have deleted it.
	won, err = store.ClaimDue(ctx, "sc_ghost", next, advanced)
	if err != nil || won {
		t.Errorf("ClaimDue(missing schedule) = (%v, %v), want a quietly lost race", won, err)
	}
}

// TestCredentialTypeCreationTimeIsUnstorable demonstrates the one store that keeps a timestamp as
// an integer instead of the shared text form.
//
// The column holds UnixNano, which is undefined for a time far from the epoch, so a type saved
// without a creation time comes back stamped in the eighteenth century rather than as the zero
// value. List orders by that column, so such a type sorts ahead of everything real, and the upsert
// deliberately does not write created_at on a conflict, which means saving the type again cannot
// correct it.
func TestCredentialTypeCreationTimeIsUnstorable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openStore(t).CredentialTypes()

	if err := store.Save(ctx, &credential.CredentialType{ID: "ct_1", Name: "vault"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "ct_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("a type saved with no creation time reads %v, want the zero value: the stored "+
			"integer is undefined for a time this far from the epoch", got.CreatedAt)
	}

	// Saving it again with a real time must be able to correct the stamp.
	want := baseTime
	if err := store.Save(ctx, &credential.CredentialType{ID: "ct_1", Name: "vault",
		CreatedAt: want}); err != nil {
		t.Fatalf("Save(with a time) error = %v", err)
	}
	got, err = store.Get(ctx, "ct_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v after a re-save that set it to %v: the upsert omits the column",
			got.CreatedAt, want)
	}
}

// TestCredentialTypeInjectorsRoundTripAsAbsentOrPresent pins the difference between a type with no
// injectors and one with an empty set. They are stored as JSON objects rather than as null so a
// decode never has to guess, and an empty map is normalized back to nil so the two cannot drift
// apart between a fresh write and a read.
func TestCredentialTypeInjectorsRoundTripAsAbsentOrPresent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).CredentialTypes()

	tests := []struct {
		Name string
		Type *credential.CredentialType
	}{{ // Test 0: A type with nothing at all beyond its name.
		Name: "bare", Type: &credential.CredentialType{ID: "ct_bare", Name: "bare",
			CreatedAt: baseTime},
	}, { // Test 1: A type whose injector maps are present but empty.
		Name: "empty maps", Type: &credential.CredentialType{ID: "ct_empty", Name: "empty",
			CreatedAt: baseTime, EnvInjectors: map[string]string{},
			ExtraVarInjectors: map[string]string{}},
	}, { // Test 2: A type with both injector kinds and unicode in the values.
		Name: "populated", Type: &credential.CredentialType{ID: "ct_full", Name: "vault",
			CreatedAt:         baseTime,
			Fields:            []credential.Field{{Name: "token", Label: "Tökén", Secret: true}},
			EnvInjectors:      map[string]string{"VAULT_TOKEN": "{{ token }}"},
			ExtraVarInjectors: map[string]string{"vault_token": "{{ token }}"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := store.Save(ctx, test.Type); err != nil {
				t.Fatalf("Save(%s) error = %v", test.Name, err)
			}
			got, err := store.Get(ctx, test.Type.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.Type, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s round trip (-want +got):\n%s", test.Name, diff)
			}
			// An empty map and no map read back the same way, so nothing downstream has to guess.
			if len(got.EnvInjectors) == 0 && got.EnvInjectors != nil {
				t.Errorf("%s: an empty env injector map came back non-nil", test.Name)
			}
		})
	}
}

// TestCredentialSecretsAreStoredExactlyAsGiven pins that the sealed secret is opaque bytes to this
// layer. A store that trimmed, normalized, or re-encoded it would produce a credential that
// decrypts to something else, which surfaces as an authentication failure against somebody else's
// infrastructure rather than as a storage bug.
func TestCredentialSecretsAreStoredExactlyAsGiven(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Credentials()

	secrets := []string{
		"  leading and trailing spaces  ",
		"line\nbreak",
		"tab\there",
		strings.Repeat("A", 100000),
		"ünïcode 🔐",
		`{"looks":"like json"}`,
	}
	for testNum, secret := range secrets {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id := fmt.Sprintf("cred_%d", testNum)
			if err := store.Save(ctx, &credential.Credential{
				ID: id, Name: "n", Kind: credential.KindSSHKey, Secret: secret,
				CreatedAt: baseTime,
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			got, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Secret != secret {
				t.Errorf("the stored secret is %d bytes and differs from the %d given: a sealed "+
					"value must be opaque to this layer", len(got.Secret), len(secret))
			}
		})
	}
}

// TestOrgMembershipRolesUpsertRatherThanDuplicate pins that adding an existing member changes their
// role instead of creating a second row. Two membership rows for one person in one organization
// would make their effective role depend on which row a reader saw first, which is an access
// decision made by row order.
func TestOrgMembershipRolesUpsertRatherThanDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openStore(t)
	orgs := db.Orgs()

	if err := orgs.Save(ctx, &org.Org{ID: "org_1", Name: "acme", CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := orgs.AddMember(ctx, "org_1", "u_1", org.RoleMember); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	if err := orgs.AddMember(ctx, "org_1", "u_1", org.RoleAdmin); err != nil {
		t.Fatalf("AddMember(again) error = %v", err)
	}
	members, err := orgs.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	want := []org.Member{{UserID: "u_1", Role: org.RoleAdmin}}
	if diff := cmp.Diff(want, members); diff != "" {
		t.Errorf("membership after a repeated add (-want +got):\n%s", diff)
	}

	// Removing somebody who is not a member is a quiet no-op rather than an error.
	if err := orgs.RemoveMember(ctx, "org_1", "u_stranger"); err != nil {
		t.Errorf("RemoveMember(non-member) = %v, want a no-op", err)
	}
	// Deleting the organization takes its memberships with it.
	if err := orgs.Delete(ctx, "org_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got, err := orgs.OrgsForUser(ctx, "u_1"); err != nil || len(got) != 0 {
		t.Errorf("OrgsForUser after the org was deleted = (%v, %v), want no memberships", got, err)
	}
}

// TestTeamMembershipAddIsIdempotent pins that adding somebody twice does not fail and does not
// duplicate. Membership is read as a set, so a duplicate row is invisible until something counts.
func TestTeamMembershipAddIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	teams := openStore(t).Teams()

	if err := teams.Save(ctx, &team.Team{ID: "team_1", Name: "ops",
		CreatedAt: baseTime}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := teams.AddMember(ctx, "team_1", "u_1"); err != nil {
			t.Fatalf("AddMember(%d) error = %v", i, err)
		}
	}
	members, err := teams.Members(ctx, "team_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	if diff := cmp.Diff([]string{"u_1"}, members); diff != "" {
		t.Errorf("members after three identical adds (-want +got):\n%s", diff)
	}
	if err := teams.RemoveMember(ctx, "team_1", "u_stranger"); err != nil {
		t.Errorf("RemoveMember(non-member) = %v, want a no-op", err)
	}
	if got, err := teams.Members(ctx, "team_ghost"); err != nil || len(got) != 0 {
		t.Errorf("Members(unknown team) = (%v, %v), want none and no error", got, err)
	}
}

// TestPolicyBooleansAndThresholdRoundTrip pins the fields an approval rule is evaluated from. A
// rule that comes back with its separation-of-duties requirement off, or with its destroy threshold
// reset, is a control that reads as configured and does not hold, and the run it should have held
// executes.
func TestPolicyBooleansAndThresholdRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Policies()

	tests := []struct {
		Name   string
		Policy *policy.Policy
	}{{ // Test 0: Every gate on, with a threshold of zero, which is not the same as unset.
		Name: "all on", Policy: &policy.Policy{
			ID: "pol_on", Name: "prod apply", Tool: "terraform", CommandContains: "infra/prod",
			InventoryID: "inv_1", Queue: "prod", ExcludeDryRun: true, MaxDestroy: 0,
			ActorKind: "agent", Actor: "deploy-bot", MinRisk: "high", Effect: "hold",
			RequireDistinctApprover: true, CreatedAt: baseTime,
		},
	}, { // Test 1: Every gate off, with the threshold at the value that disables the check.
		Name: "all off", Policy: &policy.Policy{
			ID: "pol_off", Name: "loose", MaxDestroy: -1, CreatedAt: baseTime,
		},
	}, { // Test 2: A large threshold, so the column is not narrower than the value.
		Name: "large threshold", Policy: &policy.Policy{
			ID: "pol_big", Name: "big", MaxDestroy: 2147483647, CreatedAt: baseTime,
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := store.Save(ctx, test.Policy); err != nil {
				t.Fatalf("Save(%s) error = %v", test.Name, err)
			}
			got, err := store.Get(ctx, test.Policy.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.Policy, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s round trip (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestGrantsForObjectAreScopedToThatObject pins the read every authorization decision goes through.
// A query that leaked another object's grants would hand somebody access they were never given, and
// it would do so invisibly, because the grant it matched is a real grant to a real subject.
func TestGrantsForObjectAreScopedToThatObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).Grants()

	grants := []*grant.Grant{
		{ID: "g_1", Subject: "user:sam", Object: "project:proj_1", Access: grant.AccessUse,
			CreatedAt: baseTime},
		{ID: "g_2", Subject: "team:ops", Object: "project:proj_1", Access: grant.AccessManage,
			CreatedAt: baseTime.Add(time.Second)},
		{ID: "g_3", Subject: "user:pat", Object: "project:proj_2", Access: grant.AccessUse,
			CreatedAt: baseTime},
		{ID: "g_4", Subject: "user:pat", Object: "credential:cred_1", Access: grant.AccessUse,
			CreatedAt: baseTime},
	}
	for _, g := range grants {
		if err := store.Save(ctx, g); err != nil {
			t.Fatalf("Save(%s) error = %v", g.ID, err)
		}
	}

	tests := []struct {
		Name    string
		Object  string
		WantIDs []string
	}{{ // Test 0: An object with two grants returns both, oldest first.
		Name: "shared object", Object: "project:proj_1", WantIDs: []string{"g_1", "g_2"},
	}, { // Test 1: A different object returns only its own.
		Name: "other object", Object: "project:proj_2", WantIDs: []string{"g_3"},
	}, { // Test 2: A different object kind with a colliding id is a different object.
		Name: "other kind", Object: "credential:cred_1", WantIDs: []string{"g_4"},
	}, { // Test 3: An object nobody granted returns nothing rather than everything.
		Name: "ungranted", Object: "project:proj_999",
	}, { // Test 4: The empty object matches nothing, not every grant.
		Name: "empty", Object: "",
	}, { // Test 5: A LIKE wildcard is literal text here, not a pattern over every object.
		Name: "wildcard", Object: "%",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := store.ForObject(ctx, test.Object)
			if err != nil {
				t.Fatalf("ForObject(%s) error = %v", test.Name, err)
			}
			ids := make([]string, len(got))
			for i, g := range got {
				ids[i] = g.ID
			}
			if diff := cmp.Diff(test.WantIDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ForObject(%s) (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestInventorySourceUpdateLeavesSyncStateAlone pins the columns an edit must not touch. A source's
// backing inventory and its sync record belong to the sync, not to the person editing the source,
// and rewriting them from an edit form would either repoint the source at a different inventory or
// erase the record of the last sync and its error.
func TestInventorySourceUpdateLeavesSyncStateAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t).InventorySources()

	synced := baseTime.Add(time.Hour)
	if err := store.Save(ctx, &invsource.Source{
		ID: "src_1", Name: "ec2", Source: "aws_ec2", CredentialID: "cred_1",
		ProjectID: "proj_1", InventoryID: "inv_1", SyncedAt: &synced,
		LastError: "throttled", UpdateOnLaunch: true, SyncIntervalSeconds: 300,
		CreatedAt: baseTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Update(ctx, &invsource.Source{
		ID: "src_1", Name: "ec2 renamed", Source: "gcp_compute", CredentialID: "cred_2",
		ProjectID: "proj_2", InventoryID: "inv_HIJACK", LastError: "cleared",
		UpdateOnLaunch: false, SyncIntervalSeconds: 900,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "src_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "ec2 renamed" || got.Source != "gcp_compute" || got.CredentialID != "cred_2" ||
		got.ProjectID != "proj_2" || got.UpdateOnLaunch || got.SyncIntervalSeconds != 900 {
		t.Errorf("the editable fields did not all land: %+v", got)
	}
	if got.InventoryID != "inv_1" {
		t.Errorf("InventoryID = %q, want inv_1: an edit must not repoint a source at another "+
			"inventory", got.InventoryID)
	}
	if got.LastError != "throttled" {
		t.Errorf("LastError = %q, want the sync's own record kept", got.LastError)
	}
	if got.SyncedAt == nil || !got.SyncedAt.Equal(synced) {
		t.Errorf("SyncedAt = %v, want the sync's own record kept", got.SyncedAt)
	}
	if !got.CreatedAt.Equal(baseTime) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, baseTime)
	}
}

// TestListsAreOrderedOldestFirstAndTieBreakOnID pins the ordering every listing endpoint pages
// through. Objects created in the same instant, which an import or a seeded demo produces in bulk,
// would otherwise come back in an unstable order and a paged client would see rows twice or not at
// all.
func TestListsAreOrderedOldestFirstAndTieBreakOnID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openStore(t)

	// Three objects sharing one creation instant, saved out of id order.
	for _, id := range []string{"org_c", "org_a", "org_b"} {
		if err := db.Orgs().Save(ctx, &org.Org{ID: id, Name: id, CreatedAt: baseTime}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	// One older object, which must sort ahead of all of them.
	if err := db.Orgs().Save(ctx, &org.Org{ID: "org_z", Name: "oldest",
		CreatedAt: baseTime.Add(-time.Hour)}); err != nil {
		t.Fatalf("Save(org_z) error = %v", err)
	}

	got, err := db.Orgs().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	ids := make([]string, len(got))
	for i, o := range got {
		ids[i] = o.ID
	}
	want := []string{"org_z", "org_a", "org_b", "org_c"}
	if diff := cmp.Diff(want, ids); diff != "" {
		t.Errorf("List() order (-want +got):\n%s", diff)
	}
}
