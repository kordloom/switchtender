package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// seedStores holds one memory store per dependency the seeder writes through, so a test can build a
// full Deps and then read back everything that was seeded.
type seedStores struct {
	// Runs holds the seeded runs.
	Runs run.Store
	// Projects holds the seeded git projects.
	Projects project.Store
	// Inventories holds the seeded inventories.
	Inventories inventory.Store
	// Templates holds the seeded job templates.
	Templates template.Store
	// Credentials holds the seeded credentials.
	Credentials credential.Store
	// Policies holds the seeded approval rules.
	Policies policy.Store
	// Users holds the seeded accounts.
	Users user.Store
	// InvSources holds the seeded dynamic inventory sources.
	InvSources invsource.Store
	// Schedules holds the seeded cron entries.
	Schedules schedule.Store
	// Audit holds the seeded change history.
	Audit audit.Store
}

// newSeedStores returns a full set of empty memory stores.
func newSeedStores() *seedStores {
	return &seedStores{
		Runs:        run.NewMemStore(),
		Projects:    project.NewMemStore(),
		Inventories: inventory.NewMemStore(),
		Templates:   template.NewMemStore(),
		Credentials: credential.NewMemStore(),
		Policies:    policy.NewMemStore(),
		Users:       user.NewMemStore(),
		InvSources:  invsource.NewMemStore(),
		Schedules:   schedule.NewMemStore(),
		Audit:       audit.NewMemStore(),
	}
}

// deps builds a Deps from the stores, with no submitter or clock attached.
func (s *seedStores) deps() Deps {
	return Deps{
		Runs: s.Runs, Projects: s.Projects, Inventories: s.Inventories,
		Templates: s.Templates, Credentials: s.Credentials, Policies: s.Policies,
		Users: s.Users, InvSources: s.InvSources, Schedules: s.Schedules, Audit: s.Audit,
	}
}

// terminalSubmitter accepts every run shape the seeder produces, stores the run in a terminal state,
// and commits the outcome entry the seeder waits for. It stands in for the dispatcher so a whole
// seed runs without ansible, terraform, or any process execution.
type terminalSubmitter struct {
	// runs holds every run the seeder submitted.
	runs run.Store
	// audits receives the outcome entry each finished run commits.
	audits audit.Store
	// clock stamps the run the way the dispatcher's clock would.
	clock *SeedClock
}

// submit records one run in a terminal state and commits its outcome to the chain.
func (f *terminalSubmitter) submit(ctx context.Context, r *run.Run, opts ...run.SubmitOption) (*run.Run, error) {
	for _, o := range opts {
		o(r)
	}
	now := time.Now()
	if f.clock != nil {
		now = f.clock.Now()
	}
	r.CreatedAt = now
	// A held run never executes, so it keeps the status the policy options gave it and records no
	// start or end, exactly as the dispatcher would leave it.
	if r.Status != run.StatusPendingApproval {
		started := now
		ended := now
		r.StartedAt = &started
		r.EndedAt = &ended
		r.Status = run.StatusSucceeded
	}
	if err := f.runs.Save(ctx, r); err != nil {
		return nil, err
	}
	if f.audits != nil && r.Status.Terminal() {
		_ = f.audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: now, Actor: r.Actor,
			Method: audit.MethodRun, Path: "/v1/runs/" + r.ID,
		})
	}
	return r, nil
}

// Submit records a single run.
func (f *terminalSubmitter) Submit(ctx context.Context, playbook, inv string,
	opts ...run.SubmitOption) (*run.Run, error) {
	return f.submit(ctx, &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inv, Status: run.StatusPending,
	}, opts...)
}

// SubmitSplit records a sharded run as one parent.
func (f *terminalSubmitter) SubmitSplit(ctx context.Context, playbook, inv string, shards int,
	opts ...run.SubmitOption) (*run.Run, error) {
	return f.submit(ctx, &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inv,
		Kind: run.KindSplit, Status: run.StatusPending,
	}, opts...)
}

// SubmitPipeline records an ordered set of steps as one parent.
func (f *terminalSubmitter) SubmitPipeline(ctx context.Context, name, inv string,
	steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	return f.submit(ctx, &run.Run{
		ID: run.NewID(), Playbook: name, Inventory: inv, Steps: steps,
		Kind: run.KindPipeline, Status: run.StatusPending,
	}, opts...)
}

// releasingApprover releases a held run the way the dispatcher does, refusing a decision by the
// account that asked for the run.
type releasingApprover struct {
	// runs holds the run being decided on.
	runs run.Store
}

// Approve releases a held run and records that it then executed.
func (a *releasingApprover) Approve(ctx context.Context, id, by, byType string) (*run.Run, error) {
	r, err := a.runs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.RequireDistinctApprover && by == r.Actor {
		return nil, fmt.Errorf("%q asked for this run and cannot release it", by)
	}
	r.Status = run.StatusSucceeded
	return r, a.runs.Save(ctx, r)
}

