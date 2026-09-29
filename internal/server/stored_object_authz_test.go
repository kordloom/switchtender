package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"time"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestWritesOnConfigurationStayAdminOnly pins that raising the read role on schedules, triggers, and
// inventory sources did not lower the write role on them.
//
// The block that raised those reads from viewer to operator sat above the method check, and the
// switch below it defaults to admin, so it silently lowered every write on all three families as
// well. Eleven routes moved from admin to operator without a line of test noticing, because the test
// that shipped with the change asserted GET paths only. Two independent reviews found it.
func TestWritesOnConfigurationStayAdminOnly(t *testing.T) {
	t.Parallel()
	writes := []struct {
		Method string
		Path   string
	}{
		{http.MethodPost, "/v1/schedules"},
		{http.MethodPut, "/v1/schedules/sch_1"},
		{http.MethodDelete, "/v1/schedules/sch_1"},
		{http.MethodPost, "/v1/triggers"},
		{http.MethodPut, "/v1/triggers/trg_1"},
		{http.MethodDelete, "/v1/triggers/trg_1"},
		{http.MethodPost, "/v1/triggers/trg_1/rotate-secret"},
		{http.MethodPost, "/v1/inventory-sources"},
		{http.MethodPut, "/v1/inventory-sources/src_1"},
		{http.MethodDelete, "/v1/inventory-sources/src_1"},
		{http.MethodPost, "/v1/inventory-sources/src_1/refresh"},
	}
	for testNum, w := range writes {
		t.Run(fmt.Sprintf("test %d %s %s", testNum, w.Method, w.Path), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(context.Background(), w.Method,
				"http://example.test"+w.Path, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if got := requiredRole(req); got != user.RoleAdmin {
				t.Errorf("%s %s needs %q, want admin: raising the read role must not lower the "+
					"write role", w.Method, w.Path, got)
			}
		})
	}
}

// TestHookBypassAndRedactionAgree pins that the test deciding a request is a public webhook is the
// same one deciding its token is redacted.
//
// They differed: the bypass compared the raw path and the redaction cleaned it first, so
// /hooks/<token>/../../probe read as public in one and as not-a-hook in the other. An
// unauthenticated stranger then appended a permanent hash-linked entry per probe, with the presented
// token written into it verbatim, in a record that travels to third parties.
func TestHookBypassAndRedactionAgree(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/hooks/whk_live",
		"/hooks/whk_live/../../probe",
		"/hooks/../x",
		"/v1/hooks/whk_live/..%2f..%2fprobe",
		"//hooks/whk_live/../probe",
	}
	for testNum, p := range paths {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				"http://example.test"+p, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			g := &authGate{}
			public := !g.protects(req)
			redacted := auditPath(req) != req.URL.Path
			if public != redacted {
				t.Errorf("%s is public=%v but redacted=%v; a request that skips the token gate "+
					"without redaction writes its own credential into the chain", p, public, redacted)
			}
		})
	}
}

// TestDerivedReadsRespectTheRunFilter checks that a view computed from runs shows only runs the
// caller may read.
//
// Fleet health, drift, task trends, host history, host facts, and the worker list are all derived
// from runs, and each returned the whole install to any viewer. The same caller got a 403 asking for
// one of those runs by name, so the boundary held on the direct route and leaked on every view built
// on top of it.
func TestDerivedReadsRespectTheRunFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	mine := &run.Run{
		ID: "run_mine", Playbook: "mine.yml", InventoryID: "inv_mine",
		Status: run.StatusSucceeded, CreatedAt: time.Now(),
	}
	theirs := &run.Run{
		ID: "run_theirs", Playbook: "theirs.yml", InventoryID: "inv_theirs",
		Status: run.StatusFailed, CreatedAt: time.Now(),
	}
	for _, r := range []*run.Run{mine, theirs} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	authz := &authorizer{
		strict: true,
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"inv_mine": {{Subject: "user_1", Access: grant.AccessUse}},
		}},
	}
	actorCtx := context.WithValue(ctx, actorKey{}, Actor{UserID: "user_1", Role: user.RoleViewer})

	keep, anyReadable, err := derivedReadFilter(actorCtx, authz, store)
	if err != nil {
		t.Fatalf("derivedReadFilter() error = %v", err)
	}
	if !anyReadable {
		t.Fatal("the caller can read their own run, so aggregates should not be withheld")
	}
	if !keep("run_mine") {
		t.Error("the caller's own run was filtered out of every derived view")
	}
	if keep("run_theirs") {
		t.Error("a run the caller is refused by name is shown in views derived from it")
	}

	// A caller granted nothing sees no aggregate at all, since an aggregate with no run id attached
	// is still a summary of work they may not know about.
	noneCtx := context.WithValue(ctx, actorKey{}, Actor{UserID: "user_none", Role: user.RoleViewer})
	if _, any, ferr := derivedReadFilter(noneCtx, authz, store); ferr != nil {
		t.Fatalf("derivedReadFilter() error = %v", ferr)
	} else if any {
		t.Error("a caller granted nothing is shown fleet-wide aggregates")
	}
}
