package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// testUnicode exercises characters a snapshot must carry untouched: a non-Latin script, a combining
// accent, and a right-to-left run. A name mangled by a restore is a name nobody searching the
// restored install will find again.
const testUnicode = "運用チーム équipe צוות"

// testLong is a value long enough to prove nothing in the pipeline truncates content.
var testLong = strings.Repeat("host-", 20000)

// testFillTime is a fixed instant so restored objects compare exactly.
var testFillTime = time.Date(2026, 3, 4, 5, 6, 7, 89000000, time.UTC)

// fillEverything saves at least two objects of every backed-up kind, each with its optional fields
// set, so a round trip compares whole objects rather than the handful of fields a minimal fixture
// happens to touch.
//
//nolint:funlen // Test helper.
func fillEverything(t *testing.T, ctx context.Context, s Stores) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("save fixture: %v", err)
		}
	}
	later := testFillTime.Add(time.Hour)

	must(s.Orgs.Save(ctx, &org.Org{ID: "org_1", Name: testUnicode, CreatedAt: testFillTime}))
	must(s.Orgs.Save(ctx, &org.Org{ID: "org_2", Name: "second", CreatedAt: later}))
	must(s.Teams.Save(ctx, &team.Team{ID: "team_1", Name: "sre", CreatedAt: testFillTime}))
	must(s.Teams.Save(ctx, &team.Team{ID: "team_2", Name: testUnicode, CreatedAt: later}))

	must(s.Users.Save(ctx, &user.User{
		ID: "user_1", Username: "ada", PasswordHash: "argon2id$HASH", Role: user.RoleAdmin,
		FullName: testUnicode, Email: "ada@example.com", Phone: "+1 555 0100", Title: "Engineer",
		Links: []string{"https://example.com/ada", "http://example.org/a"},
		Notes: "on call", Source: "local", CreatedAt: testFillTime,
	}))
	must(s.Users.Save(ctx, &user.User{
		ID: "user_2", Username: "grace", PasswordHash: "argon2id$OTHER", Role: user.RoleViewer,
		CreatedAt: later,
	}))
	must(s.Teams.AddMember(ctx, "team_1", "user_1"))
	must(s.Teams.AddMember(ctx, "team_1", "user_2"))
	must(s.Teams.AddMember(ctx, "team_2", "user_2"))
	must(s.Orgs.AddMember(ctx, "org_1", "user_1", org.RoleAdmin))
	must(s.Orgs.AddMember(ctx, "org_1", "user_2", org.RoleMember))

	used := testFillTime.Add(2 * time.Hour)
	expires := testFillTime.Add(720 * time.Hour)
	must(s.Tokens.Save(ctx, &auth.Token{
		ID: "tok_1", Name: "ci", UserID: "user_1", Kind: auth.KindAgent, Hash: "SHA-ONE",
		CreatedAt: testFillTime, LastUsedAt: &used, ExpiresAt: &expires,
	}))
	must(s.Tokens.Save(ctx, &auth.Token{
		ID: "tok_2", Name: "cli", Hash: "SHA-TWO", CreatedAt: later,
	}))

	must(s.CredentialTypes.Save(ctx, &credential.CredentialType{ID: "ct_1", Name: "datadog"}))
	must(s.Credentials.Save(ctx, &credential.Credential{
		ID: "cred_1", Name: testUnicode, Kind: credential.KindSSHKey, TypeID: "ct_1",
		Source: "vault", Secret: "SEALED-SECRET-ONE", VaultID: "kv/data/ssh",
		Settings: map[string]string{"path": "kv/data/ssh", "field": "private_key"},
		OrgID:    "org_1", CreatedAt: testFillTime,
	}))
	must(s.Credentials.Save(ctx, &credential.Credential{
		ID: "cred_2", Name: "plain", Kind: credential.KindSSHKey, Secret: testLong,
		CreatedAt: later,
	}))

	must(s.Grants.Save(ctx, &grant.Grant{
		ID: "grant_1", Subject: "team_1", Object: "cred_1", Access: grant.AccessUse,
		CreatedAt: testFillTime,
	}))
	must(s.Grants.Save(ctx, &grant.Grant{
		ID: "grant_2", Subject: "user_1", Object: "cred_2", Access: grant.AccessManage,
		CreatedAt: later,
	}))

	must(s.Policies.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "hold prod destroy", Tool: "terraform", CommandContains: "destroy",
		Queue: "prod", ActorKind: policy.ActorKindAgent, MinRisk: "high",
		Effect: policy.EffectDeny, ExcludeDryRun: true, RequireDistinctApprover: true,
		MaxDestroy: policy.DisabledMaxDestroy, CreatedAt: testFillTime,
	}))
	must(s.Policies.Save(ctx, &policy.Policy{
		ID: "pol_2", Name: "blanket", MaxDestroy: policy.DisabledMaxDestroy, CreatedAt: later,
	}))
	// A plan-content rule carries the other value MaxDestroy can hold. It is a separate policy
	// because a deny rule cannot also set a destroy ceiling: a denied run is never planned at all,
	// so a file holding one is a file no path could have written and a restore now refuses it.
	must(s.Policies.Save(ctx, &policy.Policy{
		ID: "pol_3", Name: "hold wide applies", Tool: "terraform",
		Effect: policy.EffectRequireApproval, MaxDestroy: 5, CreatedAt: later,
	}))

	must(s.Projects.Save(ctx, &project.Project{
		ID: "proj_1", Name: "app", RepoURL: "https://example.com/app.git", Branch: "main",
		CredentialID: "cred_1", InstallDeps: true, Image: "ghcr.io/example/runner:1",
		PullCredentialID: "cred_2", OrgID: "org_1", CreatedAt: testFillTime,
	}))
	must(s.Projects.Save(ctx, &project.Project{
		ID: "proj_2", Name: testUnicode, RepoURL: "https://example.com/b.git", CreatedAt: later,
	}))

	must(s.Inventories.Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "prod", Content: "[all]\n" + testUnicode,
		CredentialIDs: []string{"cred_1", "cred_2"}, ContentSource: "dynamic",
		ContentConfig: "SEALED-CONFIG", Queue: "prod", OrgID: "org_1", CreatedAt: testFillTime,
	}))
	must(s.Inventories.Save(ctx, &inventory.Inventory{
		ID: "inv_2", Name: "big", Content: testLong, CreatedAt: later,
	}))

	synced := testFillTime.Add(3 * time.Hour)
	must(s.InventorySources.Save(ctx, &invsource.Source{
		ID: "src_1", Name: "aws", Source: "aws_ec2", CredentialID: "cred_1", ProjectID: "proj_1",
		InventoryID: "inv_1", SyncedAt: &synced, LastError: "throttled", UpdateOnLaunch: true,
		SyncIntervalSeconds: 900, CreatedAt: testFillTime,
	}))
	must(s.InventorySources.Save(ctx, &invsource.Source{
		ID: "src_2", Name: "azure", Source: "azure_rm", InventoryID: "inv_2", CreatedAt: later,
	}))

	must(s.Templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", ProjectID: "proj_1", Playbook: "site.yml",
		Inventory: "prod", InventoryID: "inv_1", Tool: "ansible", DryRun: true, Limit: "web*",
		Tags: []string{"deploy", testUnicode}, SkipTags: []string{"slow"}, Verbosity: 2,
		Forks: 10, DiffMode: true, Shards: 3, Queue: "prod", Timeout: 600,
		CredentialIDs: []string{"cred_1"}, SelectableCredentialIDs: []string{"cred_2"},
		ExtraVars: map[string]any{"env": "prod", "verbose": true, "note": testUnicode},
		Steps: []run.PipelineStep{{
			Name: "one", Playbook: "a.yml", Retries: 2, DependsOn: []string{"zero"},
		}},
		CreatedAt: testFillTime,
	}))
	must(s.Templates.Save(ctx, &template.Template{
		ID: "tpl_2", Name: "plain", Playbook: "b.yml", CreatedAt: later,
	}))

	lastRun := testFillTime.Add(4 * time.Hour)
	must(s.Schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC", Playbook: "site.yml",
		Inventory: "prod", Shards: 2, TemplateID: "tpl_1", OrgID: "org_1", Enabled: true,
		CreatedAt: testFillTime, CreatedBy: "user_1", LastRunAt: &lastRun, LastRunID: "run_9",
	}))
	must(s.Schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_2", Name: "off", Cron: "0 3 * * *", TemplateID: "tpl_2", Enabled: false,
		CreatedAt: later,
	}))

	fired := testFillTime.Add(5 * time.Hour)
	must(s.Triggers.Save(ctx, &trigger.Trigger{
		ID: "trg_1", Name: "hook", TemplateID: "tpl_1", TokenHash: "TOKEN-HASH",
		SigningSecret: "SEALED-SIGN", RequireSignature: true, LastFiredAt: &fired,
		CreatedBy: "user_1", CreatedAt: testFillTime,
	}))
	must(s.Triggers.Save(ctx, &trigger.Trigger{
		ID: "trg_2", Name: "second", TemplateID: "tpl_2", TokenHash: "H2", CreatedAt: later,
	}))
}

