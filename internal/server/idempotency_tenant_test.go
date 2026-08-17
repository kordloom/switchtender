package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAnIdempotencyKeyDoesNotCrossOrganizations covers a cross-tenant leak reachable with nothing but
// a header. Idempotency keys are chosen by callers, and callers choose ordinary words, so two
// organizations on one install collide as a matter of course. The stored key was global: the second
// organization's submission resolved to the first organization's run and the API answered with it, id,
// command, actor and all, while the change the second caller actually asked for never ran. No error
// was raised on either side.
func TestAnIdempotencyKeyDoesNotCrossOrganizations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	orgs := org.NewMemStore()
	runs := run.NewMemStore()

	// Two organizations, each with an operator who may submit runs.
	type tenant struct {
		org   string
		token string
	}
	var made []tenant
	for _, name := range []string{"acme", "globex"} {
		o := &org.Org{ID: "org_" + name, Name: name}
		if err := orgs.Save(ctx, o); err != nil {
			t.Fatalf("Save org: %v", err)
		}
		u, err := user.New(name+"-operator", "pw", user.RoleOperator)
		if err != nil {
			t.Fatalf("user.New: %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save user: %v", err)
		}
		if err := orgs.AddMember(ctx, o.ID, u.ID, org.RoleMember); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		plain, tok, err := auth.New(name + "-token")
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("Save token: %v", err)
		}
		made = append(made, tenant{org: o.ID, token: plain})
	}

	handler := New(runs, newKeyedSubmitter(runs), zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithOrgs(orgs)).Handler()

	submit := func(bearer, key, command string) (int, map[string]any) {
		body := `{"tool":"bash","command":"` + command + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	const shared = "nightly-deploy"
	code, first := submit(made[0].token, shared, "acme secret deploy")
	if code != http.StatusCreated && code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("first submission = %d (%v)", code, first)
	}
	code, second := submit(made[1].token, shared, "globex own deploy")
	if code != http.StatusCreated && code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("second submission = %d (%v)", code, second)
	}

	if first["id"] == second["id"] {
		t.Fatalf("both organizations' submissions resolved to run %v: the second caller was handed "+
			"the first organization's run instead of getting their own", first["id"])
	}
	if cmd, _ := second["command"].(string); strings.Contains(cmd, "acme") {
		t.Errorf("the second organization's response carries the first organization's command %q", cmd)
	}

	// The same caller repeating their own key still deduplicates, which is the point of the header.
	code, repeat := submit(made[1].token, shared, "globex own deploy")
	if code != http.StatusCreated && code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("repeat submission = %d (%v)", code, repeat)
	}
	if repeat["id"] != second["id"] {
		t.Errorf("a repeated key from the same organization created a second run (%v then %v)",
			second["id"], repeat["id"])
	}
}

// keyedSubmitter stores each submitted run and honors the idempotency key the handler resolved, which
// is the behavior under test: the dispatcher's own lookup is keyed on exactly this value.
type keyedSubmitter struct {
	// runs is where submitted runs land, and where a repeated key finds its original.
	runs run.Store
}

// newKeyedSubmitter returns a submitter backed by store.
func newKeyedSubmitter(store run.Store) *keyedSubmitter { return &keyedSubmitter{runs: store} }

// Submit records the run, returning the existing one when its idempotency key was already used.
func (k *keyedSubmitter) Submit(ctx context.Context, playbook, inventory string,
	opts ...run.SubmitOption) (*run.Run, error) {
	r := &run.Run{ID: run.NewID(), Playbook: playbook, Inventory: inventory,
		Status: run.StatusPending, CreatedAt: time.Now()}
	run.ApplyOptions(r, opts)
	if r.IdempotencyKey != "" {
		if existing, err := k.runs.ByIdempotencyKey(ctx, r.IdempotencyKey); err == nil && existing != nil {
			return existing, nil
		}
	}
	if err := k.runs.Save(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// SubmitSplit is unused here and reports as much rather than pretending to shard.
func (k *keyedSubmitter) SubmitSplit(context.Context, string, string, int,
	...run.SubmitOption) (*run.Run, error) {
	return nil, errors.New("not used in this test")
}

// SubmitPipeline is unused here.
func (k *keyedSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep,
	...run.SubmitOption) (*run.Run, error) {
	return nil, errors.New("not used in this test")
}
