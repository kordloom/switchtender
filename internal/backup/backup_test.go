package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

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

// testTime is a fixed timestamp so restored objects compare exactly.
var testTime = time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)

// freshStores returns a Stores backed entirely by in-memory stores.
func freshStores() Stores {
	return Stores{
		Credentials:      credential.NewMemStore(),
		Projects:         project.NewMemStore(),
		Templates:        template.NewMemStore(),
		Inventories:      inventory.NewMemStore(),
		InventorySources: invsource.NewMemStore(),
		Schedules:        schedule.NewMemStore(),
		Triggers:         trigger.NewMemStore(),
		Users:            user.NewMemStore(),
		Teams:            team.NewMemStore(),
		Orgs:             org.NewMemStore(),
		Grants:           grant.NewMemStore(),
		Policies:         policy.NewMemStore(),
	}
}

// populate saves one object of each kind, each carrying a distinctive hidden secret field so the
// round trip proves sealed material survives.
func populate(t *testing.T, ctx context.Context, s Stores) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("save fixture: %v", err)
		}
	}
	must(s.Credentials.Save(ctx, &credential.Credential{
		ID: "cred_1", Name: "key", Kind: credential.KindSSHKey, Secret: "SEALED-SECRET", CreatedAt: testTime}))
	must(s.Projects.Save(ctx, &project.Project{
		ID: "proj_1", Name: "app", RepoURL: "https://example.com/y.git", CreatedAt: testTime}))
	must(s.Templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml", CreatedAt: testTime}))
	must(s.Inventories.Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "prod", Content: "[all]\nlocalhost", ContentConfig: "SEALED-CONFIG", CreatedAt: testTime}))
	must(s.InventorySources.Save(ctx, &invsource.Source{
		ID: "src_1", Name: "aws", Source: "aws_ec2", InventoryID: "inv_1", CreatedAt: testTime}))
	must(s.Schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 0 * * *", TemplateID: "tpl_1", Enabled: true, CreatedAt: testTime}))
	must(s.Triggers.Save(ctx, &trigger.Trigger{
		ID: "trg_1", Name: "hook", TemplateID: "tpl_1", TokenHash: "TOKEN-HASH", SigningSecret: "SEALED-SIGN", CreatedAt: testTime}))
	must(s.Users.Save(ctx, &user.User{
		ID: "usr_1", Username: "admin", Role: user.RoleAdmin, PasswordHash: "HASHED-PW", CreatedAt: testTime}))
	must(s.Teams.Save(ctx, &team.Team{ID: "team_1", Name: "sre", CreatedAt: testTime}))
	must(s.Orgs.Save(ctx, &org.Org{ID: "org_1", Name: "acme", CreatedAt: testTime}))
	must(s.Grants.Save(ctx, &grant.Grant{
		ID: "grant_1", Subject: "team_1", Object: "cred_1", Access: grant.AccessUse, CreatedAt: testTime}))
}

