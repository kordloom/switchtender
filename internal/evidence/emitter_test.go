package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// seedPeriod stores one run inside the period and one outside it.
func seedPeriod(t *testing.T) (run.Store, audit.Store, time.Time) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for _, r := range []*run.Run{
		{ID: "run_in", Playbook: "site.yml", Status: run.StatusSucceeded, Actor: "deploy-bot",
			CreatedAt: base, HeldByPolicy: "prod terraform destroy"},
		{ID: "run_out", Playbook: "old.yml", Status: run.StatusSucceeded,
			CreatedAt: base.Add(-60 * 24 * time.Hour)},
	} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	if err := audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base, Actor: "root", Method: "POST", Path: "/v1/runs/run_in/approve",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	return runs, audits, base
}

func TestEmitWritesThePeriodPack(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()
	var gotPath string
	e := NewEmitter(runs, audits, dir, time.Hour, nil,
		WithNotify(func(p string, _, _ time.Time) { gotPath = p }))
	defer e.Close()

	from, to := base.Add(-time.Hour), base.Add(24*time.Hour)
	if err := e.Emit(from, to); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	// Named for the period it covers, so a missing period is a missing file.
	want := filepath.Join(dir, "change-register-20260701-to-20260702.html")
	if gotPath != want {
		t.Errorf("notified path = %q, want %q", gotPath, want)
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	html := string(body)
	if !strings.Contains(html, "run_in") {
		t.Error("the pack omits a change inside the period")
	}
	if strings.Contains(html, "run_out") {
		t.Error("the pack carries a change from outside the period")
	}
	if !strings.Contains(html, "prod terraform destroy") {
		t.Error("the pack does not name the rule that held the change")
	}
}

func TestEmitReturnsFailureRatherThanLeavingASilentHole(t *testing.T) {
	t.Parallel()
	runs, audits, base := seedPeriod(t)
	// A directory that cannot be created: an archive with an unreported gap is worse than one
	// that is loudly incomplete, because only the second gets fixed.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	e := NewEmitter(runs, audits, filepath.Join(file, "packs"), time.Hour, nil)
	defer e.Close()
	err := e.Emit(base, base.Add(time.Hour))
	if err == nil {
		t.Fatal("Emit() reported success while writing nothing")
	}
	// The message names the directory, so an operator reading it fixes the right thing rather
	// than working backwards from a path deep inside a filename.
	if !strings.Contains(err.Error(), "evidence directory") {
		t.Errorf("error = %q, want it to name the directory that could not be made", err)
	}
}

func TestNewEmitterRefusesACadenceTooShortToCoverAPeriod(t *testing.T) {
	t.Parallel()
	runs, audits, _ := seedPeriod(t)
	defer func() {
		if recover() == nil {
			t.Error("NewEmitter accepted a cadence that cannot produce a period register")
		}
	}()
	NewEmitter(runs, audits, t.TempDir(), time.Minute, nil)
}
