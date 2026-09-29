// Package demo seeds a SwitchTender store with lifelike sample data by running real jobs through the
// engine: Ansible playbooks plus Bash, Python, Terraform, and Go. A public read-only instance then shows
// genuine host matrices, split runs, mixed-tool pipelines, and cross-run fleet memory rather than
// fabricated records.
package demo

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	cron "github.com/robfig/cron/v3"
	"go.uber.org/zap"

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

// assets holds the playbook, inventory, and Terraform configuration the seeder runs.
//
//go:embed assets
var assets embed.FS

// Submitter accepts the run shapes the seeder produces. The dispatcher satisfies it.
type Submitter interface {
	// Submit accepts a single run.
	Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error)
	// SubmitSplit accepts a run sharded across the inventory.
	SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error)
	// SubmitPipeline accepts ordered playbook steps as one pipeline.
	SubmitPipeline(ctx context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error)
}

// Approver decides on a run the policy gate is holding. The dispatcher satisfies it.
type Approver interface {
	// Approve releases a held run for execution and records who decided.
	Approve(ctx context.Context, id, by, byType string) (*run.Run, error)
}

// Deps are the stores and submitter the seeder writes through.
type Deps struct {
	// Submitter runs the demo runs through the engine.
	Submitter Submitter
	// Runs reads run state while the seeder waits for terminal runs.
	Runs run.Store
	// Projects, Inventories, Templates, and Credentials hold the browsable sample configuration.
	Projects    project.Store
	Inventories inventory.Store
	Templates   template.Store
	Credentials credential.Store
	// Policies and Users hold sample governance rules and accounts, so those pages show real data.
	Policies policy.Store
	Users    user.Store
	// Approver decides on the held run the governance seed approves. Nil seeds the run the gate is
	// still holding and skips the approved one, so a bare Deps still seeds.
	Approver Approver
	// InvSources holds sample dynamic inventory sources, so that page shows the relationship
	// between a source and the inventory it refreshes.
	InvSources invsource.Store
	// Audit records the sample change history, so the tamper-evident chain has something to
	// verify rather than an empty page.
	Audit audit.Store
	// Schedules holds the sample cron entries, so that page shows real cadences.
	Schedules schedule.Store
	// Clock parks the timestamps of seeded runs in the recent past and steps forward between them, so
	// the run history spreads across the last several hours the way a live fleet's would rather than
	// piling into the seed instant. Nil seeds every run at the real wall clock. It must be the same
	// clock the dispatcher was given through dispatch.WithClock, or the record and its outcome entry
	// disagree on when the run happened.
	Clock *SeedClock
}

// The demo lays its seeded runs across a window of the recent past so the overview's activity chart,
// the runs list, and fleet memory read like a fleet that has been working, not one seeded in a single
// instant. The window opens seedRunWindow ago and closes no later than seedRunMargin before now, and
// the seeder steps the clock seedRunGap forward once each run's record and outcome have landed.
const (
	seedRunWindow = 11 * time.Hour
	seedRunMargin = 15 * time.Minute
	seedRunGap    = 40 * time.Minute
	// seedHistoryBackshiftHours pushes the seeded change history back so all of it predates the run
	// window. The runs commit their outcomes to the same chain after this history is appended, so
	// keeping the history older leaves the chain's times descending with its sequence, the way a live
	// chain reads, rather than a run outcome from hours ago landing beneath a newer config change.
	seedHistoryBackshiftHours = 12
)

// SeedClock is the demo's stand-in for the wall clock. Its cursor opens in the past and advances by
// the real time that elapses between readings, so a run whose tasks took six seconds shows a
// six-second duration, and the cursor is strictly monotonic, so a run's created, claimed, started,
// and ended stamps can never invert however the seeder and the dispatcher's worker goroutines
// interleave their reads. An earlier design advanced a fixed millisecond per read, which collapsed
// every run's span to a couple of milliseconds; a later one read a fixed shift off the real clock,
// which restored real durations but let a between-run step race the workers and stamp a claim hours
// after the run it belonged to had ended. This design keeps both properties: real elapsed durations
// and an order that can never go backward. It never passes its ceiling, so no seeded time is near or
// past now.
type SeedClock struct {
	// mu serializes reads and steps so concurrent shard executions and the seeder never race.
	mu sync.Mutex
	// cursor is the last time handed out; every read advances it by real elapsed time.
	cursor time.Time
	// realAt is the real wall time at the last read, so the next read advances the cursor by however
	// much real time has passed since.
	realAt time.Time
	// ceiling is the latest time the clock will ever return, holding every stamp safely before now.
	ceiling time.Time
}

// NewSeedClock returns a clock whose cursor opens seedRunWindow before now.
func NewSeedClock() *SeedClock {
	now := time.Now()
	return &SeedClock{
		cursor:  now.Add(-seedRunWindow),
		realAt:  now,
		ceiling: now.Add(-seedRunMargin),
	}
}