// treeDigest returns a stable fingerprint of every file under dir, skipping git metadata, so two
// materializations can be compared byte for byte.
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestMaterializeIsDeterministicAndSelfCleaning pins that writing the demo assets out twice produces
// exactly the same tree.
//
// The seeder writes to one fixed path rather than a random one, because the path lands in the run
// records a visitor reads. That makes leftovers from an earlier seed a real hazard: a file removed
// from the embedded assets, or edited, would otherwise survive on disk and the demo would run
// something no longer in the binary. Reseeding has to reproduce the tree exactly and drop anything
// that is not part of it.
func TestMaterializeIsDeterministicAndSelfCleaning(t *testing.T) {
	// Not parallel: materialize writes to one fixed path under the system temp directory.
	first, err := materialize()
	if err != nil {
		t.Fatalf("materialize() error = %v", err)
	}
	before := treeDigest(t, first)
	if len(before) == 0 {
		t.Fatal("materialize() wrote no files")
	}

	// The assets a seed actually runs must all be there.
	for _, want := range []string{
		"site.yml", "inv.ini", "facts.yml", "drift.yml",
		"terraform/main.tf",
		"repos/web-platform/site.yml", "repos/database-ops/infra/network/main.tf",
		// The drift check compares these two trees, so a materialization missing either reports
		// every host as drifted or fails outright.
		"config/desired/app.conf", "config/previous/app.conf",
		"state/web01/app.conf", "state/edge01/packages.pin",
	} {
		if _, ok := before[want]; !ok {
			t.Errorf("materialize() did not write %s", want)
		}
	}

	// Debris from a previous seed, which the next materialization must remove.
	stale := filepath.Join(first, "left-behind.yml")
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	second, err := materialize()
	if err != nil {
		t.Fatalf("materialize() second call error = %v", err)
	}
	if second != first {
		t.Errorf("materialize() returned %q then %q, want the same fixed path", first, second)
	}
	after := treeDigest(t, second)
	if diff := cmp.Diff(before, after, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the second materialization differs from the first (-first +second):\n%s", diff)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("debris from the previous seed survived, err = %v", err)
	}
}

// TestMaterializeBuildsRunnableDemoRepositories pins that each materialized project subtree is a
// real git repository with a commit on main. The seeded projects clone from these paths, so a
// subtree that is not a repository turns a launched template into a git failure on the public demo.
func TestMaterializeBuildsRunnableDemoRepositories(t *testing.T) {
	// Not parallel: materialize writes to one fixed path under the system temp directory.
	dir, err := materialize()
	if err != nil {
		t.Fatalf("materialize() error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "repos"))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no demo repositories were materialized")
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		head := filepath.Join(dir, "repos", entry.Name(), ".git", "HEAD")
		body, err := os.ReadFile(head)
		if err != nil {
			t.Errorf("%s is not a git repository: %v", entry.Name(), err)
			continue
		}
		if !strings.Contains(string(body), "refs/heads/main") {
			t.Errorf("%s HEAD = %q, want main, which is the branch the seeded project pins",
				entry.Name(), strings.TrimSpace(string(body)))
		}
	}
}

