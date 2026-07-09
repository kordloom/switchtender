package importer_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/importer"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/template"
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
		Credentials: credential.NewMemStore(),
		Templates:   template.NewMemStore(),
		Schedules:   schedule.NewMemStore(),
	}
	created, err := plan.Apply(context.Background(), stores)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	want := len(plan.Projects) + len(plan.Inventories) + len(plan.Credentials) +
		len(plan.Templates) + len(plan.Schedules)
	if created != want {
		t.Errorf("Apply() created = %d, want %d", created, want)
	}
	if list, err := stores.Templates.List(context.Background()); err != nil || len(list) != len(plan.Templates) {
		t.Errorf("templates stored = %d (err %v), want %d", len(list), err, len(plan.Templates))
	}
}
