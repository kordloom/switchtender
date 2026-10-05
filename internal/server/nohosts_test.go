package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// errNoHostsMatched is the refusal the dispatcher gives a launch whose composed inventory resolved
// to no hosts.
var errNoHostsMatched = fmt.Errorf("%w: patch window matched nothing the launching actor may use",
	inventory.ErrNoHosts)

// noHostsTemplates returns a template store holding one template over a composed inventory.
func noHostsTemplates(t *testing.T) template.Store {
	t.Helper()
	templates := template.NewMemStore()
	if err := templates.Save(context.Background(), &template.Template{
		ID: "tpl_patch", Name: "patch", Playbook: "patch.yml", InventoryID: "inv_window",
	}); err != nil {
		t.Fatalf("Save(template) error = %v", err)
	}
	return templates
}

// TestManualLaunchMatchingNoHostsLinksToThePreview pins the answer a person gets when a launch's
// composed inventory matches no hosts: the reason, and a link to the inventory's host preview, both
// in the text and as preview_url, from every way a person launches. A refusal that only said
// nothing matched left the person to find the inventory and preview it by hand.
func TestManualLaunchMatchingNoHostsLinksToThePreview(t *testing.T) {
	t.Parallel()
	const wantURL = "/ui/inventories?preview=inv_window"
	ended := time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC)
	tests := []struct {
		Method      string
		Path        string
		Body        string
		SubmitErr   error
		WantStatus  int
		WantPreview string
	}{{ // Test 0: A template launch.
		Method: http.MethodPost, Path: "/v1/templates/tpl_patch/launch", SubmitErr: errNoHostsMatched,
		WantStatus: http.StatusBadRequest, WantPreview: wantURL,
	}, { // Test 1: A run submitted directly against the stored inventory.
		Method: http.MethodPost, Path: "/v1/runs", SubmitErr: errNoHostsMatched,
		Body:       `{"playbook":"patch.yml","inventory_id":"inv_window"}`,
		WantStatus: http.StatusBadRequest, WantPreview: wantURL,
	}, { // Test 2: A rerun, which resolves the inventory again.
		Method: http.MethodPost, Path: "/v1/runs/run_done/rerun", SubmitErr: errNoHostsMatched,
		WantStatus: http.StatusBadRequest, WantPreview: wantURL,
	}, { // Test 3: Any other refusal carries no preview link.
		Method: http.MethodPost, Path: "/v1/templates/tpl_patch/launch",
		SubmitErr:  fmt.Errorf("%w: input inv_x", inventory.ErrResolve),
		WantStatus: http.StatusBadRequest,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs := run.NewMemStore()
			if err := runs.Save(context.Background(), &run.Run{
				ID: "run_done", Playbook: "patch.yml", InventoryID: "inv_window",
				Status: run.StatusSucceeded, CreatedAt: ended, EndedAt: &ended,
			}); err != nil {
				t.Fatalf("Save(run) error = %v", err)
			}
			handler := New(runs, &fakeSubmitter{err: test.SubmitErr}, zap.NewNop(),
				WithTemplates(noHostsTemplates(t))).Handler()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(test.Method, test.Path, strings.NewReader(test.Body))
			if test.Body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			var got noHostsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if got.PreviewURL != test.WantPreview {
				t.Errorf("preview_url = %q, want %q", got.PreviewURL, test.WantPreview)
			}
			if test.WantPreview != "" && !strings.HasSuffix(got.Error,
				"See which hosts it matches now: "+test.WantPreview) {
				t.Errorf("error = %q, want it to end with the preview link", got.Error)
			}
			if !strings.Contains(got.Error, test.SubmitErr.Error()) {
				t.Errorf("error = %q, want it to keep the reason %q", got.Error, test.SubmitErr)
			}
		})
	}
}

// skipRefusingChain takes every entry but a webhook skip's, standing in for a chain that fails
// between the fire's entry and the skip's.
type skipRefusingChain struct {
	audit.Store
}

// Append refuses a skip entry and keeps every other.
func (s skipRefusingChain) Append(ctx context.Context, e *audit.Entry) error {
	if strings.HasSuffix(e.Path, "/skipped") {
		return errors.New("chain unavailable")
	}
	return s.Store.Append(ctx, e)
}

