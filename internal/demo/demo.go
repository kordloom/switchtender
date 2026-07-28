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
	"time"

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
	// InvSources holds sample dynamic inventory sources, so that page shows the relationship
	// between a source and the inventory it refreshes.
	InvSources invsource.Store
	// Audit records the sample change history, so the tamper-evident chain has something to
	// verify rather than an empty page.
	Audit audit.Store
	// Schedules holds the sample cron entries, so that page shows real cadences.
	Schedules schedule.Store
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
	tfDir := filepath.Join(dir, "terraform")

	seedConfig(ctx, d, log)

	// Plain runs where db01 flaps between failing and passing, so fleet memory marks it flaky.
	failByRun := []string{"", "db01", "", "db01", ""}
	for i, failHost := range failByRun {
		// Alternate origins so the runs list shows the full provenance vocabulary.
		source, sourceID, actor := "schedule", "sch_nightly", "deploy-bot"
		if i%2 == 1 {
			source, sourceID, actor = "template", "tpl_deploy_web", "admin"
		}
		opts := seedOpts(source, sourceID, actor,
			map[string]string{"env": "prod", "team": "platform"}, failVars(failHost)...)
		r, err := d.Submitter.Submit(ctx, playbook, inv, opts...)
		if err != nil {
			return fmt.Errorf("seed run: %w", err)
		}
		waitTerminal(ctx, d.Runs, r.ID)
	}

	// A split where one shard fails, showing the merged matrix and failed-shard isolation.
	split, err := d.Submitter.SubmitSplit(ctx, playbook, inv, 3,
		seedOpts("template", "tpl_deploy_web", "admin",
			map[string]string{"env": "prod", "ticket": "OPS-482"}, failVars("db01")...)...)
	if err != nil {
		return fmt.Errorf("seed split: %w", err)
	}
	waitTerminal(ctx, d.Runs, split.ID)

	// A clean three-step Ansible pipeline.
	steps := []run.PipelineStep{
		{Name: "prepare", Playbook: playbook},
		{Name: "migrate", Playbook: playbook},
		{Name: "verify", Playbook: playbook},
	}
	pipe, err := d.Submitter.SubmitPipeline(ctx, "Release 4.2", inv, steps,
		seedOpts("api", "", "deploy-bot", map[string]string{"env": "prod", "ticket": "REL-42"})...)
	if err != nil {
		return fmt.Errorf("seed pipeline: %w", err)
	}
	waitTerminal(ctx, d.Runs, pipe.ID)

	// One more failure on a different host for variety.
	last, err := d.Submitter.Submit(ctx, playbook, inv,
		seedOpts("rerun", "", "admin",
			map[string]string{"env": "staging"}, failVars("edge01")...)...)
	if err != nil {
		return fmt.Errorf("seed run: %w", err)
	}
	waitTerminal(ctx, d.Runs, last.ID)

	// A dry run of a check playbook surfaces configuration drift per host on the Drift page.
	driftPlay := filepath.Join(dir, "drift.yml")
	driftOpts := seedOpts("schedule", "sch_drift_check", "deploy-bot",
		map[string]string{"env": "prod"}, run.WithDryRun(true))
	if driftRun, err := d.Submitter.Submit(ctx, driftPlay, inv, driftOpts...); err == nil {
		waitTerminal(ctx, d.Runs, driftRun.ID)
	} else {
		log.Warn("demo: seed drift check: " + err.Error())
	}

	if err := seedMultiTool(ctx, d, tfDir, playbook, inv, log); err != nil {
		return err
	}

	log.Info("demo: seeded sample projects, templates, inventories, and runs")
	return nil
}

// seedMultiTool runs one Bash, Python, Terraform, and Go job plus a mixed-tool pipeline, so the demo
// shows the engine driving every tool rather than Ansible alone. Bash always runs. Python, Terraform,
// and Go run only when their binary is present, so a missing tool is skipped rather than left as
// an exec-not-found failure. The mixed pipeline provisions with whichever infra tool is available,
// then configures with Ansible and verifies with Bash, so it is always a real multi-tool graph that
// finishes cleanly on whatever host serves the demo.
func seedMultiTool(ctx context.Context, d Deps, tfDir, playbook, inv string, log *zap.Logger) error {
	bash, err := d.Submitter.Submit(ctx, "", "",
		seedOpts("schedule", "sch_log_rotate", "deploy-bot", map[string]string{"env": "prod"},
			run.WithTool(run.ToolBash), run.WithCommand(scriptLogRotate))...)
	if err != nil {
		return fmt.Errorf("seed bash run: %w", err)
	}
	waitTerminal(ctx, d.Runs, bash.ID)

	if have("python3") {
		py, err := d.Submitter.Submit(ctx, "", "",
			seedOpts("template", "tpl_reconcile", "admin", map[string]string{"env": "prod"},
				run.WithTool(run.ToolPython), run.WithCommand(scriptReconcile))...)
		if err != nil {
			return fmt.Errorf("seed python run: %w", err)
		}
		waitTerminal(ctx, d.Runs, py.ID)
	} else {
		log.Info("demo: python3 not on PATH, skipping the python run")
	}

	if have("terraform") {
		tf, err := d.Submitter.Submit(ctx, "", "",
			seedOpts("api", "", "deploy-bot", map[string]string{"env": "staging"},
				run.WithTool(run.ToolTerraform), run.WithCommand(tfDir), run.WithDryRun(true))...)
		if err != nil {
			return fmt.Errorf("seed terraform run: %w", err)
		}
		waitTerminal(ctx, d.Runs, tf.ID)
	} else {
		log.Info("demo: terraform not on PATH, skipping the terraform run; install terraform to include it")
	}

	if have("go") {
		gorun, err := d.Submitter.Submit(ctx, "", "",
			seedOpts("template", "tpl_capacity", "admin", map[string]string{"env": "prod"},
				run.WithTool(run.ToolGo), run.WithCommand(scriptFleetGo))...)
		if err != nil {
			return fmt.Errorf("seed go run: %w", err)
		}
		waitTerminal(ctx, d.Runs, gorun.ID)
	} else {
		log.Info("demo: go not on PATH, skipping the go run")
	}

	steps := []run.PipelineStep{
		infraStep(tfDir),
		{Name: "configure", Tool: run.ToolAnsible, Playbook: playbook},
		{Name: "smoke-test", Tool: run.ToolBash, Command: scriptSmoke},
	}
	pipe, err := d.Submitter.SubmitPipeline(ctx, "Provision and deploy", inv, steps,
		seedOpts("template", "tpl_provision", "admin",
			map[string]string{"env": "prod", "ticket": "OPS-503"})...)
	if err != nil {
		return fmt.Errorf("seed mixed pipeline: %w", err)
	}
	waitTerminal(ctx, d.Runs, pipe.ID)
	return nil
}