// testRoundTrip writes src to a sealed file and restores it into fresh stores, returning both
// summaries and the restored stores.
func testRoundTrip(t *testing.T, ctx context.Context, src Stores) (Summary, Summary, Stores) {
	t.Helper()
	var buf bytes.Buffer
	wrote, err := Write(ctx, src, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	dst := freshStores()
	read, err := Read(ctx, dst, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return wrote, read, dst
}

// TestRoundTripIsFaithfulForEveryObject compares every restored object against the original field
// for field, including the fields hidden from JSON.
//
// The existing round trip proves the file carries something of each kind. This proves it carries
// all of each kind. A backup is a hand-written projection of the entity into a payload, so a field
// added to a credential, a template, or a trigger is carried only if somebody remembered. Nothing
// fails when one is missed: the file writes, the restore reads, the counts agree, and the value is
// simply gone on the day it is needed.
//
//nolint:funlen // Test function.
func TestRoundTripIsFaithfulForEveryObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	wrote, read, dst := testRoundTrip(t, ctx, src)

	if diff := cmp.Diff(wrote, read, cmpopts.IgnoreFields(Summary{}, "CreatedAt")); diff != "" {
		t.Errorf("the restore moved different counts than the backup held (-wrote +read):\n%s", diff)
	}
	if !read.CreatedAt.Equal(wrote.CreatedAt) {
		t.Errorf("restored snapshot time = %s, want %s", read.CreatedAt, wrote.CreatedAt)
	}

	tests := []struct {
		Name string
		List func(Stores) (any, error)
		Opts []cmp.Option
	}{{ // Test 0: Credentials, whose sealed secret is hidden from JSON.
		Name: "credentials",
		List: func(s Stores) (any, error) { return s.Credentials.List(ctx) },
	}, { // Test 1: Credential types, without which every typed credential injects nothing.
		Name: "credential types",
		List: func(s Stores) (any, error) { return s.CredentialTypes.List(ctx) },
	}, { // Test 2: Projects.
		Name: "projects",
		List: func(s Stores) (any, error) { return s.Projects.List(ctx) },
	}, { // Test 3: Templates, including their steps, survey, and extra vars.
		Name: "templates",
		List: func(s Stores) (any, error) { return s.Templates.List(ctx) },
	}, { // Test 4: Inventories, whose dynamic source config is hidden from JSON.
		Name: "inventories",
		List: func(s Stores) (any, error) { return s.Inventories.List(ctx) },
	}, { // Test 5: Inventory sources.
		Name: "inventory sources",
		List: func(s Stores) (any, error) { return s.InventorySources.List(ctx) },
	}, { // Test 6: Triggers, whose token hash and signing secret are hidden from JSON.
		Name: "triggers",
		List: func(s Stores) (any, error) { return s.Triggers.List(ctx) },
	}, { // Test 7: Accounts, whose password hash is hidden from JSON.
		Name: "users",
		List: func(s Stores) (any, error) { return s.Users.List(ctx) },
	}, { // Test 8: API tokens, whose hash is what actually authenticates them.
		Name: "tokens",
		List: func(s Stores) (any, error) { return s.Tokens.List(ctx) },
	}, { // Test 9: Teams.
		Name: "teams",
		List: func(s Stores) (any, error) { return s.Teams.List(ctx) },
	}, { // Test 10: Organizations.
		Name: "orgs",
		List: func(s Stores) (any, error) { return s.Orgs.List(ctx) },
	}, { // Test 11: Access grants.
		Name: "grants",
		List: func(s Stores) (any, error) { return s.Grants.List(ctx) },
	}, { // Test 12: Approval policies, the gates in front of every change.
		Name: "policies",
		List: func(s Stores) (any, error) { return s.Policies.List(ctx) },
	}, { // Test 13: Schedules, whose next fire time is deliberately recomputed on restore.
		Name: "schedules",
		List: func(s Stores) (any, error) { return s.Schedules.List(ctx) },
		Opts: []cmp.Option{cmpopts.IgnoreFields(schedule.Schedule{}, "NextRunAt")},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			want, err := test.List(src)
			if err != nil {
				t.Fatalf("%s: source List() error = %v", test.Name, err)
			}
			got, err := test.List(dst)
			if err != nil {
				t.Fatalf("%s: restored List() error = %v", test.Name, err)
			}
			opts := append([]cmp.Option{cmpopts.EquateEmpty()}, test.Opts...)
			if diff := cmp.Diff(want, got, opts...); diff != "" {
				t.Errorf("%s did not survive the round trip (-want +got):\n%s", test.Name, diff)
			}
		})
	}

	// Membership is stored beside the team and organization rows rather than on them, so it is
	// compared on its own.
	for _, id := range []string{"team_1", "team_2"} {
		want, err := src.Teams.Members(ctx, id)
		if err != nil {
			t.Fatalf("source Members(%s) error = %v", id, err)
		}
		got, err := dst.Teams.Members(ctx, id)
		if err != nil {
			t.Fatalf("restored Members(%s) error = %v", id, err)
		}
		if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("team %s membership mismatch (-want +got):\n%s", id, diff)
		}
	}
	want, err := src.Orgs.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("source org Members() error = %v", err)
	}
	got, err := dst.Orgs.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("restored org Members() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("organization membership mismatch (-want +got):\n%s", diff)
	}
}

