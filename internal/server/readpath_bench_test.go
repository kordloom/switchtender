package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// benchRowCounts are the row counts every read-path benchmark is measured at. Four points an order
// of magnitude apart are what make the shape of the curve readable: a handler that costs ten times
// more for ten times the rows is linear and fine, and one that costs a hundred times more is the
// finding.
var benchRowCounts = []int{10, 100, 1000, 10000}

// benchLogger is a discarding logger, so no benchmark measures log formatting.
func benchLogger() *zap.Logger { return zap.NewNop() }

// benchRun builds one realistic run row: it names a project, an inventory, and a credential, which
// is what the read filter has to resolve per row, and carries the fields the list view serializes.
func benchRun(i int) *run.Run {
	at := time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(i) * time.Second)
	end := at.Add(90 * time.Second)
	return &run.Run{
		ID:            fmt.Sprintf("run_%06d", i),
		Playbook:      "site.yml",
		Inventory:     "production",
		Tool:          "ansible",
		Status:        run.StatusSucceeded,
		CreatedAt:     at,
		StartedAt:     &at,
		EndedAt:       &end,
		Actor:         "operator@example.test",
		ActorType:     "human",
		Source:        "api",
		ProjectID:     fmt.Sprintf("proj_%d", i%8),
		InventoryID:   fmt.Sprintf("inv_%d", i%4),
		CredentialIDs: []string{fmt.Sprintf("cred_%d", i%16)},
		Labels:        map[string]string{"env": "prod", "team": "platform"},
	}
}

// benchRunStore returns an in-memory run store holding n runs, each with one host summary so the
// fleet, drift, and host views have rows to derive from.
//
// The summary is written while the run is still pending and the run is settled afterward, because
// the store fences a terminal run's summaries. Writing the summary last stored nothing at all and
// the fleet benchmarks measured an empty answer.
func benchRunStore(b *testing.B, n int) run.Store {
	b.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	for i := range n {
		rn := benchRun(i)
		final := rn.Status
		rn.Status = run.StatusRunning
		// Every third run is a check, so the drift view has rows to filter.
		rn.DryRun = i%3 == 0
		if err := store.Save(ctx, rn); err != nil {
			b.Fatalf("save run: %v", err)
		}
		summary := run.HostSummary{
			Host: fmt.Sprintf("host-%04d", i%256), OK: 12, Changed: i % 3,
			Worst: "ok", DurationSeconds: 4.5, RanAt: rn.CreatedAt,
		}
		if err := store.SaveHostSummary(ctx, rn.ID, []run.HostSummary{summary}); err != nil {
			b.Fatalf("save host summary: %v", err)
		}
		rn.Status = final
		if err := store.Save(ctx, rn); err != nil {
			b.Fatalf("settle run: %v", err)
		}
	}
	return store
}

// benchAuditStore returns an in-memory audit store holding n chained entries.
func benchAuditStore(b *testing.B, n int) audit.Store {
	b.Helper()
	ctx := context.Background()
	store := audit.NewMemStore()
	at := time.Unix(1_700_000_000, 0).UTC()
	for i := range n {
		entry := &audit.Entry{
			ID: fmt.Sprintf("aud_%06d", i), At: at.Add(time.Duration(i) * time.Second),
			Actor: "operator@example.test", ActorType: "human",
			Method: http.MethodPost, Path: "/v1/runs",
		}
		if err := store.Append(ctx, entry); err != nil {
			b.Fatalf("append audit entry: %v", err)
		}
	}
	return store
}

// benchOpenAuthz returns an authorizer with grants disabled, the default deployment, where every
// read filter keeps everything.
func benchOpenAuthz() *authorizer { return &authorizer{} }

// benchStrictAuthz returns a strict-grants authorizer holding grants many, plus the grants a
// non-admin actor needs to read every benchmark run. Strict grants are the expensive path: the read
// filter is rebuilt from the whole grant table rather than short-circuited.
func benchStrictAuthz(b *testing.B, extra int) *authorizer {
	b.Helper()
	ctx := context.Background()
	grants := grant.NewMemStore()
	save := func(id, subject, object string, access grant.Access) {
		g := &grant.Grant{ID: id, Subject: subject, Object: object, Access: access}
		if err := grants.Save(ctx, g); err != nil {
			b.Fatalf("save grant: %v", err)
		}
	}
	for i := range 8 {
		save(fmt.Sprintf("gr_proj_%d", i), "user_bench", fmt.Sprintf("proj_%d", i), grant.AccessUse)
	}
	for i := range 4 {
		save(fmt.Sprintf("gr_inv_%d", i), "user_bench", fmt.Sprintf("inv_%d", i), grant.AccessUse)
	}
	for i := range 16 {
		save(fmt.Sprintf("gr_cred_%d", i), "user_bench", fmt.Sprintf("cred_%d", i), grant.AccessUse)
	}
	// Grants belonging to other subjects, which the filter still has to read and reject on every
	// rebuild. A real install accumulates these per project, per team, and per tenant.
	for i := range extra {
		save(fmt.Sprintf("gr_other_%d", i), fmt.Sprintf("user_other_%d", i),
			fmt.Sprintf("proj_other_%d", i), grant.AccessUse)
	}
	return &authorizer{grants: grants, strict: true}
}