// Now advances the cursor by the real time elapsed since the last read and returns it, strictly after
// the previous reading and never past the ceiling. It is the function handed to dispatch.WithClock.
func (c *SeedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	real := time.Now()
	step := real.Sub(c.realAt)
	if step < time.Millisecond {
		step = time.Millisecond
	}
	c.realAt = real
	c.cursor = c.cursor.Add(step)
	if c.cursor.After(c.ceiling) {
		c.cursor = c.ceiling
	}
	return c.cursor
}

// advance steps the cursor forward by gap so the next seeded run lands that much later, without
// passing the ceiling. It does not touch realAt, so the next real-elapsed step is unaffected.
func (c *SeedClock) advance(gap time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor = c.cursor.Add(gap)
	if c.cursor.After(c.ceiling) {
		c.cursor = c.ceiling
	}
}

// Seed populates the stores with sample configuration and a set of runs that exercise the matrix,
// splits, mixed-tool pipelines, and cross-run fleet memory. It runs real jobs locally, so it needs
// ansible on the PATH and uses bash, python3, terraform, and go when they are present. A tool whose
// binary is missing is logged and skipped rather than left as a broken failure. It returns when
// every seeded run has finished.
func Seed(ctx context.Context, d Deps, log *zap.Logger) error {
	dir, err := materialize()
	if err != nil {
		return fmt.Errorf("materialize demo assets: %w", err)
	}
	playbook := filepath.Join(dir, "site.yml")
	inv := filepath.Join(dir, "inv.ini")
	// The seeded project's own network root rather than the flat scratch directory beside it: the
	// path lands in the runs list, where "network" reads as a Terraform root somebody would really
	// have and "terraform" beside a TERRAFORM badge says nothing. Both configurations declare only
	// variables, locals, and outputs, so either plans offline with no provider download.
	tfDir := filepath.Join(dir, "repos", "database-ops", "infra", "network")

	seedConfig(ctx, d, log, dir)

	// Plain runs where db01 flaps between failing and passing, so fleet memory marks it flaky.
	failByRun := []string{"", "db01", "", "db01", ""}
	for i, failHost := range failByRun {
		// Alternate origins so the runs list shows the full provenance vocabulary.
		source, sourceID, actor := "schedule", "sch_nightly", "deploy-bot"
		if i%2 == 1 {
			source, sourceID, actor = "template", "tpl_deploy_web", "admin"
		}
		opts := seedOpts(ctx, d, source, sourceID, actor,
			map[string]string{"env": "prod", "team": "platform"}, failVars(failHost)...)
		r, err := d.Submitter.Submit(ctx, playbook, inv, opts...)
		if err != nil {
			return fmt.Errorf("seed run: %w", err)
		}
		settle(ctx, d, r.ID)
	}

	// A split where one shard fails, showing the merged matrix and failed-shard isolation.
	split, err := d.Submitter.SubmitSplit(ctx, playbook, inv, 3,
		seedOpts(ctx, d, "template", "tpl_deploy_web", "admin",
			map[string]string{"env": "prod", "ticket": "OPS-482"}, failVars("db01")...)...)
	if err != nil {
		return fmt.Errorf("seed split: %w", err)
	}
	settle(ctx, d, split.ID)

	// A clean three-step Ansible pipeline.
	steps := []run.PipelineStep{
		{Name: "prepare", Playbook: playbook},
		{Name: "migrate", Playbook: playbook},
		{Name: "verify", Playbook: playbook},
	}
	pipe, err := d.Submitter.SubmitPipeline(ctx, "Release 4.2", inv, steps,
		seedOpts(ctx, d, "api", "", "deploy-bot", map[string]string{"env": "prod", "ticket": "REL-42"})...)
	if err != nil {
		return fmt.Errorf("seed pipeline: %w", err)
	}
	settle(ctx, d, pipe.ID)

	// One more failure on a different host for variety.
	last, err := d.Submitter.Submit(ctx, playbook, inv,
		seedOpts(ctx, d, "rerun", "", "admin",
			map[string]string{"env": "staging"}, failVars("edge01")...)...)
	if err != nil {
		return fmt.Errorf("seed run: %w", err)
	}
	settle(ctx, d, last.ID)

	// One fact-gathering play, so every host page shows its distribution, kernel, and the rest
	// rather than an empty panel.
	factsPlay := filepath.Join(dir, "facts.yml")
	factsOpts := seedOpts(ctx, d, "schedule", "sch_facts", "deploy-bot", map[string]string{"env": "prod"})
	if factsRun, err := d.Submitter.Submit(ctx, factsPlay, inv, factsOpts...); err == nil {
		settle(ctx, d, factsRun.ID)
	} else {
		log.Warn("demo: seed fact gather: " + err.Error())
	}

	// A dry run of a check playbook surfaces configuration drift per host on the Drift page.
	driftPlay := filepath.Join(dir, "drift.yml")
	driftOpts := seedOpts(ctx, d, "schedule", "sch_drift_check", "deploy-bot",
		map[string]string{"env": "prod"}, run.WithDryRun(true))
	if driftRun, err := d.Submitter.Submit(ctx, driftPlay, inv, driftOpts...); err == nil {
		settle(ctx, d, driftRun.ID)
	} else {
		log.Warn("demo: seed drift check: " + err.Error())
	}

	if err := seedMultiTool(ctx, d, tfDir, playbook, inv, log); err != nil {
		return err
	}

	seedGovernance(ctx, d, playbook, inv, tfDir, log)

	normalizeClaimStamps(ctx, d, log)

	log.Info("demo: seeded sample projects, templates, inventories, and runs")
	return nil
}

