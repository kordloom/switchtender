package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// recordingAudits records appended entries and, at append time, snapshots the submitter's last run,
// so a test can prove the audit entry is written before the run launches. Append fails when err is
// set, standing in for an unhealthy store.
type recordingAudits struct {
	// entries holds every appended entry in order.
	entries []*audit.Entry
	// sub is the submitter whose launch state is snapshotted at append time.
	sub *fakeSubmitter
	// runAtAppend is the submitter's probe run as it stood when the entry was appended.
	runAtAppend *run.Run
	// err, when set, fails every append.
	err error
}

// Append records the entry or fails, snapshotting the submitter first.
func (a *recordingAudits) Append(_ context.Context, e *audit.Entry) error {
	if a.err != nil {
		return a.err
	}
	if a.sub != nil {
		a.runAtAppend = a.sub.gotRun
	}
	cp := *e
	a.entries = append(a.entries, &cp)
	return nil
}

// AppendSpanBeat is unused by the hook path.
func (a *recordingAudits) AppendSpanBeat(context.Context, time.Time, int) (*audit.Entry, error) {
	return nil, nil
}

// SpanBeats returns nothing.
func (a *recordingAudits) SpanBeats(context.Context, int) ([]*audit.Entry, error) { return nil, nil }

// List returns nothing.
func (a *recordingAudits) List(context.Context, int) ([]*audit.Entry, error) { return nil, nil }

// Chain returns nothing.
func (a *recordingAudits) Chain(context.Context) ([]*audit.Entry, error) { return nil, nil }

// ChainScan streams nothing.
func (a *recordingAudits) ChainScan(context.Context, int64, func(*audit.Entry) error) error {
	return nil
}

// newHookServer builds a handler wiring triggers, one template, a submitter, a run store, and an
// audit store, and seeds one unsigned trigger, returning its plaintext token and id.
func newHookServer(t *testing.T, store run.Store, sub Submitter, audits audit.Store) (http.Handler, string, string) {
	t.Helper()
	ctx := context.Background()
	triggers := trigger.NewMemStore()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{ID: "tpl_1", Name: "deploy", Playbook: "site.yml"}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	plain, tg, err := trigger.New("hookfire", "tpl_1")
	if err != nil {
		t.Fatalf("trigger.New: %v", err)
	}
	if err := triggers.Save(ctx, tg); err != nil {
		t.Fatalf("save trigger: %v", err)
	}
	handler := New(store, sub, zap.NewNop(),
		WithTriggers(triggers, credential.NewSealer("", "")),
		WithTemplates(templates), WithAudit(audits)).Handler()
	return handler, plain, tg.ID
}

// TestHookAuditBeforeLaunchFailClosed proves a webhook fire is recorded before it launches and is
// fail-closed: an unhealthy audit store refuses the fire and no run starts, matching the ordering the
// authenticated middleware uses for a mutation. Recording after the launch left a real run with no
// tamper-evident chain entry whenever the store was down.
func TestHookAuditBeforeLaunchFailClosed(t *testing.T) {
	t.Parallel()

	// An audit store that cannot append refuses the fire, and the run never launches.
	t.Run("append fails: no launch, 5xx", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
		audits := &recordingAudits{sub: sub, err: errors.New("disk full")}
		handler, token, _ := newHookServer(t, run.NewMemStore(), sub, audits)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+token, nil))

		if rec.Code < 500 {
			t.Errorf("status = %d, want a 5xx refusal when the fire cannot be recorded", rec.Code)
		}
		if sub.gotRun != nil {
			t.Errorf("a run launched despite the audit append failing: %+v", sub.gotRun)
		}
	})

	// A healthy store writes the entry before the run exists, and the entry carries no run id.
	t.Run("healthy: entry precedes the run", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
		audits := &recordingAudits{sub: sub}
		handler, token, tgID := newHookServer(t, run.NewMemStore(), sub, audits)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+token, nil))

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
		}
		if len(audits.entries) != 1 {
			t.Fatalf("appended %d entries, want exactly 1", len(audits.entries))
		}
		if audits.runAtAppend != nil {
			t.Errorf("the run already existed when the entry was appended: %+v", audits.runAtAppend)
		}
		if sub.gotRun == nil {
			t.Error("the run did not launch after the entry was recorded")
		}
		if want := "/hooks/" + tgID + "/fired"; audits.entries[0].Path != want {
			t.Errorf("entry path = %q, want %q (a pre-execution entry needs no run id)",
				audits.entries[0].Path, want)
		}
	})
}

// TestHookDedupeStableAcrossTime proves a redelivery carrying the same delivery id collapses onto
// the first run no matter how much later it arrives, rather than firing a second real run once the
// old ten-second time bucket had advanced.
func TestHookDedupeStableAcrossTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// First delivery: nothing exists yet, so it yields the key a fresh run must carry.
	existing, key, err := resolveHookDedupe(ctx, store, "tg_1", ":deliv", t0)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if existing != nil {
		t.Fatalf("first delivery resolved to an existing run: %+v", existing)
	}
	if key == "" {
		t.Fatal("first delivery returned an empty idempotency key")
	}
	// The launch stores the run under that key, as the dispatcher does.
	if err := store.Save(ctx, &run.Run{
		ID: "run_1", Status: run.StatusPending, IdempotencyKey: key, CreatedAt: t0,
	}); err != nil {
		t.Fatalf("save first run: %v", err)
	}

	// A redelivery of the same event 30 seconds later, well past the old bucket, must dedupe.
	again, key2, err := resolveHookDedupe(ctx, store, "tg_1", ":deliv", t0.Add(30*time.Second))
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if again == nil || again.ID != "run_1" {
		t.Fatalf("redelivery did not collapse onto the first run, got %+v: a replay outside the "+
			"time bucket fired a second run", again)
	}
	if key2 != key {
		t.Errorf("redelivery key = %q, want the stable key %q", key2, key)
	}
}