// TestInitDemoReposRefusesAMissingReposTree pins that the repository step reports a missing assets
// tree rather than seeding projects that point at nothing. A project whose remote does not exist
// fails on every launch, and the failure would be attributed to git rather than to the seed.
func TestInitDemoReposRefusesAMissingReposTree(t *testing.T) {
	t.Parallel()
	if err := initDemoRepos(t.TempDir()); err == nil {
		t.Error("initDemoRepos() with no repos tree = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "read demo repos") {
		t.Errorf("initDemoRepos() error = %v, want the missing repos tree named", err)
	}
}

// TestSeedConfigProducesTheSameShapeEveryTime pins that seeding twice into fresh stores yields the
// same catalog.
//
// The demo is reseeded on a schedule against a public instance, so a visitor comparing two days
// apart must see the same projects, inventories, templates, policies, schedules, and accounts.
// Identifiers and timestamps are generated per seed and are deliberately not compared; everything a
// visitor actually reads is. The collected lines are sorted before comparison because one seeded
// pair does not hold a stable order, which TestSeedConfigOrdersCredentialsStably covers on its own.
func TestSeedConfigProducesTheSameShapeEveryTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	shape := func() []string {
		stores := newSeedStores()
		seedConfig(ctx, stores.deps(), zap.NewNop())
		var out []string

		projects, err := stores.Projects.List(ctx)
		if err != nil {
			t.Fatalf("Projects.List() error = %v", err)
		}
		for _, p := range projects {
			out = append(out, fmt.Sprintf("project %s %s %s", p.Name, p.Branch, p.RepoURL))
		}
		inventories, err := stores.Inventories.List(ctx)
		if err != nil {
			t.Fatalf("Inventories.List() error = %v", err)
		}
		for _, inv := range inventories {
			out = append(out, "inventory "+inv.Name)
		}
		templates, err := stores.Templates.List(ctx)
		if err != nil {
			t.Fatalf("Templates.List() error = %v", err)
		}
		for _, tpl := range templates {
			out = append(out, fmt.Sprintf("template %s %s %d", tpl.Name, tpl.Tool, tpl.Shards))
		}
		creds, err := stores.Credentials.List(ctx)
		if err != nil {
			t.Fatalf("Credentials.List() error = %v", err)
		}
		for _, c := range creds {
			out = append(out, fmt.Sprintf("credential %s %s", c.Name, c.Kind))
		}
		policies, err := stores.Policies.List(ctx)
		if err != nil {
			t.Fatalf("Policies.List() error = %v", err)
		}
		for _, p := range policies {
			out = append(out, fmt.Sprintf("policy %s %s %s", p.Name, p.Tool, p.CommandContains))
		}
		schedules, err := stores.Schedules.List(ctx)
		if err != nil {
			t.Fatalf("Schedules.List() error = %v", err)
		}
		for _, sc := range schedules {
			out = append(out, fmt.Sprintf("schedule %s %s %s %v", sc.Name, sc.Cron, sc.Timezone, sc.Enabled))
		}
		users, err := stores.Users.List(ctx)
		if err != nil {
			t.Fatalf("Users.List() error = %v", err)
		}
		for _, u := range users {
			out = append(out, fmt.Sprintf("user %s %s", u.Username, u.Role))
		}
		sources, err := stores.InvSources.List(ctx)
		if err != nil {
			t.Fatalf("InvSources.List() error = %v", err)
		}
		for _, src := range sources {
			out = append(out, fmt.Sprintf("source %s %s %d", src.Name, src.Source, src.SyncIntervalSeconds))
		}
		sort.Strings(out)
		return out
	}

	first := shape()
	if len(first) == 0 {
		t.Fatal("seedConfig() stored nothing")
	}
	if diff := cmp.Diff(first, shape(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("a second seed produced a different catalog (-first +second):\n%s", diff)
	}
}

// TestSeedConfigStoresRepositoriesTheSyncerWillAccept pins the seam between the two packages. The
// seeded projects are only useful if the syncer will clone them: a project whose URL the validator
// refuses shows up on the demo and fails the moment anyone launches its template, which is worse
// than not seeding it.
//
// It also pins that no seeded project publishes a path off the demo server's disk. The remote is
// the one column a visitor reads to learn where the demo's playbooks come from, and a file URL
// under a temporary directory reads as an install somebody never finished.
func TestSeedConfigStoresRepositoriesTheSyncerWillAccept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	projects, err := stores.Projects.List(ctx)
	if err != nil {
		t.Fatalf("Projects.List() error = %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("no projects were seeded")
	}
	for _, p := range projects {
		if err := project.ValidateRepoURL(p.RepoURL); err != nil {
			t.Errorf("project %q has a repository the syncer refuses: %v", p.Name, err)
		}
		if p.Branch != "main" {
			t.Errorf("project %q pins branch %q, want the branch the demo repository is on", p.Name, p.Branch)
		}
		if !strings.HasPrefix(p.RepoURL, "https://") {
			t.Errorf("project %q publishes %q, want a remote a reader can recognize as one",
				p.Name, p.RepoURL)
		}
		if strings.Contains(p.RepoURL, os.TempDir()) || strings.Contains(p.RepoURL, "/tmp/") {
			t.Errorf("project %q publishes a scratch path on the demo server: %q", p.Name, p.RepoURL)
		}
	}
}

// TestSeedConfigKeepsTheChangeHistoryOlderThanTheRunWindow pins the one ordering invariant that
// makes the seeded chain read like a live one.
//
// The runs commit their outcomes to the same chain after this history is appended, so the history
// has to be older than the oldest run or a config change from hours ago lands beneath a newer run
// outcome. The margin comes from the backshift being larger than the run window, and a change to
// either constant that closed that margin would invert the chain's times against its sequence.
func TestSeedConfigKeepsTheChangeHistoryOlderThanTheRunWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	before := time.Now()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	chain, err := stores.Audit.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("no change history was seeded, so the audit page has nothing to verify")
	}
	windowOpens := before.Add(-seedRunWindow)
	for i, e := range chain {
		if !e.At.Before(windowOpens) {
			t.Errorf("history entry %d is stamped %v, which is inside the run window opening at %v",
				i, e.At, windowOpens)
		}
		if int64(i+1) != e.Seq {
			t.Errorf("entry %d carries sequence %d, want a contiguous chain", i, e.Seq)
		}
		if e.Hash == "" {
			t.Errorf("entry %d carries no link hash, so nothing can be verified", i)
		}
		if i > 0 && e.PrevHash != chain[i-1].Hash {
			t.Errorf("entry %d does not link to the one before it", i)
		}
	}
	// The entries are appended oldest first, so their times must not go backward either.
	for i := 1; i < len(chain); i++ {
		if chain[i].At.Before(chain[i-1].At) {
			t.Errorf("history entry %d is stamped before entry %d, so the chain reads out of order",
				i, i-1)
		}
	}
}

// TestSeedConfigStoresHashedDemoPasswords pins that the demo accounts carry a hash rather than the
// password they were built from. The demo instance is public, its store is browsable through the
// API, and a seeded password sitting in a user row would be a credential handed to every visitor.
func TestSeedConfigStoresHashedDemoPasswords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	users, err := stores.Users.List(ctx)
	if err != nil {
		t.Fatalf("Users.List() error = %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("seeded %d accounts, want the three the demo shows", len(users))
	}
	roles := make(map[user.Role]string, len(users))
	for _, u := range users {
		if u.PasswordHash == "" {
			t.Errorf("account %q carries no password hash", u.Username)
		}
		if strings.Contains(u.PasswordHash, "demo-password") {
			t.Errorf("account %q stores its password rather than a hash", u.Username)
		}
		roles[u.Role] = u.Username
	}
	for _, want := range []user.Role{user.RoleAdmin, user.RoleOperator, user.RoleViewer} {
		if roles[want] == "" {
			t.Errorf("no seeded account holds the %s role, so that page shows an incomplete fleet", want)
		}
	}
}

