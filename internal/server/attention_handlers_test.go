package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// attentionView is the part of the attention answer these tests compare.
type attentionView struct {
	// Counts are the four counts.
	Counts attention.Counts `json:"counts"`
	// Keys are the listed items' keys.
	Keys []string
	// Total is how many items the caller may see.
	Total int `json:"total"`
}

// attentionFixture builds a server whose attention view reads a held run the reader's organization
// owns, a held run on a project another organization owns, and a run whose worker was lost long
// enough ago to alert.
func attentionFixture(t *testing.T, withSource bool) (http.Handler, *user.User) {
	t.Helper()
	ctx := context.Background()
	users := user.NewMemStore()
	orgs := org.NewMemStore()
	projects := project.NewMemStore()
	runs := run.NewMemStore()
	for _, o := range []*org.Org{
		{ID: "org_reader", Name: "reader"}, {ID: "org_owner", Name: "owner"},
	} {
		if err := orgs.Save(ctx, o); err != nil {
			t.Fatalf("Save(org) error = %v", err)
		}
	}
	if err := projects.Save(ctx, &project.Project{ID: "proj_p", Name: "prod",
		RepoURL: "https://example.com/p.git", OrgID: "org_owner"}); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	reader, err := user.New("reader", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, reader); err != nil {
		t.Fatalf("Save(user) error = %v", err)
	}
	if err := orgs.AddMember(ctx, "org_reader", reader.ID, org.RoleMember); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	lost := time.Now().Add(-2 * time.Minute)
	for _, r := range []*run.Run{
		{ID: "run_mine", Playbook: "mine.yml", Status: run.StatusPendingApproval,
			CreatedAt: time.Now().Add(-time.Minute), OrgID: "org_reader"},
		{ID: "run_theirs", Playbook: "theirs.yml", Status: run.StatusPendingApproval,
			CreatedAt: time.Now().Add(-time.Minute), ProjectID: "proj_p", OrgID: "org_owner"},
		{ID: "run_lost", Playbook: "lost.yml", Status: run.StatusRunning, CreatedAt: lost,
			ClaimedBy: "worker-gone", ClaimedAt: &lost, OrgID: "org_reader"},
	} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	opts := []Option{WithGrants(grant.NewMemStore(), true), WithOrgs(orgs),
		WithProjects(projects), WithUsers(users)}
	if withSource {
		opts = append(opts, WithAttention(&attention.Source{Runs: runs,
			Timing: attention.DefaultTiming(30*time.Second, 3*time.Second, 10*time.Second,
				10*time.Second)}))
	}
	return New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(), opts...).Handler(),
		reader
}

// TestAttentionViewShowsOnlyWhatTheCallerMayRead pins the dashboard's answer: the four counts and
// the items, narrowed by ?blocker=, and only what the caller may read, so the view never tells a
// reader that another organization's work exists or what is stopping it.
func TestAttentionViewShowsOnlyWhatTheCallerMayRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WithSource bool
		AsReader   bool
		Query      string
		WantCode   int
		WantView   attentionView
	}{{ // Test 0: An admin sees every item, most urgent first, and the counts add up to them.
		WithSource: true, WantCode: http.StatusOK,
		WantView: attentionView{Counts: attention.Counts{ApprovalNeeded: 2, WorkerLost: 1},
			Keys: []string{"run_lost", "run_mine", "run_theirs"}, Total: 3},
	}, { // Test 1: A reader sees only what their organization owns, counted the same way.
		WithSource: true, AsReader: true, WantCode: http.StatusOK,
		WantView: attentionView{Counts: attention.Counts{ApprovalNeeded: 1, WorkerLost: 1},
			Keys: []string{"run_lost", "run_mine"}, Total: 2},
	}, { // Test 2: The blocker filter narrows the list and leaves the counts whole.
		WithSource: true, Query: "?blocker=worker_lost", WantCode: http.StatusOK,
		WantView: attentionView{Counts: attention.Counts{ApprovalNeeded: 2, WorkerLost: 1},
			Keys: []string{"run_lost"}, Total: 3},
	}, { // Test 3: A blocker the view does not know is refused rather than matching nothing.
		WithSource: true, Query: "?blocker=stuck", WantCode: http.StatusBadRequest,
	}, { // Test 4: A server without a source answers with nothing waiting.
		WantCode: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler, reader := attentionFixture(t, test.WithSource)
			req := httptest.NewRequest(http.MethodGet, "/v1/attention"+test.Query, nil)
			actor := Actor{Role: user.RoleAdmin, Name: "ops-admin"}
			if test.AsReader {
				actor = Actor{UserID: reader.ID, Role: user.RoleViewer, Name: "reader"}
			}
			req = req.WithContext(context.WithValue(req.Context(), actorKey{}, actor))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Fatalf("status mismatch (-want +got):\n%s\nbody %s", diff, rec.Body.String())
			}
			if test.WantCode != http.StatusOK {
				return
			}
			var body struct {
				attentionView
				// Items are the listed items.
				Items []attention.Item `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := body.attentionView
			for _, it := range body.Items {
				got.Keys = append(got.Keys, it.Key)
			}
			if diff := cmp.Diff(test.WantView, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("view mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDoctorReportsWhatIsPastItsAlertThreshold pins that the doctor warns about work that has
// needed attention past its alert threshold, and about nothing that has not.
func TestDoctorReportsWhatIsPastItsAlertThreshold(t *testing.T) {
	t.Parallel()
	handler, _ := attentionFixture(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/doctor", nil)
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{Role: user.RoleAdmin, Name: "ops-admin"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("doctor = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var report doctorReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	type finding struct {
		// ObjectID is what holds the problem.
		ObjectID string
		// FixPath is where it is repaired.
		FixPath string
	}
	var got []finding
	for _, f := range report.Findings {
		if f.ObjectType == "run" {
			got = append(got, finding{ObjectID: f.ObjectID, FixPath: f.FixPath})
		}
	}
	want := []finding{{ObjectID: "run_lost", FixPath: "/ui/?attention=worker_lost"}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("run findings mismatch (-want +got):\n%s", diff)
	}
}
