package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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

// TestFilterHostHealthKeepsOutcomesPairedWithTheirRuns pins that narrowing a host's health to the
// runs a caller may read leaves the two index-parallel slices aligned.
//
// Recent and RecentRuns are paired by contract: entry i is the outcome of run i, and the host page
// draws its sparkline by walking them together so every tick links to the run that produced it. The
// ids were compacted and the outcomes left whole, so the ticks drew the full history against
// whichever ids survived and a tick labeled failed linked to a run that succeeded, on the page whose
// entire job is pointing at the run that broke. Failures, total, flips, and the last outcome were
// worse still, since they kept describing runs the caller is refused by name.
func TestFilterHostHealthKeepsOutcomesPairedWithTheirRuns(t *testing.T) {
	t.Parallel()
	// Newest first, the order the store returns: a failure the caller cannot read, then two runs it
	// can. A correct filter leaves two outcomes, both "ok", paired with their own runs.
	h := run.HostHealth{
		Host:        "web01",
		Recent:      []string{"failed", "ok", "ok"},
		RecentRuns:  []string{"run_hidden", "run_seen", "run_older"},
		Failures:    1,
		Total:       3,
		Flips:       1,
		Flaky:       false,
		LastOutcome: "failed",
	}
	keep := func(id string) bool { return id != "run_hidden" }

	got, ok := filterHostHealth(h, keep)
	if !ok {
		t.Fatal("a host with readable runs was dropped entirely")
	}
	if len(got.Recent) != len(got.RecentRuns) {
		t.Fatalf("%d outcomes against %d runs: the sparkline pairs them by index, so a tick carries "+
			"another run's result", len(got.Recent), len(got.RecentRuns))
	}
	if want := []string{"run_seen", "run_older"}; !slices.Equal(got.RecentRuns, want) {
		t.Fatalf("visible runs = %v, want %v", got.RecentRuns, want)
	}
	if want := []string{"ok", "ok"}; !slices.Equal(got.Recent, want) {
		t.Errorf("outcomes = %v, want %v: the readable runs are shown with the hidden run's result",
			got.Recent, want)
	}
	if got.Failures != 0 || got.Total != 2 {
		t.Errorf("failures/total = %d/%d, want 0/2: the counts still describe a run this caller is "+
			"refused by name", got.Failures, got.Total)
	}
	if got.LastOutcome != "ok" {
		t.Errorf("last outcome = %q, want ok: it names a run the caller cannot read", got.LastOutcome)
	}
	if got.Flips != 0 || got.Flaky {
		t.Errorf("flips/flaky = %d/%v, want 0/false: the host reads flaky on the strength of a run "+
			"the caller cannot see", got.Flips, got.Flaky)
	}

	// A host whose runs are all hidden is not shown at all.
	if _, ok := filterHostHealth(h, func(string) bool { return false }); ok {
		t.Error("a host with no readable runs was still shown")
	}

	// History recorded before run ids were tracked carries none, and is left as it stands.
	bare := run.HostHealth{Host: "old01", Recent: []string{"ok"}, Total: 1}
	if kept, ok := filterHostHealth(bare, func(string) bool { return false }); !ok || kept.Total != 1 {
		t.Error("a host recorded before run ids were tracked was emptied or dropped")
	}
}