// TestRoundTrip proves a populated control plane backs up and restores into fresh stores with every
// object, including hidden secret fields, intact.
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	src := freshStores()
	populate(t, ctx, src)

	var buf bytes.Buffer
	wrote, err := Write(ctx, src, sealer, &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if wrote.Credentials != 1 || wrote.Users != 1 || wrote.Grants != 1 {
		t.Errorf("Write summary = %+v, want one of each kind", wrote)
	}

	dst := freshStores()
	read, err := Read(ctx, dst, sealer, &buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if diff := cmp.Diff(wrote.Credentials, read.Credentials); diff != "" {
		t.Errorf("restored credential count mismatch:\n%s", diff)
	}

	// The hidden secret fields must survive the round trip.
	creds, _ := dst.Credentials.List(ctx)
	if len(creds) != 1 || creds[0].Secret != "SEALED-SECRET" {
		t.Errorf("credential secret not restored: %+v", creds)
	}
	users, _ := dst.Users.List(ctx)
	if len(users) != 1 || users[0].PasswordHash != "HASHED-PW" {
		t.Errorf("user password hash not restored: %+v", users)
	}
	invs, _ := dst.Inventories.List(ctx)
	if len(invs) != 1 || invs[0].ContentConfig != "SEALED-CONFIG" {
		t.Errorf("inventory content config not restored: %+v", invs)
	}
	trigs, _ := dst.Triggers.List(ctx)
	if len(trigs) != 1 || trigs[0].TokenHash != "TOKEN-HASH" || trigs[0].SigningSecret != "SEALED-SIGN" {
		t.Errorf("trigger secrets not restored: %+v", trigs)
	}

	// The non-secret objects must match the originals field for field.
	wantProjects, _ := src.Projects.List(ctx)
	gotProjects, _ := dst.Projects.List(ctx)
	if diff := cmp.Diff(wantProjects, gotProjects); diff != "" {
		t.Errorf("projects mismatch (-want +got):\n%s", diff)
	}
}

// TestConfidential proves the backup never contains a secret in the clear.
func TestConfidential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	src := freshStores()
	populate(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	for _, secret := range []string{"SEALED-SECRET", "HASHED-PW", "SEALED-CONFIG", "SEALED-SIGN", "TOKEN-HASH"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("backup file exposes %q in the clear", secret)
		}
	}
}

// TestWrongKey proves a backup will not restore under a different encryption key.
func TestWrongKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := freshStores()
	populate(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, credential.NewSealer("pass", "salt"), &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := Read(ctx, freshStores(), credential.NewSealer("other", "salt"), &buf); !errors.Is(err, ErrOpen) {
		t.Errorf("Read() with the wrong key error = %v, want ErrOpen", err)
	}
}

// TestTampered proves a modified sealed payload fails authentication on restore.
func TestTampered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	src := freshStores()
	populate(t, ctx, src)
	var buf bytes.Buffer
	if _, err := Write(ctx, src, sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	var env envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	// Flip the last character of the sealed payload.
	last := env.Sealed[len(env.Sealed)-1]
	flip := byte('A')
	if last == 'A' {
		flip = 'B'
	}
	env.Sealed = env.Sealed[:len(env.Sealed)-1] + string(flip)
	altered, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := Read(ctx, freshStores(), sealer, bytes.NewReader(altered)); !errors.Is(err, ErrOpen) {
		t.Errorf("Read() of a tampered file error = %v, want ErrOpen", err)
	}
}

// TestBadFormat proves a foreign or future-version file is refused.
func TestBadFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	tests := []struct {
		Name string
		Body string
	}{
		{Name: "wrong format", Body: `{"format":"something-else","version":1}`},              // Test 0.
		{Name: "unsupported version", Body: `{"format":"switchtender-backup","version":99}`}, // Test 1.
	}
	for _, test := range tests {
		if _, err := Read(ctx, freshStores(), sealer, strings.NewReader(test.Body)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: Read() error = %v, want ErrFormat", test.Name, err)
		}
	}
}

// TestDisabled proves backup and restore refuse to run without an encryption key.
func TestDisabled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	off := credential.NewSealer("", "")
	if _, err := Write(ctx, freshStores(), off, &bytes.Buffer{}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Write() without a key error = %v, want ErrDisabled", err)
	}
	if _, err := Read(ctx, freshStores(), off, strings.NewReader("{}")); !errors.Is(err, ErrDisabled) {
		t.Errorf("Read() without a key error = %v, want ErrDisabled", err)
	}
}