// seedGovernance seeds the two runs that show the policy gate doing its job: one a rule is still
// holding, and one that was held, decided on by a second person, and only then executed.
//
// Every other seeded run goes straight from submit to execution, so the demo showed the engine and it
// showed the evidence but never the gate between them. A visitor reading that this is the boundary
// every change comes through found the rules listed as configuration and not one run any of them had
// ever stopped, which left the product's central claim as the one thing the demo could not show.
func seedGovernance(ctx context.Context, d Deps, playbook, inv, tfDir string, log *zap.Logger) {
	// A production destroy, held and left that way, so the runs list always has a change the gate is
	// refusing right now. It never executes, so it needs no terraform on the host.
	held := seedOpts(ctx, d, "api", "", "deploy-bot",
		map[string]string{"env": "prod", "ticket": "OPS-511"},
		run.WithTool(run.ToolTerraform), run.WithCommand(tfDir),
		run.WithRequireApproval(true), run.WithRequireDistinctApprover(true),
		run.WithHeldByPolicy("prod terraform destroy"))
	if _, err := d.Submitter.Submit(ctx, "", "", held...); err != nil {
		log.Warn("demo: seed held run: " + err.Error())
	} else if d.Clock != nil {
		// There is nothing to settle: the run is held and reaches no terminal state, so the clock is
		// stepped here instead of by settle, keeping the next run's window in order.
		d.Clock.advance(seedRunGap)
	}

	if d.Approver == nil {
		return
	}
	// The same gate carried all the way through: held by the production rule, released by somebody
	// other than the person who asked for it, then executed. This is the run whose evidence carries a
	// decision and the digest of the exact spec that decision released.
	opts := seedOpts(ctx, d, "template", "tpl_deploy_web", "deploy-bot",
		map[string]string{"env": "prod", "ticket": "OPS-512"},
		run.WithRequireApproval(true), run.WithRequireDistinctApprover(true),
		run.WithHeldByPolicy("any production run"))
	r, err := d.Submitter.Submit(ctx, playbook, inv, opts...)
	if err != nil {
		log.Warn("demo: seed approved run: " + err.Error())
		return
	}
	if _, err := d.Approver.Approve(ctx, r.ID, "admin", "user"); err != nil {
		log.Warn("demo: approve seeded run: " + err.Error())
		return
	}
	settle(ctx, d, r.ID)
}

// normalizeClaimStamps pulls each seeded run's claimed_at back into its own window. The dispatcher's
// worker claims a run through the store, and the store stamps claimed_at from the real wall clock, not
// the demo's seed clock, so a run whose record sits hours in the past was otherwise marked claimed at
// the seed instant, moments before now. That left the run timeline reading created and ended hours ago
// but claimed just now, a contradiction against the audit trail that a careful reader would catch. Here
// the claim is reseated between the run's creation and its start, where a real claim falls, so the
// timeline reads in order. It runs only for the historical seed: without the seed clock the store's
// claim stamp already matches the rest of the run, so there is nothing to correct.
func normalizeClaimStamps(ctx context.Context, d Deps, log *zap.Logger) {
	if d.Clock == nil || d.Runs == nil {
		return
	}
	tops, err := d.Runs.List(ctx)
	if err != nil {
		return
	}
	// List returns only the top-level runs, so a split's shards and a pipeline's steps, which carry
	// their own claim stamps and show their own timelines, are reached through the parent.
	for _, r := range tops {
		normalizeOneClaim(ctx, d, r, log)
		shards, _ := d.Runs.Shards(ctx, r.ID)
		for _, s := range shards {
			normalizeOneClaim(ctx, d, s, log)
		}
		steps, _ := d.Runs.Steps(ctx, r.ID)
		for _, s := range steps {
			normalizeOneClaim(ctx, d, s, log)
		}
	}
}