// benchActorRequest returns a GET request for target carrying a non-admin actor, so the strict
// benchmarks exercise the filtering path rather than the admin bypass.
func benchActorRequest(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	actor := Actor{UserID: "user_bench", Role: user.RoleOperator, Name: "bench", Type: "token"}
	return r.WithContext(context.WithValue(r.Context(), actorKey{}, actor))
}

// serveBench drives handler once per iteration and fails the benchmark on any non-200, so a
// benchmark can never report the cost of an error page as the cost of the read.
func serveBench(b *testing.B, handler http.HandlerFunc, req *http.Request) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
	}
}

// BenchmarkListRunsOpen measures GET /v1/runs with grants disabled, one page of runs out of a store
// holding n. The page size is fixed, so the cost per request should be flat as n grows.
func BenchmarkListRunsOpen(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := listRunsHandler(store, benchOpenAuthz(), benchLogger())
			serveBench(b, handler, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
		})
	}
}

// BenchmarkListRunsStrict measures the same page under strict grants, where every row on the page
// is resolved against the grant table.
func BenchmarkListRunsStrict(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := listRunsHandler(store, benchStrictAuthz(b, 100), benchLogger())
			serveBench(b, handler, benchActorRequest("/v1/runs"))
		})
	}
}

// BenchmarkGetRun measures GET /v1/runs/{id}, the run detail read, against stores of growing size.
// One row is returned whatever n is, so anything but a flat curve is the store's lookup, not the
// handler's.
func BenchmarkGetRun(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := getRunHandler(store, benchOpenAuthz(), benchLogger())
			req := httptest.NewRequest(http.MethodGet, "/v1/runs/x", nil)
			req.SetPathValue("id", fmt.Sprintf("run_%06d", n/2))
			serveBench(b, handler, req)
		})
	}
}

// BenchmarkGetRunStrict measures the run detail read under strict grants, where the run's project,
// inventory, and credentials are each authorized before it is returned.
func BenchmarkGetRunStrict(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := getRunHandler(store, benchStrictAuthz(b, 100), benchLogger())
			req := benchActorRequest("/v1/runs/x")
			req.SetPathValue("id", fmt.Sprintf("run_%06d", n/2))
			serveBench(b, handler, req)
		})
	}
}

// BenchmarkAuditPage measures GET /v1/audit, which returns a fixed hundred-entry page however long
// the chain is.
func BenchmarkAuditPage(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			store := benchAuditStore(b, n)
			handler := auditHandler(store, benchLogger())
			serveBench(b, handler, httptest.NewRequest(http.MethodGet, "/v1/audit", nil))
		})
	}
}

// BenchmarkFleetOpen measures GET /v1/fleet with grants disabled.
func BenchmarkFleetOpen(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := fleetHandler(store, benchOpenAuthz(), benchLogger())
			serveBench(b, handler, httptest.NewRequest(http.MethodGet, "/v1/fleet", nil))
		})
	}
}

// BenchmarkFleetStrict measures GET /v1/fleet under strict grants, where every host row's governing
// runs are checked against the grant table. This is the read path that decides what a tenant sees,
// so its shape decides whether the fleet view survives a busy install.
func BenchmarkFleetStrict(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := fleetHandler(store, benchStrictAuthz(b, 100), benchLogger())
			serveBench(b, handler, benchActorRequest("/v1/fleet"))
		})
	}
}

// BenchmarkDriftStrict measures GET /v1/drift under strict grants, the same per-row check over the
// drift rows.
func BenchmarkDriftStrict(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := driftHandler(store, benchStrictAuthz(b, 100), benchLogger())
			serveBench(b, handler, benchActorRequest("/v1/drift"))
		})
	}
}

// BenchmarkHostHistoryStrict measures GET /v1/hosts/{host}/runs under strict grants, the per-host
// page the fleet view links into.
func BenchmarkHostHistoryStrict(b *testing.B) {
	for _, n := range benchRowCounts {
		b.Run(fmt.Sprintf("rows=%d", n), func(b *testing.B) {
			store := benchRunStore(b, n)
			handler := hostHistoryHandler(store, benchStrictAuthz(b, 100), benchLogger())
			req := benchActorRequest("/v1/hosts/host-0001/runs?limit=200")
			req.SetPathValue("host", "host-0001")
			serveBench(b, handler, req)
		})
	}
}

// BenchmarkStrictGrantTable measures how the strict-grants fleet read scales with the size of the
// grant table rather than the run count, since the read filter is built from every grant on the
// install. The run count is held fixed so only the grant table moves.
func BenchmarkStrictGrantTable(b *testing.B) {
	for _, grants := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("grants=%d", grants), func(b *testing.B) {
			store := benchRunStore(b, 1000)
			handler := fleetHandler(store, benchStrictAuthz(b, grants), benchLogger())
			serveBench(b, handler, benchActorRequest("/v1/fleet"))
		})
	}
}