// TestRoundTripOfAnEmptyControlPlane proves a fresh install backs up and restores without error and
// reports nothing, rather than writing a file that cannot be read back.
func TestRoundTripOfAnEmptyControlPlane(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	wrote, read, dst := testRoundTrip(t, ctx, freshStores())
	if diff := cmp.Diff(wrote, read, cmpopts.IgnoreFields(Summary{}, "CreatedAt")); diff != "" {
		t.Errorf("empty round trip summary mismatch (-wrote +read):\n%s", diff)
	}
	if wrote.Users != 0 || wrote.Credentials != 0 || wrote.Grants != 0 {
		t.Errorf("an empty install reported %+v, want zeros", wrote)
	}
	if got := testObjectCount(t, ctx, dst); got != 0 {
		t.Errorf("an empty backup restored %d objects", got)
	}
	if wrote.CreatedAt.IsZero() {
		t.Error("the snapshot time is zero, so the file cannot say when it was taken")
	}
}

// TestRestoreNeverDeletes proves a restore adds and overwrites but never removes, which is what the
// package documents. An operator restoring one lost object must not lose everything created since.
func TestRestoreNeverDeletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	if err := src.Projects.Save(ctx, &project.Project{
		ID: "proj_backed_up", Name: "old", RepoURL: "https://example.com/a.git",
		CreatedAt: testFillTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	dst := freshStores()
	if err := dst.Projects.Save(ctx, &project.Project{
		ID: "proj_newer", Name: "created since the backup",
		RepoURL: "https://example.com/b.git", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := Read(ctx, dst, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	got, err := dst.Projects.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after restore there are %d projects, want the restored one and the newer one", len(got))
	}
	if _, err := dst.Projects.Get(ctx, "proj_newer"); err != nil {
		t.Errorf("the project created after the backup was removed by the restore: %v", err)
	}
}

// TestRestoreIsIdempotent proves restoring the same file twice lands in the same place. A recovery
// is often retried after a partial failure, so the second attempt must not double anything or
// refuse on rows the first attempt already wrote, including the account username check that guards
// the front of the restore.
func TestRestoreIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)

	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	file := buf.Bytes()

	dst := freshStores()
	first, err := Read(ctx, dst, testSealerOnce(), bytes.NewReader(file))
	if err != nil {
		t.Fatalf("first Read() error = %v", err)
	}
	afterFirst := testObjectCount(t, ctx, dst)
	second, err := Read(ctx, dst, testSealerOnce(), bytes.NewReader(file))
	if err != nil {
		t.Fatalf("second Read() error = %v", err)
	}
	if diff := cmp.Diff(first, second); diff != "" {
		t.Errorf("the second restore reported different counts (-first +second):\n%s", diff)
	}
	if got := testObjectCount(t, ctx, dst); got != afterFirst {
		t.Errorf("the second restore left %d objects, want the same %d", got, afterFirst)
	}
	members, err := dst.Teams.Members(ctx, "team_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	if len(members) != 2 {
		t.Errorf("team_1 has %d members after two restores, want 2", len(members))
	}
}

// TestRestoreOverItsOwnInstall proves a backup restores into the install it came from. This is the
// ordinary rollback, and the account collision check has to recognize that an account restored over
// itself is the same account rather than a name clash.
func TestRestoreOverItsOwnInstall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := freshStores()
	fillEverything(t, ctx, s)
	var buf bytes.Buffer
	wrote, err := Write(ctx, s, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	read, err := Read(ctx, s, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("restoring an install over itself failed: %v", err)
	}
	if diff := cmp.Diff(wrote, read, cmpopts.IgnoreFields(Summary{}, "CreatedAt")); diff != "" {
		t.Errorf("self restore summary mismatch (-wrote +read):\n%s", diff)
	}
}

// TestRoundTripCarriesUnicodeAndVeryLongValues proves nothing in the compress, seal, and JSON
// pipeline truncates or mangles content. An inventory is often the largest object in an install and
// a name is often not written in Latin script.
func TestRoundTripCarriesUnicodeAndVeryLongValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	_, _, dst := testRoundTrip(t, ctx, src)

	inv, err := dst.Inventories.Get(ctx, "inv_2")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(testLong, inv.Content); diff != "" {
		t.Errorf("long inventory content mismatch (-want +got):\n%s", diff)
	}
	cred, err := dst.Credentials.Get(ctx, "cred_2")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(testLong, cred.Secret); diff != "" {
		t.Errorf("long sealed secret mismatch (-want +got):\n%s", diff)
	}
	o, err := dst.Orgs.Get(ctx, "org_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(testUnicode, o.Name); diff != "" {
		t.Errorf("unicode organization name mismatch (-want +got):\n%s", diff)
	}
}