// TestSeedConfigPairsADynamicSourceWithItsInventory pins the relationship the sources page exists to
// show. A source whose inventory id names nothing renders as a dangling reference, which is the
// empty page this seed was written to replace.
func TestSeedConfigPairsADynamicSourceWithItsInventory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	sources, err := stores.InvSources.List(ctx)
	if err != nil {
		t.Fatalf("InvSources.List() error = %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("seeded %d inventory sources, want one", len(sources))
	}
	src := sources[0]
	if _, err := stores.Inventories.Get(ctx, src.InventoryID); err != nil {
		t.Errorf("source %q points at inventory %q, which was not seeded: %v",
			src.Name, src.InventoryID, err)
	}
	if !src.UpdateOnLaunch || src.SyncIntervalSeconds == 0 || src.SyncedAt == nil {
		t.Errorf("source %+v does not read as a source that has ever refreshed", src)
	}
}

// TestSeedConfigSchedulesFireOnTheirOwnCadence pins that each seeded schedule's next fire really is
// the next fire of the expression beside it.
//
// Seeding the next run as a fixed offset showed "daily at 2am" firing next at 3:18, which on an
// audit and scheduling product reads as the schedule itself being wrong. A disabled schedule carries
// no next fire at all, because a cadence that is switched off is not going to happen.
//
// Every schedule also names the zone its expression is read in, and the next fire is that
// expression's next fire in that zone. Seeded without one, the page showed "0 2 * * *" and nothing
// else, which tells a visitor that something fires at two o'clock in a zone they cannot see, and
// the next fire was computed against whatever zone the demo server happened to sit in.
func TestSeedConfigSchedulesFireOnTheirOwnCadence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	schedules, err := stores.Schedules.List(ctx)
	if err != nil {
		t.Fatalf("Schedules.List() error = %v", err)
	}
	if len(schedules) != 3 {
		t.Fatalf("seeded %d schedules, want three", len(schedules))
	}
	for _, sc := range schedules {
		if sc.Cron == "" {
			t.Errorf("schedule %q carries no cron expression", sc.Name)
		}
		if sc.Timezone == "" {
			t.Errorf("schedule %q names no timezone, so %q on the page means nothing on its own",
				sc.Name, sc.Cron)
			continue
		}
		zone, err := time.LoadLocation(sc.Timezone)
		if err != nil {
			t.Errorf("schedule %q names timezone %q, which will not resolve: %v",
				sc.Name, sc.Timezone, err)
			continue
		}
		if _, err := stores.Templates.Get(ctx, sc.TemplateID); err != nil {
			t.Errorf("schedule %q names template %q, which was not seeded: %v", sc.Name, sc.TemplateID, err)
		}
		if !sc.Enabled {
			if sc.NextRunAt != nil {
				t.Errorf("disabled schedule %q claims a next fire at %v", sc.Name, *sc.NextRunAt)
			}
			continue
		}
		if sc.NextRunAt == nil {
			t.Errorf("enabled schedule %q has no next fire, so the page shows a blank cadence", sc.Name)
			continue
		}
		if !sc.NextRunAt.After(time.Now()) {
			t.Errorf("schedule %q fires next at %v, which is in the past", sc.Name, *sc.NextRunAt)
		}
		// Read in the schedule's own zone, which is the reading the page shows beside the cadence.
		if sc.Cron == "0 2 * * *" {
			at := sc.NextRunAt.In(zone)
			if h, m := at.Hour(), at.Minute(); h != 2 || m != 0 {
				t.Errorf("schedule %q reads as daily at 2am %s but fires next at %02d:%02d there",
					sc.Name, sc.Timezone, h, m)
			}
		}
	}
}

// TestSeedConfigStoresPoliciesTheGovernanceSeedNames pins that the two seeded rules are the two the
// held and released runs cite. A rule named on a run but absent from the policy list leaves the
// demo showing a change that was stopped by something a visitor cannot look up.
func TestSeedConfigStoresPoliciesTheGovernanceSeedNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	policies, err := stores.Policies.List(ctx)
	if err != nil {
		t.Fatalf("Policies.List() error = %v", err)
	}
	byName := make(map[string]*policy.Policy, len(policies))
	for _, p := range policies {
		byName[p.Name] = p
	}
	tfDestroy, ok := byName["prod terraform destroy"]
	if !ok {
		t.Fatal("the rule the held run cites was not seeded")
	}
	if tfDestroy.Tool != run.ToolTerraform || tfDestroy.CommandContains != "destroy" {
		t.Errorf("the terraform rule = %+v, want it matching a terraform destroy", tfDestroy)
	}
	if !tfDestroy.ExcludeDryRun {
		t.Error("the terraform rule holds plans as well as destroys, so every seeded plan would be gated")
	}
	anyProd, ok := byName["any production run"]
	if !ok {
		t.Fatal("the rule the released run cites was not seeded")
	}
	if _, err := stores.Inventories.Get(ctx, anyProd.InventoryID); err != nil {
		t.Errorf("the production rule matches inventory %q, which was not seeded: %v",
			anyProd.InventoryID, err)
	}
}