// infraStep returns the mixed pipeline's first step, choosing the best available infrastructure tool.
// Terraform is preferred, then Python, then Bash, so the pipeline mixes tools and runs to completion
// on any host: a Terraform machine gets a genuine plan, a machine without it still shows a distinct
// provisioning tool ahead of the Ansible and Bash steps.
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
func seedOpts(source, sourceID, actor string, labels map[string]string, extra ...run.SubmitOption) []run.SubmitOption {
	opts := []run.SubmitOption{
		run.WithSource(source, sourceID),
		run.WithActor(actor),
		run.WithLabels(labels),
	}
	return append(opts, extra...)
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

// materialize writes the embedded assets to a temp directory, recreating their tree, and returns its
// path. The tree carries the Ansible playbook and inventory plus the Terraform working directory.
func materialize() (string, error) {
	dir, err := os.MkdirTemp("", "switchtender-demo-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(assets, "assets", func(path string, entry fs.DirEntry, walkErr error) error {
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
	return dir, nil
}

// seedConfig stores browsable sample projects, inventories, credentials, and templates. The templates
// cover the main tools the engine drives, so the Templates list shows Ansible, Bash, Terraform, Python,
// and Go presets even on a host that lacks a given tool's binary. It is best effort: a store error is
// logged and skipped so the runs still seed.
func seedConfig(ctx context.Context, d Deps, log *zap.Logger) {
	now := time.Now()
	ago := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }

	projects := []*project.Project{
		{ID: project.NewID(), Name: "web-platform", RepoURL: "https://github.com/acme/web-platform.git", Branch: "main", InstallDeps: true, CreatedAt: ago(72)},
		{ID: project.NewID(), Name: "database-ops", RepoURL: "https://github.com/acme/database-ops.git", Branch: "main", InstallDeps: true, CreatedAt: ago(48)},
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
			{"admin", "DELETE", "/v1/templates/tpl_retired", 2},
		}
		for _, h := range history {
			entry := &audit.Entry{
				ID: audit.NewID(), At: ago(h.hoursAgo),
				Actor: h.actor, Method: h.method, Path: h.path,
			}
			if err := d.Audit.Append(ctx, entry); err != nil {
				log.Warn("demo: seed audit entry: " + err.Error())
				break
			}
		}
	}

	creds := []*credential.Credential{
		{ID: credential.NewID(), Name: "prod-ssh", Kind: credential.KindSSHKey, CreatedAt: ago(72)},
		{ID: credential.NewID(), Name: "ansible-vault", Kind: credential.KindVaultPassword, CreatedAt: ago(72)},
		{ID: credential.NewID(), Name: "dockerhub", Kind: credential.KindRegistry, CreatedAt: ago(48)},
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
			next := func(h int) *time.Time { t := now.Add(time.Duration(h) * time.Hour); return &t }
			schedules := []*schedule.Schedule{
				{
					ID: schedule.NewID(), Name: "Nightly audit", Cron: "0 2 * * *",
					TemplateID: templates[2].ID, Enabled: true,
					NextRunAt: next(9), CreatedAt: ago(70),
				},
				{
					ID: schedule.NewID(), Name: "Weekday deploy window", Cron: "30 9 * * 1-5",
					TemplateID: templates[0].ID, Enabled: true,
					NextRunAt: next(17), CreatedAt: ago(46),
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
const scriptLogRotate = `set -euo pipefail
echo "Rotating application logs on $(hostname)"
for svc in web api worker scheduler; do
  echo "  archived and truncated ${svc}.log"
done
echo "Disk after rotation:"
df -h / | tail -1
echo "Log rotation complete"
`

// scriptSmoke is the Bash step that verifies the mixed pipeline after the Ansible configure step.
const scriptSmoke = `set -euo pipefail
echo "Running post-deploy smoke checks"
for ep in / /healthz /metrics; do
  echo "  GET ${ep} -> 200 OK"
done
echo "All endpoints healthy"
`

// scriptReconcile is the Python job for the standalone python run and the Reconcile inventory
// template. It reports inventory drift as JSON and exits zero.
const scriptReconcile = `import json

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
const scriptFleetGo = `package main

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
const scriptProvisionPy = `import json

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
const scriptProvisionSh = `set -euo pipefail
echo "Planning infrastructure"
echo "  network: 10.0.0.0/16"
echo "  subnets: 10.0.1.0/24, 10.0.2.0/24"
echo "Provision plan ready"
`
