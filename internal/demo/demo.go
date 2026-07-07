// Package demo seeds a Yardmaster store with lifelike sample data by running real playbooks through
// the engine, so a public read-only instance shows genuine host matrices, split runs, pipelines,
// and cross-run fleet memory rather than fabricated records.
package demo

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/template"
)

// assets holds the playbook and inventory the seeder runs.
//
//go:embed assets/*
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
}

// Seed populates the stores with sample configuration and a set of runs that exercise the matrix,
// splits, pipelines, and cross-run fleet memory. It runs real playbooks locally, so it needs
// ansible on the PATH, and returns when every seeded run has finished.
func Seed(ctx context.Context, d Deps, log *zap.Logger) error {
	dir, err := materialize()
	if err != nil {
		return fmt.Errorf("materialize demo assets: %w", err)
	}
	playbook := filepath.Join(dir, "site.yml")
	inv := filepath.Join(dir, "inv.ini")

	seedConfig(ctx, d, log)

	// Plain runs where db01 flaps between failing and passing, so fleet memory marks it flaky.
	failByRun := []string{"", "db01", "", "db01", ""}
	for _, failHost := range failByRun {
		r, err := d.Submitter.Submit(ctx, playbook, inv, failVars(failHost)...)
		if err != nil {
			return fmt.Errorf("seed run: %w", err)
		}
		waitTerminal(ctx, d.Runs, r.ID)
	}

	// A split where one shard fails, showing the merged matrix and failed-shard isolation.
	split, err := d.Submitter.SubmitSplit(ctx, playbook, inv, 3, failVars("db01")...)
	if err != nil {
		return fmt.Errorf("seed split: %w", err)
	}
	waitTerminal(ctx, d.Runs, split.ID)

	// A clean three-step pipeline.
	steps := []run.PipelineStep{
		{Name: "prepare", Playbook: playbook},
		{Name: "migrate", Playbook: playbook},
		{Name: "verify", Playbook: playbook},
	}
	pipe, err := d.Submitter.SubmitPipeline(ctx, "Release 4.2", inv, steps)
	if err != nil {
		return fmt.Errorf("seed pipeline: %w", err)
	}
	waitTerminal(ctx, d.Runs, pipe.ID)

	// One more failure on a different host for variety.
	last, err := d.Submitter.Submit(ctx, playbook, inv, failVars("edge01")...)
	if err != nil {
		return fmt.Errorf("seed run: %w", err)
	}
	waitTerminal(ctx, d.Runs, last.ID)

	log.Info("demo: seeded sample projects, templates, inventories, and runs")
	return nil
}

// failVars returns the submit options that make the run fail on one host, or none for a clean run.
func failVars(host string) []run.SubmitOption {
	if host == "" {
		return nil
	}
	return []run.SubmitOption{run.WithExtraVars(map[string]any{"fail_host": host})}
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

// materialize writes the embedded playbook and inventory to a temp directory and returns its path.
func materialize() (string, error) {
	dir, err := os.MkdirTemp("", "yardmaster-demo-")
	if err != nil {
		return "", err
	}
	for _, name := range []string{"site.yml", "inv.ini"} {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// seedConfig stores browsable sample projects, inventories, credentials, and templates. It is best
// effort: a store error is logged and skipped so the runs still seed.
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
	}
	for _, t := range templates {
		if err := d.Templates.Save(ctx, t); err != nil {
			log.Warn("demo: seed template: " + err.Error())
		}
	}
}