// TestSeedConfigCredentialsCarryPlaceholderMaterial pins that each seeded credential holds sealed
// material. The doctor flags a credential with no secret, and a demo full of warnings reads as a
// misconfigured install rather than a working one.
func TestSeedConfigCredentialsCarryPlaceholderMaterial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	creds, err := stores.Credentials.List(ctx)
	if err != nil {
		t.Fatalf("Credentials.List() error = %v", err)
	}
	if len(creds) != 4 {
		t.Fatalf("seeded %d credentials, want four", len(creds))
	}
	kinds := make(map[string]bool, len(creds))
	for _, c := range creds {
		if c.Secret == "" {
			t.Errorf("credential %q carries no secret, which the doctor flags", c.Name)
		}
		if c.Kind == "" {
			t.Errorf("credential %q carries no kind", c.Name)
		}
		if kinds[string(c.Kind)] {
			t.Errorf("credential kind %q was seeded twice", c.Kind)
		}
		kinds[string(c.Kind)] = true
	}
}

// TestSeedConfigTemplatesCoverTheAdvertisedTools pins that the templates list shows a preset for
// each tool the engine drives, even on a host that lacks the binary. The list is configuration
// rather than execution, so a missing tool is no reason for the page to show three entries when the
// product advertises five.
func TestSeedConfigTemplatesCoverTheAdvertisedTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	seedConfig(ctx, stores.deps(), zap.NewNop())

	templates, err := stores.Templates.List(ctx)
	if err != nil {
		t.Fatalf("Templates.List() error = %v", err)
	}
	tools := make(map[string]bool, len(templates))
	for _, tpl := range templates {
		tools[run.NormalizeTool(tpl.Tool)] = true
		if tpl.ProjectID == "" {
			t.Errorf("template %q names no project", tpl.Name)
			continue
		}
		if _, err := stores.Projects.Get(ctx, tpl.ProjectID); err != nil {
			t.Errorf("template %q names project %q, which was not seeded: %v", tpl.Name, tpl.ProjectID, err)
		}
	}
	var want []string
	for _, tool := range []string{run.ToolAnsible, run.ToolBash, run.ToolTerraform,
		run.ToolPython, run.ToolGo} {
		if !tools[tool] {
			want = append(want, tool)
		}
	}
	sort.Strings(want)
	if len(want) > 0 {
		t.Errorf("no template covers %v, so the templates page understates what the engine drives", want)
	}
}

// TestNormalizeClaimStampsReseatsTheClaimBetweenCreationAndStart pins the pass that keeps a seeded
// run's timeline readable.
//
// The store stamps claimed_at from the real wall clock, not the seed clock, so a run whose record
// sits hours in the past was otherwise marked claimed moments before now. That left created and
// ended hours ago beside a claim from just now, a contradiction against the audit trail that a
// careful reader would catch on the one install strangers try first.
func TestNormalizeClaimStampsReseatsTheClaimBetweenCreationAndStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := time.Now().Add(-6 * time.Hour)
	started := created.Add(2 * time.Minute)
	ended := started.Add(3 * time.Minute)
	realNow := time.Now()

	tests := []struct {
		// Name labels the run's stamp shape.
		Name string
		// Run is the seeded run before normalization.
		Run *run.Run
		// WantMoved is whether the claim must be pulled back.
		WantMoved bool
	}{{ // Test 0: A claim stamped at the seed instant is pulled back into the run's own window.
		Name: "claim from the wall clock",
		Run: &run.Run{
			ID: "run_late", Status: run.StatusSucceeded, CreatedAt: created,
			StartedAt: &started, EndedAt: &ended, ClaimedAt: &realNow,
		},
		WantMoved: true,
	}, { // Test 1: A run that was never claimed is left exactly as it is.
		Name: "never claimed",
		Run: &run.Run{
			ID: "run_unclaimed", Status: run.StatusSucceeded, CreatedAt: created,
			StartedAt: &started, EndedAt: &ended,
		},
	}, { // Test 2: A claim already sitting in order is left alone.
		Name: "already in order",
		Run: &run.Run{
			ID: "run_ordered", Status: run.StatusSucceeded, CreatedAt: created,
			StartedAt: &started, EndedAt: &ended, ClaimedAt: ptrTime(created.Add(30 * time.Second)),
		},
	}, { // Test 3: A run that never started falls back to its creation time.
		Name: "never started",
		Run: &run.Run{
			ID: "run_nostart", Status: run.StatusFailed, CreatedAt: created, ClaimedAt: &realNow,
		},
		WantMoved: true,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs := run.NewMemStore()
			r := test.Run.Clone()
			if err := runs.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			deps := Deps{Runs: runs, Clock: NewSeedClock()}
			normalizeClaimStamps(ctx, deps, zap.NewNop())

			got, err := runs.Get(ctx, test.Run.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if test.Run.ClaimedAt == nil {
				if got.ClaimedAt != nil {
					t.Errorf("%s: an unclaimed run was given a claim at %v", test.Name, *got.ClaimedAt)
				}
				return
			}
			if got.ClaimedAt == nil {
				t.Fatalf("%s: the claim stamp was removed", test.Name)
			}
			if got.ClaimedAt.Before(got.CreatedAt) {
				t.Errorf("%s: claimed %v is before created %v", test.Name, *got.ClaimedAt, got.CreatedAt)
			}
			if got.StartedAt != nil && got.ClaimedAt.After(*got.StartedAt) {
				t.Errorf("%s: claimed %v is after started %v", test.Name, *got.ClaimedAt, *got.StartedAt)
			}
			moved := !got.ClaimedAt.Equal(*test.Run.ClaimedAt)
			if moved != test.WantMoved {
				t.Errorf("%s: claim moved = %v, want %v (claim is now %v)",
					test.Name, moved, test.WantMoved, *got.ClaimedAt)
			}
		})
	}
}