// normalizeOneClaim reseats a single run's claim stamp between its creation and its start, where a real
// claim falls, and persists it. A run never claimed, or already claimed in order, is left untouched.
func normalizeOneClaim(ctx context.Context, d Deps, r *run.Run, log *zap.Logger) {
	if r.ClaimedAt == nil {
		return
	}
	target := r.CreatedAt
	if r.StartedAt != nil && r.StartedAt.After(r.CreatedAt) {
		// Sit the claim midway between creation and start, so created, claimed, started reads as
		// three ordered instants rather than collapsing the claim onto the start.
		target = r.CreatedAt.Add(r.StartedAt.Sub(r.CreatedAt) / 2)
	}
	if !r.ClaimedAt.After(target) {
		return
	}
	r.ClaimedAt = &target
	// A failed save leaves the run's claim at the real wall clock while its other stamps sit in the
	// seed window, the exact contradiction this pass removes, so surface it rather than swallow it.
	if err := d.Runs.Save(ctx, r); err != nil && log != nil {
		log.Warn("demo: normalize claim stamp: save failed: " + err.Error())
	}
}

// seedMultiTool runs one Bash, Python, Terraform, and Go job plus a mixed-tool pipeline, so the demo
// shows the engine driving every tool rather than Ansible alone. Bash always runs. Python, Terraform,
// and Go run only when their binary is present, so a missing tool is skipped rather than left as
// an exec-not-found failure. The mixed pipeline provisions with whichever infra tool is available,
// then configures with Ansible and verifies with Bash, so it is always a real multi-tool graph that
// finishes cleanly on whatever host serves the demo.
func seedMultiTool(ctx context.Context, d Deps, tfDir, playbook, inv string, log *zap.Logger) error {
	// The tools this host cannot run, so the end of seeding can say what the demo will not show
	// rather than leaving three skips in the log for somebody to notice.
	var missing []string
	bash, err := d.Submitter.Submit(ctx, "", "",
		seedOpts(ctx, d, "schedule", "sch_log_rotate", "deploy-bot", map[string]string{"env": "prod"},
			run.WithTool(run.ToolBash), run.WithCommand(scriptLogRotate))...)
	if err != nil {
		return fmt.Errorf("seed bash run: %w", err)
	}
	settle(ctx, d, bash.ID)

	if have("python3") {
		py, err := d.Submitter.Submit(ctx, "", "",
			seedOpts(ctx, d, "template", "tpl_reconcile", "admin", map[string]string{"env": "prod"},
				run.WithTool(run.ToolPython), run.WithCommand(scriptReconcile))...)
		if err != nil {
			return fmt.Errorf("seed python run: %w", err)
		}
		settle(ctx, d, py.ID)
	} else {
		log.Info("demo: python3 not on PATH, skipping the python run")
	}

	if have("terraform") {
		tf, err := d.Submitter.Submit(ctx, "", "",
			seedOpts(ctx, d, "api", "", "deploy-bot", map[string]string{"env": "staging"},
				run.WithTool(run.ToolTerraform), run.WithCommand(tfDir), run.WithDryRun(true))...)
		if err != nil {
			return fmt.Errorf("seed terraform run: %w", err)
		}
		settle(ctx, d, tf.ID)
	} else {
		missing = append(missing, "terraform")
	}

	if have("go") {
		gorun, err := d.Submitter.Submit(ctx, "", "",
			seedOpts(ctx, d, "template", "tpl_capacity", "admin", map[string]string{"env": "prod"},
				run.WithTool(run.ToolGo), run.WithCommand(scriptFleetGo))...)
		if err != nil {
			return fmt.Errorf("seed go run: %w", err)
		}
		settle(ctx, d, gorun.ID)
	} else {
		missing = append(missing, "go")
	}

	steps := []run.PipelineStep{
		infraStep(tfDir),
		{Name: "configure", Tool: run.ToolAnsible, Playbook: playbook},
		{Name: "smoke-test", Tool: run.ToolBash, Command: scriptSmoke},
	}
	pipe, err := d.Submitter.SubmitPipeline(ctx, "Provision and deploy", inv, steps,
		seedOpts(ctx, d, "template", "tpl_provision", "admin",
			map[string]string{"env": "prod", "ticket": "OPS-503"})...)
	if err != nil {
		return fmt.Errorf("seed mixed pipeline: %w", err)
	}
	settle(ctx, d, pipe.ID)

	// A demo that shows three of the five tools this product advertises is a weak first impression,
	// and the three skips that produce it were three separate lines nobody reads. Said once, as a
	// warning, naming what a visitor will not see and how to fix it.
	if len(missing) > 0 {
		it, them := "it", "it"
		if len(missing) > 1 {
			it, them = "them", "them"
		}
		log.Warn("demo: seeded without "+strings.Join(missing, " and ")+
			", so a visitor sees no run for "+englishList(missing)+
			" even though this product advertises "+it+": install "+them+" on this host and reseed",
			zap.Strings("missing", missing))
	}
	return nil
}

// englishList joins names the way a sentence does, so a warning reads as prose rather than as a
// slice printed into the middle of one.
func englishList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

