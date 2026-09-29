package schedule

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// TestAScheduledRunBelongsToTheSchedulesOrg covers who can see the runs a tenant's own schedules fire.
//
// A schedule carries the organization that created it, and a run that references no stored object is
// scoped by the organization it was stamped with, because there is nothing else to scope it by. The
// scheduler stamped nothing: it submits with a source and, for a template, the template's preset, and
// neither carries the org, while the tick loop has no actor for the submit path to infer one from.
//
// So on a strict-grants install every run a schedule fired was ownerless, which for a non-admin means
// denied. The tenant that imported a crontab could see its schedules and not one of the runs they
// produced: the schedule's last-run link answered 403, the runs appeared in no list, and only an admin
// could tell whether the nightly work had run at all. The relay's proposed-apply path had already fixed
// this same gap by stamping the plan's org onto the apply; the scheduler never did.
func TestAScheduledRunBelongsToTheSchedulesOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, test := range []struct {
		// Name says which shape of schedule fires.
		Name string
		// Schedule is the schedule the tick fires.
		Schedule *Schedule
		// Template is stored when the schedule names one.
		Template *template.Template
		// WantOrg is the organization the run must be stamped with.
		WantOrg string
	}{{ // Test 0: An inline playbook, the shape every crontab import produces.
		Name: "inline playbook",
		Schedule: &Schedule{
			ID: "sch_inline", Name: "nightly", Cron: "0 3 * * *", Playbook: "site.yml",
			Inventory: "prod", Enabled: true, OrgID: "org_acme",
		},
		WantOrg: "org_acme",
	}, { // Test 1: An inline split, which fans out into shards that each need the same owner.
		Name: "inline split",
		Schedule: &Schedule{
			ID: "sch_split", Name: "nightly split", Cron: "0 3 * * *", Playbook: "site.yml",
			Inventory: "prod", Shards: 3, Enabled: true, OrgID: "org_acme",
		},
		WantOrg: "org_acme",
	}, { // Test 2: An inline pipeline.
		Name: "inline pipeline",
		Schedule: &Schedule{
			ID: "sch_pipe", Name: "nightly pipeline", Cron: "0 3 * * *", Inventory: "prod",
			Steps: []run.PipelineStep{{Name: "one", Playbook: "one.yml"}}, Enabled: true,
			OrgID: "org_acme",
		},
		WantOrg: "org_acme",
	}, { // Test 3: A stored template. The template's own organization owns the work, and the schedule
		// carries the same one, so either source gives the right answer.
		Name: "template",
		Schedule: &Schedule{
			ID: "sch_tpl", Name: "nightly template", Cron: "0 3 * * *", TemplateID: "tpl_1",
			Enabled: true, OrgID: "org_acme",
		},
		Template: &template.Template{
			ID: "tpl_1", Name: "deploy", Playbook: "site.yml", Inventory: "prod", OrgID: "org_acme",
		},
		WantOrg: "org_acme",
	}} {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			schedules := NewMemStore()
			if err := schedules.Save(ctx, test.Schedule); err != nil {
				t.Fatalf("Save() schedule error = %v", err)
			}
			templates := template.NewMemStore()
			if test.Template != nil {
				if err := templates.Save(ctx, test.Template); err != nil {
					t.Fatalf("Save() template error = %v", err)
				}
			}
			rec := &recordingSubmitter{}
			s := NewScheduler(schedules, rec, zap.NewNop(), WithTemplates(templates))

			id, err := s.fire(ctx, test.Schedule)
			if err != nil {
				t.Fatalf("fire() error = %v", err)
			}
			if id == "" {
				t.Fatal("fire() returned no run id")
			}
			if rec.got == nil {
				t.Fatal("fire() reached no submitter")
			}
			if rec.got.OrgID != test.WantOrg {
				t.Errorf("the run a schedule fired is owned by %q, want %q: an ownerless run is denied "+
					"to every non-admin under strict grants, so the tenant cannot see the runs its own "+
					"schedule produced", rec.got.OrgID, test.WantOrg)
			}
		})
	}
}

// recordingSubmitter records the options a submission carried, applied to a probe run, so a test can
// assert what the scheduler asked for on every submit shape.
type recordingSubmitter struct {
	// got is a probe run with the most recent submission's options applied.
	got *run.Run
}

// probe applies opts to a fresh run and records it.
func (r *recordingSubmitter) probe(opts []run.SubmitOption) *run.Run {
	out := &run.Run{ID: run.NewID(), Status: run.StatusPending, CreatedAt: time.Now()}
	run.ApplyOptions(out, opts)
	r.got = out
	return out
}

// Submit records a plain submission.
func (r *recordingSubmitter) Submit(_ context.Context, playbook, inventory string,
	opts ...run.SubmitOption) (*run.Run, error) {
	out := r.probe(opts)
	out.Playbook, out.Inventory = playbook, inventory
	return out, nil
}

// SubmitSplit records a split submission.
func (r *recordingSubmitter) SubmitSplit(_ context.Context, playbook, inventory string, shards int,
	opts ...run.SubmitOption) (*run.Run, error) {
	out := r.probe(opts)
	out.Playbook, out.Inventory = playbook, inventory
	out.ShardCount = &shards
	return out, nil
}

// SubmitPipeline records a pipeline submission.
func (r *recordingSubmitter) SubmitPipeline(_ context.Context, name, inventory string,
	steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	out := r.probe(opts)
	out.Playbook, out.Inventory, out.Steps = name, inventory, steps
	return out, nil
}