// ptrTime returns a pointer to t, for building run stamps inline.
func ptrTime(t time.Time) *time.Time { return &t }

// TestNormalizeClaimStampsIsAHistoricalSeedOnlyPass pins that the correction runs only when the
// demo clock is in play. Without the seed clock the store's claim stamp already matches the rest of
// the run, so touching it would move a stamp that was correct.
func TestNormalizeClaimStampsIsAHistoricalSeedOnlyPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	created := time.Now().Add(-time.Hour)
	claimed := time.Now()
	if err := runs.Save(ctx, &run.Run{
		ID: "run_live", Status: run.StatusSucceeded, CreatedAt: created, ClaimedAt: &claimed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Test 0: no clock configured leaves every stamp exactly where it was.
	normalizeClaimStamps(ctx, Deps{Runs: runs}, zap.NewNop())
	got, err := runs.Get(ctx, "run_live")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.ClaimedAt.Equal(claimed) {
		t.Errorf("claim moved to %v without a seed clock, want it left at %v", *got.ClaimedAt, claimed)
	}

	// Test 1: no run store configured is a no-op rather than a panic.
	normalizeClaimStamps(ctx, Deps{Clock: NewSeedClock()}, zap.NewNop())
}

// TestSeedMultiToolSkipsMissingToolsAndWarnsOnce covers the seeder's handling of a host that lacks
// the binaries the product advertises.
//
// A missing tool has to be skipped rather than left as an exec-not-found failure, because a broken
// looking run is worse than an absent one. The warning has to be said once, naming what a visitor
// will not see: three separate skip lines in a log is a thing nobody reads, and a demo showing three
// of five tools with no explanation is a weak first impression.
func TestSeedMultiToolSkipsMissingToolsAndWarnsOnce(t *testing.T) {
	// Not parallel: it replaces PATH for the process so no optional tool resolves.
	ctx := context.Background()
	t.Setenv("PATH", t.TempDir())

	stores := newSeedStores()
	deps := stores.deps()
	deps.Submitter = &terminalSubmitter{runs: stores.Runs, audits: stores.Audit}

	core, logs := observer.New(zapcore.WarnLevel)
	if err := seedMultiTool(ctx, deps, "/srv/infra", "site.yml", "inv.ini", zap.New(core)); err != nil {
		t.Fatalf("seedMultiTool() error = %v", err)
	}

	runs, err := stores.Runs.List(ctx)
	if err != nil {
		t.Fatalf("Runs.List() error = %v", err)
	}
	// Bash always runs, and the mixed pipeline always runs, whatever the host is missing.
	if len(runs) != 2 {
		t.Errorf("seeded %d runs on a host with no optional tools, want the bash run and the pipeline",
			len(runs))
	}
	for _, r := range runs {
		if !r.Status.Terminal() {
			t.Errorf("run %s is %s, so the demo shows a run that never finished", r.ID, r.Status)
		}
	}

	warnings := logs.FilterMessageSnippet("seeded without").All()
	if len(warnings) != 1 {
		t.Fatalf("the missing tools were reported in %d warnings, want exactly one", len(warnings))
	}
	msg := warnings[0].Message
	for _, tool := range []string{"terraform", "go"} {
		if !strings.Contains(msg, tool) {
			t.Errorf("the warning does not name %s: %q", tool, msg)
		}
	}
}

// TestSeedMultiToolReportsASubmitFailure pins that a submitter refusing the very first run stops the
// seed with that error rather than pressing on. A demo seeded on top of a broken dispatcher would
// show a partial history with no indication that anything went wrong.
func TestSeedMultiToolReportsASubmitFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	deps := stores.deps()
	deps.Submitter = &refusingSubmitter{}

	err := seedMultiTool(ctx, deps, "/srv/infra", "site.yml", "inv.ini", zap.NewNop())
	if err == nil {
		t.Fatal("seedMultiTool() with a refusing submitter = nil error, want the failure returned")
	}
	if !strings.Contains(err.Error(), "seed bash run") {
		t.Errorf("seedMultiTool() error = %v, want the bash run named", err)
	}
}

// refusingSubmitter refuses every run, standing in for a dispatcher that cannot accept work.
type refusingSubmitter struct{}

// Submit refuses.
func (refusingSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run, error) {
	return nil, errAppendRefused
}

// SubmitSplit refuses.
func (refusingSubmitter) SubmitSplit(context.Context, string, string, int,
	...run.SubmitOption) (*run.Run, error) {
	return nil, errAppendRefused
}

