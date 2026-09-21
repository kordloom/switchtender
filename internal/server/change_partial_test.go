package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAChangeBiggerThanTheFetchCapSaysItIsPartial pins the summary's honesty about its own reach.
//
// The handler reads at most the cap's worth of member runs and derives the change's outcome, span,
// and actors from what it read. A change with more members than that was summarized from an
// incomplete membership with nothing saying so: an outcome computed over the newest thousand runs
// of a change whose failure is older than that reads succeeded, and the caller has no way to know
// the summary never saw the failure. The fetch reads one past the cap so hitting it is observable,
// and the response says partial rather than passing the summary off as the whole change.
func TestAChangeBiggerThanTheFetchCapSaysItIsPartial(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	// seed stores n member runs of one change and returns the decoded summary.
	summarize := func(t *testing.T, name string, n int) (partial bool, total int) {
		t.Helper()
		store := run.NewMemStore()
		for i := 0; i < n; i++ {
			end := base.Add(time.Duration(i) * time.Second)
			if err := store.Save(t.Context(), &run.Run{
				ID: fmt.Sprintf("r%04d", i), Playbook: "site.yml", Status: run.StatusSucceeded,
				CreatedAt: end, EndedAt: &end,
				Labels: map[string]string{run.ChangeLabel: name},
			}); err != nil {
				t.Fatalf("seed run %d: %v", i, err)
			}
		}
		handler := changeHandler(store, &authorizer{}, zap.NewNop())
		req := httptest.NewRequest(http.MethodGet, "/v1/changes/"+name, nil).WithContext(
			actorCtx(Actor{UserID: "user_ops", Role: user.RoleViewer}))
		req.SetPathValue("change", name)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%q)", rec.Code, rec.Body.String())
		}
		var got struct {
			Partial bool `json:"partial"`
			Total   int  `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Partial, got.Total
	}

	// One past the cap: the summary must say it is partial.
	partial, total := summarize(t, "OPS-900", maxListRows+1)
	if !partial {
		t.Errorf("a change of %d members was summarized without saying partial: the outcome was "+
			"derived from runs the handler never read", maxListRows+1)
	}
	if total > maxListRows {
		t.Errorf("total = %d claims more members than were read; the count must be as honest as "+
			"the flag", total)
	}

	// Exactly the cap: complete, and saying partial here would teach callers to ignore the flag.
	if partial, _ := summarize(t, "OPS-901", maxListRows); partial {
		t.Error("a change of exactly the cap's size was flagged partial; a false alarm makes the " +
			"real one unreadable")
	}
}