// TestRoundTripCarriesGovernance proves the objects that decide who may do what, and which changes
// wait for a person, survive a backup and restore.
//
// A restore that returns every credential and template but no approval policies looks completely
// successful and leaves the install running every gated change unapproved. The same is true of
// membership: teams and organizations came back as empty shells, so every grant written to one
// reached nobody and access silently narrowed to whoever held a direct grant. Both are the shape of
// failure that is worst here, because the operator's own check says the restore worked.
func TestRoundTripCarriesGovernance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	src := freshStores()
	populate(t, ctx, src)

	if err := src.Policies.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "hold-prod-destroy", Tool: "terraform", CommandContains: "destroy",
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	if err := src.Teams.Save(ctx, &team.Team{ID: "team_1", Name: "ops"}); err != nil {
		t.Fatalf("Save team: %v", err)
	}
	if err := src.Teams.AddMember(ctx, "team_1", "user_1"); err != nil {
		t.Fatalf("AddMember team: %v", err)
	}
	if err := src.Orgs.Save(ctx, &org.Org{ID: "org_1", Name: "acme"}); err != nil {
		t.Fatalf("Save org: %v", err)
	}
	if err := src.Orgs.AddMember(ctx, "org_1", "user_1", org.RoleAdmin); err != nil {
		t.Fatalf("AddMember org: %v", err)
	}

	var buf bytes.Buffer
	wrote, err := Write(ctx, src, sealer, &buf)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if wrote.Policies == 0 {
		t.Error("the backup carried no approval policies, so a restore would gate nothing")
	}
	if wrote.Memberships < 2 {
		t.Errorf("the backup carried %d memberships, want the team and organization ones",
			wrote.Memberships)
	}

	dst := freshStores()
	if _, err := Read(ctx, dst, sealer, &buf); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	pols, err := dst.Policies.List(ctx)
	if err != nil {
		t.Fatalf("List policies: %v", err)
	}
	if len(pols) != 1 || pols[0].Name != "hold-prod-destroy" {
		t.Errorf("restored policies = %+v, want the approval gate back", pols)
	}
	members, err := dst.Teams.Members(ctx, "team_1")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0] != "user_1" {
		t.Errorf("restored team members = %v, want user_1; grants to this team reach nobody", members)
	}
	orgMembers, err := dst.Orgs.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("Org Members: %v", err)
	}
	if len(orgMembers) != 1 || orgMembers[0].UserID != "user_1" {
		t.Errorf("restored org members = %+v, want user_1", orgMembers)
	}
	if len(orgMembers) == 1 && orgMembers[0].Role != org.RoleAdmin {
		t.Errorf("restored org role = %q, want it preserved", orgMembers[0].Role)
	}
}

// TestRestoreDoesNotFireEverySchedule proves a restored schedule waits for its next real occurrence
// rather than being due the moment the restore finishes.
//
// The scheduler treats any next-run time that is not in the future as due. A backup carries the
// time that was current when it was written, so restoring a day-old snapshot made every enabled
// schedule due at once: the estate's whole nightly workload fired together, against an install
// somebody was still bringing up.
func TestRestoreDoesNotFireEverySchedule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("pass", "salt")
	src := freshStores()

	stale := time.Now().Add(-48 * time.Hour)
	if err := src.Schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", Playbook: "site.yml",
		Enabled: true, CreatedAt: stale, NextRunAt: &stale,
	}); err != nil {
		t.Fatalf("Save schedule: %v", err)
	}

	var buf bytes.Buffer
	if _, err := Write(ctx, src, sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	dst := freshStores()
	if _, err := Read(ctx, dst, sealer, &buf); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	got, err := dst.Schedules.Get(ctx, "sch_1")
	if err != nil {
		t.Fatalf("Get schedule: %v", err)
	}
	if got.NextRunAt == nil {
		t.Fatal("the restored schedule has no next fire time, so it would never run")
	}
	if !got.NextRunAt.After(time.Now()) {
		t.Errorf("restored next fire = %s, which is already due: the restore would fire every "+
			"enabled schedule at once", got.NextRunAt)
	}
	// The cadence itself is preserved; only the stale occurrence is dropped.
	if got.Cron != "0 2 * * *" {
		t.Errorf("cron = %q, want it unchanged", got.Cron)
	}
}
