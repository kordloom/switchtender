package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestReconcileIsScopedToTheCheckOrg covers the one route that turns a read into a change, and the
// one route the objectless-run sweep missed.
//
// Reconcile authorized each object the proposal would touch: the project, the inventory, the
// credentials, the pull credential. A drift check that names none of them, an inline playbook against
// an inline inventory, presents an empty object list, and authorizing an empty list authorizes
// nothing at all. Every other run route was fixed by scoping such a run to the organization that
// submitted it; reconcile still authorized the bare list, so any operator on the install could turn
// another organization's drift check into a real change on that organization's hosts, dry run forced
// off, proposed under their own name and approvable entirely within their own tenant.
//
// The host was not supposed to be reachable either. The drift view decided visibility once for every
// row, so one organization's host names and drifting task counts were listed to any caller who could
// read a single run of their own, which is how the target of this bypass was found in the first place.
func TestReconcileIsScopedToTheCheckOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWiringFixture(t)

	// The victim's drift check: an inline playbook against an inline inventory, so it names no
	// project, no stored inventory, and no credentials. Only its organization scopes it. The host
	// summary is recorded while the run is still running, because a terminal run is fenced against
	// late writes, and the run is settled afterward the way finalize settles it.
	checked := time.Now()
	check := &run.Run{
		ID: "run_victim_check", Tool: run.ToolAnsible,
		Playbook: victimCheckPlaybook, Inventory: "[all]\nvictim-1\n",
		DryRun: true, OrgID: wiringTheirOrg, Status: run.StatusRunning, CreatedAt: checked,
	}
	if err := f.Runs.Save(ctx, check); err != nil {
		t.Fatalf("Save() check error = %v", err)
	}
	if err := f.Runs.SaveHostSummary(ctx, check.ID, []run.HostSummary{{
		Host: "victim-1", Changed: 3, Worst: "changed", RanAt: checked,
	}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	check.Status, check.EndedAt = run.StatusSucceeded, &checked
	if err := f.Runs.Save(ctx, check); err != nil {
		t.Fatalf("Save() settled check error = %v", err)
	}
	drift, err := f.Runs.DriftStatus(ctx)
	if err != nil {
		t.Fatalf("DriftStatus() error = %v", err)
	}
	if len(drift) != 1 || drift[0].Host != "victim-1" || drift[0].DriftedTasks == 0 {
		t.Fatalf("DriftStatus() = %+v, want drift recorded on the victim's host", drift)
	}

	// Reconcile refuses. Exactly forbidden: reconcile looks up drift itself and reaches the host
	// either way, so accepting a not-found here would let the drift view's filtering stand in for the
	// authorization check and leave the bypass passing this test untouched.
	rec := f.do(t, http.MethodPost, "/v1/drift/reconcile", `{"host":"victim-1"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("reconcile of another tenant's objectless check = %d, want %d (body %s)",
			rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if f.Submitter.gotRun != nil {
		t.Errorf("a change was proposed on the victim organization's hosts: playbook %q limited to %q",
			f.Submitter.gotRun.Playbook, f.Submitter.gotRun.Limit)
	}
	if strings.Contains(rec.Body.String(), victimCheckPlaybook) {
		t.Errorf("the refusal carried the victim's playbook:\n%s", rec.Body.String())
	}

	// The drift view does not list the victim's host to a caller who cannot read the check that
	// observed it.
	list := f.do(t, http.MethodGet, "/v1/drift", "")
	if strings.Contains(list.Body.String(), "victim-1") {
		t.Errorf("the drift view listed another tenant's host:\n%s", list.Body.String())
	}

	// The caller's own drift is still reconcilable, so the scoping did not simply break the feature.
	mine := &run.Run{
		ID: "run_mine_check", Tool: run.ToolAnsible,
		Playbook: "mine.yml", Inventory: "[all]\nmine-1\n",
		DryRun: true, OrgID: wiringMyOrg, Status: run.StatusRunning, CreatedAt: checked,
	}
	if err := f.Runs.Save(ctx, mine); err != nil {
		t.Fatalf("Save() own check error = %v", err)
	}
	if err := f.Runs.SaveHostSummary(ctx, mine.ID, []run.HostSummary{{
		Host: "mine-1", Changed: 2, Worst: "changed", RanAt: checked,
	}}); err != nil {
		t.Fatalf("SaveHostSummary() own error = %v", err)
	}
	mine.Status, mine.EndedAt = run.StatusSucceeded, &checked
	if err := f.Runs.Save(ctx, mine); err != nil {
		t.Fatalf("Save() settled own check error = %v", err)
	}
	own := f.do(t, http.MethodPost, "/v1/drift/reconcile", `{"host":"mine-1"}`)
	if own.Code != http.StatusAccepted {
		t.Errorf("reconcile of the caller's own objectless check = %d, want %d (body %s)",
			own.Code, http.StatusAccepted, own.Body.String())
	}
	if f.Submitter.gotRun == nil || f.Submitter.gotRun.Limit != "mine-1" {
		t.Errorf("the caller's own reconcile did not propose a change limited to their host: %+v",
			f.Submitter.gotRun)
	}
	if list := f.do(t, http.MethodGet, "/v1/drift", ""); !strings.Contains(list.Body.String(), "mine-1") {
		t.Errorf("the drift view withheld the caller's own host:\n%s", list.Body.String())
	}
}