// TestRoundTripKeepsNumericExtraVarsEquivalent proves a template's extra vars come back as the same
// JSON document. They are held as free-form values, so a numeric one crosses the payload as a JSON
// number rather than as the Go type it went in as. What has to survive is the document the launcher
// hands the tool, so that is what is compared.
func TestRoundTripKeepsNumericExtraVarsEquivalent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	vars := map[string]any{"forks": float64(12), "ratio": 0.5, "on": true, "name": testUnicode}
	if err := src.Templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "n", Playbook: "site.yml", ExtraVars: vars, CreatedAt: testFillTime,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	_, _, dst := testRoundTrip(t, ctx, src)
	got, err := dst.Templates.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want, err := json.Marshal(vars)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	gotJSON, err := json.Marshal(got.ExtraVars)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if diff := cmp.Diff(string(want), string(gotJSON)); diff != "" {
		t.Errorf("extra vars mismatch (-want +got):\n%s", diff)
	}
}

// TestRoundTripWithoutTheOptionalStores proves a deployment that pins its approval policies from a
// file, and one with no operator-defined credential types, backs up and restores without touching
// either. Both are nil in that wiring, so an unguarded loop would panic in the middle of a recovery.
func TestRoundTripWithoutTheOptionalStores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	src.Policies = nil
	src.CredentialTypes = nil

	var buf bytes.Buffer
	wrote, err := Write(ctx, src, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if wrote.Policies != 0 || wrote.CredentialTypes != 0 {
		t.Errorf("Write counted %d policies and %d credential types from stores that are not "+
			"wired", wrote.Policies, wrote.CredentialTypes)
	}
	if wrote.Users == 0 {
		t.Error("the rest of the install was not backed up")
	}

	dst := freshStores()
	dst.Policies = nil
	dst.CredentialTypes = nil
	read, err := Read(ctx, dst, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.Policies != 0 || read.CredentialTypes != 0 {
		t.Errorf("the restore reported writing to stores that are not wired: %+v", read)
	}
	if read.Users != wrote.Users {
		t.Errorf("restored %d accounts, want %d", read.Users, wrote.Users)
	}
}

// TestRestoreOfPolicyBearingFileIntoAPinnedInstall proves a file holding approval policies restores
// into an install that pins its policies from a file without failing and without writing them. The
// file is that install's source of truth, and a second answer in the database is how a gate stops
// meaning what the operator reads.
func TestRestoreOfPolicyBearingFileIntoAPinnedInstall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)

	var buf bytes.Buffer
	wrote, err := Write(ctx, src, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if wrote.Policies == 0 {
		t.Fatal("the fixture wrote no policies, so this proves nothing")
	}

	dst := freshStores()
	dst.Policies = nil
	read, err := Read(ctx, dst, testSealerOnce(), &buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if read.Policies != 0 {
		t.Errorf("the restore reported %d policies written into an install that pins them",
			read.Policies)
	}
}

// TestRestoreRecomputesOrDropsEveryFireTime pins what a restore does to each shape of schedule.
//
// The scheduler reads any next-run time that is not in the future as due. A restored schedule must
// therefore never come back with a stale time: an enabled one is recomputed from its cron, and one
// whose cadence can never come due is left with no time at all rather than one that fires
// immediately and then again on every tick.
func TestRestoreRecomputesOrDropsEveryFireTime(t *testing.T) {
	t.Parallel()
	stale := time.Now().Add(-72 * time.Hour)

	tests := []struct {
		Name        string
		Schedule    *schedule.Schedule
		WantFuture  bool
		WantDropped bool
	}{{ // Test 0: An enabled schedule is recomputed to its next real occurrence.
		Name: "enabled",
		Schedule: &schedule.Schedule{
			ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", Playbook: "a.yml",
			Enabled: true, CreatedAt: stale, NextRunAt: &stale,
		},
		WantFuture: true,
	}, { // Test 1: A cadence that can never come due is restored with no fire time at all.
		Name: "impossible cadence",
		Schedule: &schedule.Schedule{
			ID: "sch_2", Name: "february thirtieth", Cron: "0 0 30 2 *", Playbook: "a.yml",
			Enabled: true, CreatedAt: stale, NextRunAt: &stale,
		},
		WantDropped: true,
	}, { // Test 2: An unparseable cron is likewise restored with no fire time.
		Name: "unparseable cron",
		Schedule: &schedule.Schedule{
			ID: "sch_3", Name: "broken", Cron: "not a cron", Playbook: "a.yml",
			Enabled: true, CreatedAt: stale, NextRunAt: &stale,
		},
		WantDropped: true,
	}, { // Test 3: A disabled schedule keeps what it had, since the scheduler skips it entirely.
		Name: "disabled",
		Schedule: &schedule.Schedule{
			ID: "sch_4", Name: "off", Cron: "0 2 * * *", Playbook: "a.yml",
			Enabled: false, CreatedAt: stale, NextRunAt: &stale,
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			src := freshStores()
			if err := src.Schedules.Save(ctx, test.Schedule); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			_, _, dst := testRoundTrip(t, ctx, src)
			got, err := dst.Schedules.Get(ctx, test.Schedule.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			switch {
			case test.WantDropped:
				if got.NextRunAt != nil {
					t.Errorf("%s: restored fire time = %s, want none: a schedule that can never "+
						"come due must not be restored as already due", test.Name, got.NextRunAt)
				}
			case test.WantFuture:
				if got.NextRunAt == nil {
					t.Fatalf("%s: the restored schedule has no fire time, so it never runs", test.Name)
				}
				if !got.NextRunAt.After(time.Now()) {
					t.Errorf("%s: restored fire time = %s, which is already due", test.Name,
						got.NextRunAt)
				}
			default:
				if got.NextRunAt == nil || !got.NextRunAt.Equal(stale) {
					t.Errorf("%s: a disabled schedule's fire time changed to %v", test.Name,
						got.NextRunAt)
				}
			}
			if got.Cron != test.Schedule.Cron {
				t.Errorf("%s: cron = %q, want it unchanged", test.Name, got.Cron)
			}
		})
	}
}

// TestBackupHidesEveryFixtureSecret proves the sealed file exposes nothing an install would care
// about, across every kind of secret and every name in the fixture. The file lands wherever backups
// are kept, which is rarely as guarded as the database it came from.
func TestBackupHidesEveryFixtureSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	fillEverything(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, testSealerOnce(), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	file := buf.String()

	tests := []struct {
		Name   string
		Secret string
	}{
		{Name: "sealed credential secret", Secret: "SEALED-SECRET-ONE"}, // Test 0.
		{Name: "password hash", Secret: "argon2id$HASH"},                // Test 1.
		{Name: "api token hash", Secret: "SHA-ONE"},                     // Test 2.
		{Name: "webhook token hash", Secret: "TOKEN-HASH"},              // Test 3.
		{Name: "webhook signing secret", Secret: "SEALED-SIGN"},         // Test 4.
		{Name: "sealed inventory config", Secret: "SEALED-CONFIG"},      // Test 5.
		{Name: "vault path", Secret: "kv/data/ssh"},                     // Test 6.
		{Name: "account name", Secret: "ada@example.com"},               // Test 7.
		{Name: "inventory content", Secret: testUnicode},                // Test 8.
		{Name: "repository url", Secret: "example.com/app.git"},         // Test 9.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if strings.Contains(file, test.Secret) {
				t.Errorf("the backup file exposes the %s %q in the clear", test.Name, test.Secret)
			}
		})
	}
	// The header is deliberately readable so a file is self describing without a key.
	if !strings.Contains(file, Format) {
		t.Error("the backup file does not name its own format, so it cannot be identified")
	}
}