// TestWebhookFireMatchingNoHostsIsSkipped pins what a webhook delivery whose template's composed
// inventory matches no hosts gets back and leaves behind: a 200 that says skipped, so the sender
// does not deliver it again, its own chain entry after the fire's, and no stamp on the trigger,
// which records when it last launched a run. An ordinary launch failure is still an error, and a
// skip the chain refuses is refused rather than answered as done.
func TestWebhookFireMatchingNoHostsIsSkipped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		SubmitErr  error
		RefuseSkip bool
		WantStatus int
		WantBody   map[string]string
		WantPaths  []string
	}{{ // Test 0: No hosts matched, so the delivery is answered as skipped and recorded.
		SubmitErr: errNoHostsMatched, WantStatus: http.StatusOK,
		WantBody:  map[string]string{"skipped": "no hosts matched"},
		WantPaths: []string{"fired", "skipped"},
	}, { // Test 1: An ordinary failure is still a failure.
		SubmitErr: errors.New("dispatcher unavailable"), WantStatus: http.StatusBadGateway,
		WantPaths: []string{"fired"},
	}, { // Test 2: A skip the chain will not take is refused, never answered as done.
		SubmitErr: errNoHostsMatched, RefuseSkip: true, WantStatus: http.StatusServiceUnavailable,
		WantPaths: []string{"fired"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			chain := audit.NewMemStore()
			audits := audit.Store(chain)
			if test.RefuseSkip {
				audits = skipRefusingChain{Store: chain}
			}
			triggers := trigger.NewMemStore()
			token, tg, err := trigger.New("push", "tpl_patch")
			if err != nil {
				t.Fatalf("trigger.New() error = %v", err)
			}
			if err := triggers.Save(ctx, tg); err != nil {
				t.Fatalf("Save(trigger) error = %v", err)
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{err: test.SubmitErr}, zap.NewNop(),
				WithTriggers(triggers, nil), WithTemplates(noHostsTemplates(t)),
				WithAudit(audits)).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+token,
				strings.NewReader(`{"ref":"refs/heads/main"}`)))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			if test.WantBody != nil {
				want := map[string]string{"trigger": tg.ID}
				for k, v := range test.WantBody {
					want[k] = v
				}
				var got map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode %s: %v", rec.Body.String(), err)
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("body mismatch (-want +got):\n%s", diff)
				}
			}
			entries, err := chain.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var paths []string
			for _, e := range entries {
				paths = append(paths, strings.TrimPrefix(e.Path, "/hooks/"+tg.ID+"/"))
			}
			if diff := cmp.Diff(test.WantPaths, paths, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("chain mismatch (-want +got):\n%s", diff)
			}
			if ok, at := audit.Verify(entries); !ok {
				t.Errorf("the chain does not verify, broken at %d", at)
			}
			stored, err := triggers.Get(ctx, tg.ID)
			if err != nil {
				t.Fatalf("Get(trigger) error = %v", err)
			}
			if stored.LastFiredAt != nil {
				t.Errorf("LastFiredAt = %v, want none: the delivery launched no run",
					stored.LastFiredAt)
			}
		})
	}
}

// TestDoctorWarnsOnASchedulePastTheSkipBadge pins the doctor's warning for a schedule whose recent
// fires were all skipped: at the badge's count it warns, names the inventory, and links to its
// preview, and below it, where a skip can be ordinary, it says nothing.
func TestDoctorWarnsOnASchedulePastTheSkipBadge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{ID: "inv_window", Name: "patch window",
		Kind: inventory.KindSmart, HostFilter: "name=web1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save(inventory) error = %v", err)
	}
	tests := []struct {
		Skipped      int
		WantFindings []doctorFinding
	}{{ // Test 0: One short of the badge is not a finding.
		Skipped: schedule.SkipBadgeFires - 1,
	}, { // Test 1: At the badge, a warning that names the inventory and links to its preview.
		Skipped: schedule.SkipBadgeFires,
		WantFindings: []doctorFinding{{
			Severity: "warning", ObjectType: "schedule", ObjectID: "sch_patch",
			ObjectName: "nightly patch",
			Problem: fmt.Sprintf("Matched no hosts for the last %d fires, so none of them started "+
				"a run. Its inventory \"patch window\" resolves to no hosts. Preview it at "+
				"/ui/inventories?preview=inv_window.", schedule.SkipBadgeFires),
			FixPath: "/ui/inventories?preview=inv_window",
		}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			schedules := schedule.NewMemStore()
			if err := schedules.Save(ctx, &schedule.Schedule{
				ID: "sch_patch", Name: "nightly patch", Cron: "0 2 * * *", TemplateID: "tpl_patch",
				Enabled: true, CreatedAt: time.Now(), LastSkip: schedule.SkipNoHosts,
				SkippedFires: test.Skipped,
			}); err != nil {
				t.Fatalf("Save(schedule) error = %v", err)
			}
			h := doctorHandler(noHostsTemplates(t), schedules, nil, invs, nil, nil, nil, nil,
				zap.NewNop())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
			var report doctorReport
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if diff := cmp.Diff(test.WantFindings, report.Findings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestScheduleEditKeepsTheSkipsItRecorded pins that editing a schedule keeps its count of skipped
// fires, which records fires that already happened. Dropping it on every edit would clear the badge
// for a schedule whose inventory still matches nothing.
func TestScheduleEditKeepsTheSkipsItRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	schedules := schedule.NewMemStore()
	if err := schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_patch", Name: "nightly patch", Cron: "0 2 * * *", TemplateID: "tpl_patch",
		Enabled: true, CreatedAt: time.Now(), LastSkip: schedule.SkipNoHosts, SkippedFires: 4,
	}); err != nil {
		t.Fatalf("Save(schedule) error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTemplates(noHostsTemplates(t)), WithSchedules(schedules)).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/schedules/sch_patch",
		strings.NewReader(`{"name":"renamed","cron":"0 3 * * *","template_id":"tpl_patch"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := schedules.Get(ctx, "sch_patch")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "renamed" || got.SkippedFires != 4 || got.LastSkip != schedule.SkipNoHosts {
		t.Errorf("after the edit: name %q, skip %q x%d, want renamed and the skips kept", got.Name,
			got.LastSkip, got.SkippedFires)
	}
}