// infraStep returns the mixed pipeline's first step, choosing the best available infrastructure tool.
// Terraform is preferred, then Python, then Bash, so the pipeline mixes tools and runs to completion
// on any host: a Terraform machine gets a genuine plan, a machine without it still shows a distinct
// provisioning tool ahead of the Ansible and Bash steps.
// infraStep is the pipeline's provisioning step. It prefers Terraform and falls back, so a demo on a
// host without it still shows a pipeline rather than failing to seed. The substitution is reported
// by the caller, since a visitor is told this product runs Terraform and would be looking at Bash.
func infraStep(tfDir string) run.PipelineStep {
	switch {
	case have("terraform"):
		return run.PipelineStep{Name: "provision", Tool: run.ToolTerraform, Command: tfDir, DryRun: true}
	case have("python3"):
		return run.PipelineStep{Name: "provision", Tool: run.ToolPython, Command: scriptProvisionPy}
	default:
		return run.PipelineStep{Name: "provision", Tool: run.ToolBash, Command: scriptProvisionSh}
	}
}

// have reports whether the named executable resolves on PATH, so the seeder can skip a tool run when
// its binary is absent instead of leaving a broken-looking failed run in the demo.
func have(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

// failVars returns the submit options that make the run fail on one host, or none for a clean run.
func failVars(host string) []run.SubmitOption {
	if host == "" {
		return nil
	}
	return []run.SubmitOption{run.WithExtraVars(map[string]any{"fail_host": host})}
}

// seedOpts adds the provenance and labels a real deployment records, so the demo shows the origin
// column, the actor, and label filtering with believable data rather than blanks.
func seedOpts(ctx context.Context, d Deps, source, sourceID, actor string, labels map[string]string,
	extra ...run.SubmitOption) []run.SubmitOption {
	opts := []run.SubmitOption{
		run.WithSource(source, sourceID),
		run.WithActor(actor),
		run.WithLabels(labels),
	}
	// Every seeded run gets the creation entry an API-created run gets, and carries its receipt, so
	// the receipt button works on the demo. Without it every run answered "no creation receipt, so
	// its start cannot be placed on the chain", which is the flagship feature failing on the one
	// install strangers try first.
	if d.Audit != nil {
		entry := &audit.Entry{
			ID: audit.NewID(), At: seedTime(d), Actor: actor, Method: "POST", Path: "/v1/runs",
		}
		if err := d.Audit.Append(ctx, entry); err == nil {
			opts = append(opts, run.WithAuditReceiptOf(audit.Receipt(entry)))
		}
	}
	return append(opts, extra...)
}

// seedTime reads the seed clock when one is set, so a creation entry lands just before the run it
// creates, and the wall clock otherwise.
func seedTime(d Deps) time.Time {
	if d.Clock != nil {
		return d.Clock.Now()
	}
	return time.Now()
}

// settle waits for a seeded run to finish, then for its outcome to reach the chain, and only then
// steps the demo clock forward for the next run. Waiting for the outcome entry before stepping is what
// keeps the entry's time in the same window as the run's record: the outcome is committed just after
// the terminal save, so a clock stepped the instant the run went terminal would stamp the entry in the
// next run's window and break the reconciliation the historical seeding exists to show. It is a no-op
// step when no clock or chain is configured, so the seed still runs against a bare Deps.
func settle(ctx context.Context, d Deps, id string) {
	waitTerminal(ctx, d.Runs, id)
	if d.Audit != nil {
		waitOutcomeCommitted(ctx, d.Audit, id)
	}
	if d.Clock != nil {
		d.Clock.advance(seedRunGap)
	}
}

// waitTerminal polls until the run reaches a terminal state or a timeout elapses.
func waitTerminal(ctx context.Context, store run.Store, id string) {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		r, err := store.Get(ctx, id)
		if err == nil && r.Status.Terminal() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// waitOutcomeCommitted polls the newest chain entries until the run's outcome lands or a short deadline
// passes. The outcome is committed just after the terminal save, so it is among the newest entries. A
// child of a split or pipeline rolls its outcome into its parent and commits none, so the bounded wait
// keeps a run that never commits its own entry from stalling the seed.
func waitOutcomeCommitted(ctx context.Context, audits audit.Store, id string) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		recent, err := audits.List(ctx, 64)
		if err == nil {
			for _, e := range recent {
				if e.Method == audit.MethodRun && strings.Contains(e.Path, id) {
					return
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// materialize writes the embedded assets to a temp directory, recreating their tree, and returns its
// path. The tree carries the Ansible playbook and inventory plus the Terraform working directory.
func materialize() (string, error) {
	// A fixed name rather than a random suffix: the path lands in run records, where
	// switchtender-demo-assets reads as intentional and a scratch dir reads as debris.
	dir := filepath.Join(os.TempDir(), "switchtender-demo-assets")
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	err := fs.WalkDir(assets, "assets", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel("assets", path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		return "", err
	}
	if err := initDemoRepos(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// initDemoRepos turns each materialized repos/ subtree into a real git repository with one commit
// on main, so the seeded projects clone from disk and a launched template gets a green run instead
// of a git failure against a host that does not exist.
func initDemoRepos(dir string) error {
	entries, err := os.ReadDir(filepath.Join(dir, "repos"))
	if err != nil {
		return fmt.Errorf("read demo repos: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, "repos", entry.Name())
		repo, err := git.PlainInitWithOptions(path, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
		})
		if err != nil {
			return fmt.Errorf("init demo repo %s: %w", entry.Name(), err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("init demo repo %s: %w", entry.Name(), err)
		}
		if err := wt.AddGlob("."); err != nil {
			return fmt.Errorf("init demo repo %s: %w", entry.Name(), err)
		}
		if _, err := wt.Commit("Seed the demo repository", &git.CommitOptions{
			Author: &object.Signature{
				Name: "SwitchTender Demo", Email: "demo@switchtender.invalid", When: time.Now(),
			},
		}); err != nil {
			return fmt.Errorf("init demo repo %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// seedConfig stores browsable sample projects, inventories, credentials, and templates. The templates
// cover the main tools the engine drives, so the Templates list shows Ansible, Bash, Terraform, Python,
// and Go presets even on a host that lacks a given tool's binary. It is best effort: a store error is
// logged and skipped so the runs still seed.
func seedConfig(ctx context.Context, d Deps, log *zap.Logger, assetDir string) {
	now := time.Now()
	ago := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }

	// The repositories are real local clones materialized beside the other assets, so launching a
	// project-backed template on a writable instance syncs and runs green instead of failing
	// against a host that does not exist.
	projects := []*project.Project{
		{ID: project.NewID(), Name: "web-platform",
			RepoURL: "file://" + filepath.Join(assetDir, "repos", "web-platform"),
			Branch:  "main", CreatedAt: ago(72)},
		{ID: project.NewID(), Name: "database-ops",
			RepoURL: "file://" + filepath.Join(assetDir, "repos", "database-ops"),
			Branch:  "main", CreatedAt: ago(48)},
	}
	for _, p := range projects {
		if err := d.Projects.Save(ctx, p); err != nil {
			log.Warn("demo: seed project: " + err.Error())
		}
	}

	invContent, _ := assets.ReadFile("assets/inv.ini")
	inventories := []*inventory.Inventory{
		{ID: inventory.NewID(), Name: "production", Content: string(invContent), CreatedAt: ago(72)},
		{ID: inventory.NewID(), Name: "staging", Content: "[all]\nstage01 ansible_connection=local\n", CreatedAt: ago(48)},
	}
	for _, inv := range inventories {
		if err := d.Inventories.Save(ctx, inv); err != nil {
			log.Warn("demo: seed inventory: " + err.Error())
		}
	}

	// A dynamic source paired with the inventory it maintains, so the Sources page shows a real
	// relationship rather than an empty list. The script is never executed by seeding; it stands
	// as the configuration a refresh would run.
	if d.InvSources != nil {
		dynamic := &inventory.Inventory{
			ID: inventory.NewID(), Name: "cloud-discovered",
			Content:   "[all]\n# Refreshed from the cloud-hosts source.\n",
			CreatedAt: ago(20),
		}
		if err := d.Inventories.Save(ctx, dynamic); err != nil {
			log.Warn("demo: seed dynamic inventory: " + err.Error())
		} else {
			synced := ago(1)
			src := &invsource.Source{
				ID: invsource.NewID(), Name: "cloud-hosts",
				Source: "inventory/aws_ec2.yml", InventoryID: dynamic.ID,
				UpdateOnLaunch: true, SyncIntervalSeconds: 3600,
				SyncedAt: &synced, CreatedAt: ago(20),
			}
			if err := d.InvSources.Save(ctx, src); err != nil {
				log.Warn("demo: seed inventory source: " + err.Error())
			}
		}
	}

	// A change history, so the audit page shows a real hash chain the Verify button can check.
	// Each append links to the one before it exactly as a live mutation would.
	if d.Audit != nil {
		history := []struct {
			actor, method, path string
			hoursAgo            int
		}{
			{"admin", "POST", "/v1/projects", 72},
			{"admin", "POST", "/v1/inventories", 72},
			{"admin", "POST", "/v1/credentials", 72},
			{"admin", "POST", "/v1/templates", 72},
			{"deploy-bot", "PUT", "/v1/templates/tpl_deploy_web", 50},
			{"admin", "POST", "/v1/inventory-sources", 20},
			{"deploy-bot", "POST", "/v1/pipelines", 6},
			{"deploy-bot", "POST", "/v1/policies", 5},
			{"deploy-bot", "POST", "/v1/schedules", 4},
			{"deploy-bot", "POST", "/v1/runs", 3},
			{"admin", "POST", "/v1/runs/run_split_demo/approve", 3},
			{"admin", "DELETE", "/v1/templates/tpl_retired", 2},
			{"deploy-bot", "POST", "/v1/runs", 1},
		}
		for _, h := range history {
			entry := &audit.Entry{
				ID: audit.NewID(), At: ago(h.hoursAgo + seedHistoryBackshiftHours),
				Actor: h.actor, Method: h.method, Path: h.path,
			}
			if err := d.Audit.Append(ctx, entry); err != nil {
				log.Warn("demo: seed audit entry: " + err.Error())
				break
			}
		}
	}

	// Each carries placeholder sealed material: the doctor flags a credential with no secret, and
	// a demo full of warnings reads as misconfigured. Nothing seeded references these, and the
	// instance is read-only, so the placeholder is never decrypted or injected.
	creds := []*credential.Credential{
		{ID: credential.NewID(), Name: "prod-ssh", Kind: credential.KindSSHKey, Secret: "demo-placeholder", CreatedAt: ago(72)},
		{ID: credential.NewID(), Name: "ansible-vault", Kind: credential.KindVaultPassword, VaultID: "prod", Secret: "demo-placeholder", CreatedAt: ago(72)},
		{ID: credential.NewID(), Name: "dockerhub", Kind: credential.KindRegistry, Secret: "demo-placeholder", CreatedAt: ago(48)},
		{ID: credential.NewID(), Name: "openstack-prod", Kind: credential.KindOpenStack, Secret: "demo-placeholder", CreatedAt: ago(36)},
	}
	for _, c := range creds {
		if err := d.Credentials.Save(ctx, c); err != nil {
			log.Warn("demo: seed credential: " + err.Error())
		}
	}

	templates := []*template.Template{
		{ID: template.NewID(), Name: "Deploy web", ProjectID: projects[0].ID, Playbook: "site.yml", InventoryID: inventories[0].ID, Shards: 3, CreatedAt: ago(72)},
		{ID: template.NewID(), Name: "Migrate database", ProjectID: projects[1].ID, Playbook: "migrate.yml", InventoryID: inventories[0].ID, CreatedAt: ago(48)},
		{ID: template.NewID(), Name: "Nightly audit", ProjectID: projects[0].ID, Playbook: "audit.yml", InventoryID: inventories[0].ID, CreatedAt: ago(24)},
		{ID: template.NewID(), Name: "Rotate logs", ProjectID: projects[0].ID, Tool: run.ToolBash, Command: scriptLogRotate, CreatedAt: ago(36)},
		{ID: template.NewID(), Name: "Provision network", ProjectID: projects[1].ID, Tool: run.ToolTerraform, Command: "infra/network", DryRun: true, CreatedAt: ago(30)},
		{ID: template.NewID(), Name: "Reconcile inventory", ProjectID: projects[0].ID, Tool: run.ToolPython, Command: scriptReconcile, CreatedAt: ago(18)},
		{ID: template.NewID(), Name: "Fleet capacity report", ProjectID: projects[0].ID, Tool: run.ToolGo, Command: scriptFleetGo, CreatedAt: ago(12)},
	}
	for _, t := range templates {
		if err := d.Templates.Save(ctx, t); err != nil {
			log.Warn("demo: seed template: " + err.Error())
		}
	}

	if d.Policies != nil {
		tfDestroy := policy.NewPolicy("prod terraform destroy")
		tfDestroy.Tool, tfDestroy.CommandContains, tfDestroy.ExcludeDryRun, tfDestroy.CreatedAt =
			run.ToolTerraform, "destroy", true, ago(40)
		anyProd := policy.NewPolicy("any production run")
		anyProd.InventoryID, anyProd.CreatedAt = inventories[0].ID, ago(22)
		// Cron entries against the seeded templates, so the schedules page shows real cadences and
		// the plain-language reading of each expression.
		if d.Schedules != nil && len(templates) >= 3 {
			// The next run is the actual next fire of each cron from now, so the time on the page
			// matches the cadence beside it. Seeding it as a fixed offset showed "daily at 2am" firing
			// next at 3:18, which on an audit-and-scheduling product reads as the schedule being wrong.
			next := func(expr string) *time.Time {
				parsed, err := cron.ParseStandard(expr)
				if err != nil {
					return nil
				}
				t := parsed.Next(now)
				return &t
			}
			schedules := []*schedule.Schedule{
				{
					ID: schedule.NewID(), Name: "Nightly audit", Cron: "0 2 * * *",
					TemplateID: templates[2].ID, Enabled: true,
					NextRunAt: next("0 2 * * *"), CreatedAt: ago(70),
				},
				{
					ID: schedule.NewID(), Name: "Weekday deploy window", Cron: "30 9 * * 1-5",
					TemplateID: templates[0].ID, Enabled: true,
					NextRunAt: next("30 9 * * 1-5"), CreatedAt: ago(46),
				},
				{
					ID: schedule.NewID(), Name: "Hourly drift check", Cron: "0 * * * *",
					TemplateID: templates[1].ID, Enabled: false,
					CreatedAt: ago(20),
				},
			}
			for _, sc := range schedules {
				if err := d.Schedules.Save(ctx, sc); err != nil {
					log.Warn("demo: seed schedule: " + err.Error())
				}
			}
		}

		policies := []*policy.Policy{tfDestroy, anyProd}
		for _, p := range policies {
			if err := d.Policies.Save(ctx, p); err != nil {
				log.Warn("demo: seed policy: " + err.Error())
			}
		}
	}

	if d.Users != nil {
		accounts := []struct {
			Name string
			Role user.Role
		}{
			{"admin", user.RoleAdmin},
			{"deploy-bot", user.RoleOperator},
			{"auditor", user.RoleViewer},
		}
		for _, a := range accounts {
			u, err := user.New(a.Name, "demo-password", a.Role)
			if err != nil {
				log.Warn("demo: build user: " + err.Error())
				continue
			}
			if err := d.Users.Save(ctx, u); err != nil {
				log.Warn("demo: seed user: " + err.Error())
			}
		}
	}
}

// scriptLogRotate is the Bash job for the standalone bash run and the Rotate logs template. It runs
// cleanly under set -e on any host, printing lifelike operations output.
const scriptLogRotate = `# Rotate and archive application logs on every web host
set -euo pipefail
echo "Rotating application logs on $(hostname)"
for svc in web api worker scheduler; do
  echo "  archived and truncated ${svc}.log"
done
echo "Disk after rotation:"
df -h / | tail -1
echo "Log rotation complete"
`

// scriptSmoke is the Bash step that verifies the mixed pipeline after the Ansible configure step.
const scriptSmoke = `# Smoke test the deployed release before traffic is shifted
set -euo pipefail
echo "Running post-deploy smoke checks"
for ep in / /healthz /metrics; do
  echo "  GET ${ep} -> 200 OK"
done
echo "All endpoints healthy"
`

// scriptReconcile is the Python job for the standalone python run and the Reconcile inventory
// template. It reports inventory drift as JSON and exits zero.
const scriptReconcile = `# Reconcile the inventory against the hosts that actually answered
import json

want = {"web01", "web02", "web03", "db01", "db02", "edge01"}
present = {"web01", "web02", "web03", "db01", "edge01"}
missing = sorted(want - present)
report = {"expected": len(want), "present": len(present), "missing": missing}
print("Inventory reconciliation")
print(json.dumps(report, indent=2))
if missing:
    print(f"Drift detected: {', '.join(missing)} not reporting")
`

// scriptFleetGo is the Go job for the standalone go run and the Fleet capacity report template. It
// summarizes host capacity as JSON and exits zero, using only the standard library so it runs offline.
const scriptFleetGo = `// Report spare capacity per host from the fleet summary
package main

import (
	"encoding/json"
	"fmt"
)

type host struct {
	Name string ` + "`json:\"name\"`" + `
	CPU  int    ` + "`json:\"cpu_pct\"`" + `
	Mem  int    ` + "`json:\"mem_pct\"`" + `
}

func main() {
	hosts := []host{{"web01", 34, 51}, {"web02", 41, 48}, {"db01", 72, 80}}
	fmt.Println("Fleet capacity report")
	hot := 0
	for _, h := range hosts {
		status := "ok"
		if h.CPU > 70 || h.Mem > 75 {
			status = "hot"
			hot++
		}
		fmt.Printf("  %-6s cpu %3d%%  mem %3d%%  %s\n", h.Name, h.CPU, h.Mem, status)
	}
	summary, _ := json.Marshal(map[string]int{"hosts": len(hosts), "hot": hot})
	fmt.Println(string(summary))
	fmt.Printf("%d host(s) above threshold\n", hot)
}
`

// scriptProvisionPy is the mixed pipeline's provisioning step when Terraform is absent but Python is
// present, so the pipeline still leads with a distinct infrastructure tool.
const scriptProvisionPy = `# Provision the network plan and write it out for the next step
import json

plan = {
    "network": "10.0.0.0/16",
    "subnets": ["10.0.1.0/24", "10.0.2.0/24"],
    "web_hosts": ["web01", "web02", "web03"],
}
print("Planning infrastructure")
print(json.dumps(plan, indent=2))
print(f"Would create {len(plan['subnets'])} subnets for {len(plan['web_hosts'])} hosts")
`

// scriptProvisionSh is the mixed pipeline's provisioning step when neither Terraform nor Python is
// present, keeping the pipeline runnable on a bare host.
const scriptProvisionSh = `# Provision the host and hand its address to the next step
set -euo pipefail
echo "Planning infrastructure"
echo "  network: 10.0.0.0/16"
echo "  subnets: 10.0.1.0/24, 10.0.2.0/24"
echo "Provision plan ready"
`
