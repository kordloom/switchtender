package importer_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

func TestPlanApply(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/awx-export.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	plan, err := importer.FromAWX(data, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}

	stores := importer.ApplyStores{
		Projects:    project.NewMemStore(),
		Inventories: inventory.NewMemStore(),
		Sources:     invsource.NewMemStore(),
		Credentials: credential.NewMemStore(),
		Templates:   template.NewMemStore(),
		Schedules:   schedule.NewMemStore(),
	}
	created, err := plan.Apply(context.Background(), stores)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	want := len(plan.Projects) + len(plan.Inventories) + len(plan.Sources) +
		len(plan.Credentials) + len(plan.Templates) + len(plan.Schedules)
	if created != want {
		t.Errorf("Apply() created = %d, want %d", created, want)
	}
	if list, err := stores.Templates.List(context.Background()); err != nil || len(list) != len(plan.Templates) {
		t.Errorf("templates stored = %d (err %v), want %d", len(list), err, len(plan.Templates))
	}
	if list, err := stores.Sources.List(context.Background()); err != nil || len(list) != len(plan.Sources) {
		t.Errorf("sources stored = %d (err %v), want %d", len(list), err, len(plan.Sources))
	}
}

// TestPlanApplyRefusesSourcesWithoutStore verifies Apply writes nothing when the plan has dynamic
// sources but no source store, so a backing inventory is never orphaned.
func TestPlanApplyRefusesSourcesWithoutStore(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/awx-export.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	plan, err := importer.FromAWX(data, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	projects := project.NewMemStore()
	created, err := plan.Apply(context.Background(), importer.ApplyStores{
		Projects:    projects,
		Inventories: inventory.NewMemStore(),
		Credentials: credential.NewMemStore(),
		Templates:   template.NewMemStore(),
		Schedules:   schedule.NewMemStore(),
	})
	if err == nil {
		t.Fatal("Apply() error = nil, want a refusal when sources cannot be stored")
	}
	if created != 0 {
		t.Errorf("Apply() created = %d, want 0 when it refuses up front", created)
	}
	if list, err := projects.List(context.Background()); err != nil || len(list) != 0 {
		t.Errorf("projects stored = %d (err %v), want 0 (nothing written)", len(list), err)
	}
}

// TestApplyingTheSameExportTwiceIsRefused pins the second apply of one export. It created a second
// copy of every object, and a second copy of a schedule fires too, so every imported cadence ran
// twice from the next tick. The second apply now names what is already there and writes nothing.
func TestApplyingTheSameExportTwiceIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data, err := os.ReadFile("testdata/awx-export.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	stores := importer.ApplyStores{
		Projects: project.NewMemStore(), Inventories: inventory.NewMemStore(),
		Sources: invsource.NewMemStore(), Credentials: credential.NewMemStore(),
		Templates: template.NewMemStore(), Schedules: schedule.NewMemStore(),
	}
	first, err := importer.FromAWX(data, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if _, err := first.Apply(ctx, stores); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	schedulesBefore, _ := stores.Schedules.List(ctx)
	templatesBefore, _ := stores.Templates.List(ctx)
	if len(schedulesBefore) == 0 {
		t.Fatal("the fixture imported no schedule, so this cannot show a schedule is not doubled")
	}

	second, err := importer.FromAWX(data, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	created, err := second.Apply(ctx, stores)
	if !errors.Is(err, importer.ErrAlreadyImported) {
		t.Fatalf("second Apply() error = %v, want the duplicate refused", err)
	}
	if created != 0 {
		t.Errorf("second Apply() created %d objects before refusing, want none", created)
	}
	if !strings.Contains(err.Error(), "would create: schedule ") || !strings.Contains(err.Error(), "template ") {
		t.Errorf("the refusal does not lead with the schedule, which is what fires twice, and name the "+
			"template: %v", err)
	}
	schedulesAfter, _ := stores.Schedules.List(ctx)
	templatesAfter, _ := stores.Templates.List(ctx)
	if len(schedulesAfter) != len(schedulesBefore) || len(templatesAfter) != len(templatesBefore) {
		t.Errorf("the refused apply still wrote: schedules %d to %d, templates %d to %d",
			len(schedulesBefore), len(schedulesAfter), len(templatesBefore), len(templatesAfter))
	}
}