// SubmitPipeline refuses.
func (refusingSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep,
	...run.SubmitOption) (*run.Run, error) {
	return nil, errAppendRefused
}

// TestSeedRunsEndToEnd drives the whole seeder against stores and a stand-in dispatcher, so every
// step from materializing the assets to normalizing the claim stamps is exercised without ansible or
// any other tool executing.
//
// It is the test that would catch a seed which returns an error, leaves a run stuck short of a
// terminal state, or stamps a seeded run at the present instant. Each of those turns the public
// demo into an install that contradicts the product's own claims about its records.
func TestSeedRunsEndToEnd(t *testing.T) {
	// Not parallel: it materializes the real assets to their fixed path.
	ctx := context.Background()
	stores := newSeedStores()
	clock := NewSeedClock()
	deps := stores.deps()
	deps.Clock = clock
	deps.Submitter = &terminalSubmitter{runs: stores.Runs, audits: stores.Audit, clock: clock}
	deps.Approver = &releasingApprover{runs: stores.Runs}

	if err := Seed(ctx, deps, zap.NewNop()); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	runs, err := stores.Runs.List(ctx)
	if err != nil {
		t.Fatalf("Runs.List() error = %v", err)
	}
	if len(runs) < 12 {
		t.Errorf("Seed() produced %d runs, want the full sample history", len(runs))
	}

	var held int
	for _, r := range runs {
		if r.Status == run.StatusPendingApproval {
			held++
			if r.HeldByPolicy == "" {
				t.Errorf("run %s is held but names no rule", r.ID)
			}
			continue
		}
		if !r.Status.Terminal() {
			t.Errorf("run %s is %s, so the demo shows a run that never finished", r.ID, r.Status)
		}
		if r.CreatedAt.After(time.Now().Add(-seedRunMargin)) {
			t.Errorf("run %s was created at %v, which is not safely in the past", r.ID, r.CreatedAt)
		}
		if r.Actor == "" || r.Source == "" {
			t.Errorf("run %s carries no provenance, so the runs list shows blanks: %+v", r.ID, r)
		}
		if r.AuditReceipt == "" {
			t.Errorf("run %s carries no creation receipt, so its receipt button fails", r.ID)
		}
	}
	if held == 0 {
		t.Error("no run is left held by the gate, so the demo shows no change being refused")
	}

	// The seeded history spreads across the window rather than piling into one instant.
	oldest, newest := runs[0].CreatedAt, runs[0].CreatedAt
	for _, r := range runs {
		if r.CreatedAt.Before(oldest) {
			oldest = r.CreatedAt
		}
		if r.CreatedAt.After(newest) {
			newest = r.CreatedAt
		}
	}
	if newest.Sub(oldest) < time.Hour {
		t.Errorf("the seeded runs span %v, want a history spread across hours", newest.Sub(oldest))
	}

	// The whole chain still reads in order: configuration history first, run outcomes after.
	chain, err := stores.Audit.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("Seed() left an empty chain, so the verify endpoint has nothing to check")
	}
	for i := 1; i < len(chain); i++ {
		if chain[i].PrevHash != chain[i-1].Hash {
			t.Fatalf("chain entry %d does not link to the one before it", i)
		}
	}
}

// TestSeedConfigOrdersCredentialsStably demonstrates an ordering instability in the seeded catalog.
//
// The credential store lists oldest first and breaks a tie on the identifier. Two seeded
// credentials, prod-ssh and ansible-vault, are stamped with the same creation time, and every seed
// mints fresh random identifiers, so the tie is broken by a different value each time and the two
// swap places on the credentials page between reseeds. The public demo is reseeded on a schedule,
// so the list a visitor reads today is not the list they read yesterday, on a product whose pitch
// is that its records reproduce.
func TestSeedConfigOrdersCredentialsStably(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	names := func() []string {
		stores := newSeedStores()
		seedConfig(ctx, stores.deps(), zap.NewNop())
		creds, err := stores.Credentials.List(ctx)
		if err != nil {
			t.Fatalf("Credentials.List() error = %v", err)
		}
		var out []string
		for _, c := range creds {
			out = append(out, c.Name)
		}
		return out
	}

	first := names()
	for range 8 {
		if diff := cmp.Diff(first, names(), cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("the credentials list reordered between seeds (-first +later):\n%s", diff)
		}
	}
}

// TestSeedSeedsSchedulesWithoutAPolicyStore demonstrates a wiring defect in the configuration seed.
//
// Deps documents the schedule store and the policy store as separate optional dependencies, one
// holding sample cron entries so that page shows real cadences and the other holding sample
// governance rules. The schedule seeding is nested inside the policy store's own branch, so an
// install wired with schedules but no policies seeds none of them and the schedules page is empty
// for a reason that has nothing to do with schedules.
func TestSeedSeedsSchedulesWithoutAPolicyStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	deps := stores.deps()
	deps.Policies = nil

	seedConfig(ctx, deps, zap.NewNop())

	schedules, err := stores.Schedules.List(ctx)
	if err != nil {
		t.Fatalf("Schedules.List() error = %v", err)
	}
	if len(schedules) != 3 {
		t.Errorf("seeded %d schedules without a policy store, want the three cadences the page shows",
			len(schedules))
	}
}

