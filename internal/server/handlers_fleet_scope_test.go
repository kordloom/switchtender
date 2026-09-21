package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTaskTrendsAreWithheldFromAGrantScopedCallerWhoCanReadSomething pins the tenant boundary on
// the fleet aggregate that carries no run ids to scope by.
//
// The existing refusal covered a caller who may read nothing. The leak lived one step over: a
// grant-scoped caller who may read one run of their own passed that probe and received the
// install-wide aggregate whole, which is every other tenant's task names, counts, and durations,
// served on the strength of being allowed to see one's own work. With nothing on the rows to
// filter, the only honest answers are everything, for a caller who may read everything, or a
// withholding that says it is one, so an empty panel cannot be mistaken for an idle install.
func TestTaskTrendsAreWithheldFromAGrantScopedCallerWhoCanReadSomething(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	for _, r := range []*run.Run{
		{ID: "run_mine", Playbook: "mine.yml", InventoryID: "inv_mine",
			Status: run.StatusSucceeded, CreatedAt: time.Now()},
		{ID: "run_theirs", Playbook: "theirs.yml", InventoryID: "inv_theirs",
			Status: run.StatusSucceeded, CreatedAt: time.Now()},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("seed run %s: %v", r.ID, err)
		}
	}
	// The caller holds a real grant, so their reads are scoped and run_mine is readable to them.
	authz := restrictedAuthz(t, "user_granted", "inv_mine")

	rec := httptest.NewRecorder()
	taskTrendsHandler(store, authz, zap.NewNop()).ServeHTTP(rec,
		withActor(t, http.MethodGet, "/v1/tasks",
			Actor{UserID: "user_granted", Role: user.RoleViewer}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	var got taskTrendsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode task trends: %v", err)
	}
	if got.Count != 0 || len(got.Tasks) != 0 {
		t.Errorf("a grant-scoped caller was shown %d install-wide task rows", got.Count)
	}
	if !got.Withheld {
		t.Error("the response does not say the aggregate was withheld, so an empty panel reads " +
			"as an idle install rather than as a scoped caller")
	}
}