// sameActorApprover refuses every decision, standing in for a dispatcher enforcing the
// separation-of-duties rule against a seed that asked the requester to release its own run.
type sameActorApprover struct{}

// Approve refuses.
func (sameActorApprover) Approve(context.Context, string, string, string) (*run.Run, error) {
	return nil, errAppendRefused
}

// TestSeedGovernanceDegradesWithoutFailingTheSeed pins that the governance seed's failure paths are
// warnings rather than errors.
//
// The gate demonstration is the most valuable thing on the demo and the most fragile: it needs a
// submitter, an approver, and a run that reaches a terminal state. If any of that is unavailable the
// rest of the demo still has to seed, because a demo missing one panel beats a demo that would not
// come up at all.
func TestSeedGovernanceDegradesWithoutFailingTheSeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Test 0: a submitter that refuses is logged and seeding continues.
	stores := newSeedStores()
	deps := stores.deps()
	deps.Submitter = &refusingSubmitter{}
	deps.Approver = &releasingApprover{runs: stores.Runs}
	core, logs := observer.New(zapcore.WarnLevel)
	seedGovernance(ctx, deps, "site.yml", "inv.ini", "/srv/infra", zap.New(core))
	if logs.FilterMessageSnippet("seed held run").Len() == 0 {
		t.Error("a refused held run was not reported")
	}

	// Test 1: an approver that refuses the decision is logged, and nothing is left half released.
	stores = newSeedStores()
	deps = stores.deps()
	deps.Submitter = &terminalSubmitter{runs: stores.Runs, audits: stores.Audit}
	deps.Approver = sameActorApprover{}
	core, logs = observer.New(zapcore.WarnLevel)
	seedGovernance(ctx, deps, "site.yml", "inv.ini", "/srv/infra", zap.New(core))
	if logs.FilterMessageSnippet("approve seeded run").Len() == 0 {
		t.Error("a refused approval was not reported")
	}

	// Test 2: with no approver at all the held run is still seeded, which is the documented default.
	stores = newSeedStores()
	deps = stores.deps()
	deps.Submitter = &terminalSubmitter{runs: stores.Runs, audits: stores.Audit}
	deps.Clock = NewSeedClock()
	before := deps.Clock.cursor
	seedGovernance(ctx, deps, "site.yml", "inv.ini", "/srv/infra", zap.NewNop())
	runs, err := stores.Runs.List(ctx)
	if err != nil {
		t.Fatalf("Runs.List() error = %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("seeded %d runs with no approver, want only the held one", len(runs))
	}
	if runs[0].Status != run.StatusPendingApproval {
		t.Errorf("the seeded run is %s, want it held by the gate", runs[0].Status)
	}
	// The held run never settles, so the clock is stepped here to keep the next run's window in order.
	if moved := deps.Clock.cursor.Sub(before); moved < seedRunGap {
		t.Errorf("the clock moved %v for the held run, want at least the between-run gap %v",
			moved, seedRunGap)
	}
}

// TestSeedGovernanceHoldsATerraformDestroyWithoutRunningIt pins what the held run actually is. It
// stands in the runs list as a change the gate is refusing right now, and it must never execute, so
// it needs no terraform on the host and must carry the second-pair-of-eyes requirement the rule it
// cites imposes.
func TestSeedGovernanceHoldsATerraformDestroyWithoutRunningIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stores := newSeedStores()
	deps := stores.deps()
	deps.Submitter = &terminalSubmitter{runs: stores.Runs, audits: stores.Audit}

	seedGovernance(ctx, deps, "site.yml", "inv.ini", "/srv/infra/network", zap.NewNop())

	runs, err := stores.Runs.List(ctx)
	if err != nil {
		t.Fatalf("Runs.List() error = %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("seeded %d runs, want the single held one", len(runs))
	}
	held := runs[0]
	if held.Tool != run.ToolTerraform {
		t.Errorf("the held run uses %q, want terraform, which is what the rule it cites matches", held.Tool)
	}
	if held.Command != "/srv/infra/network" {
		t.Errorf("the held run's working directory = %q, want the seeded terraform root", held.Command)
	}
	if held.HeldByPolicy != "prod terraform destroy" {
		t.Errorf("the held run cites %q, want the seeded terraform rule", held.HeldByPolicy)
	}
	if !held.RequireDistinctApprover {
		t.Error("the held run accepts its own requester as approver, which is not the rule it shows")
	}
	if held.EndedAt != nil {
		t.Error("the held run recorded an end, so the demo shows a gated change that executed anyway")
	}
}

// TestSeedReportsASubmitFailure pins that a seed against a dispatcher that will not accept work
// stops with that error rather than leaving a half populated demo behind and reporting success.
func TestSeedReportsASubmitFailure(t *testing.T) {
	// Not parallel: it materializes the real assets to their fixed path.
	ctx := context.Background()
	stores := newSeedStores()
	deps := stores.deps()
	deps.Submitter = &refusingSubmitter{}

	err := Seed(ctx, deps, zap.NewNop())
	if err == nil {
		t.Fatal("Seed() with a refusing submitter = nil error, want the failure returned")
	}
	if !strings.Contains(err.Error(), "seed run") {
		t.Errorf("Seed() error = %v, want the failing seed step named", err)
	}
}
