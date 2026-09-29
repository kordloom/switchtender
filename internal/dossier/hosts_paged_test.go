package dossier

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// countingEvents records how the dossier reads a run's per-host outcomes, so a test can tell reading the
// stored summaries from folding the whole event stream.
type countingEvents struct {
	run.Store
	// wholeReads counts unpaged reads of the entire event stream.
	wholeReads int
	// pagedReads counts paged reads.
	pagedReads int
	// largestPage is the biggest number of events handed back at once.
	largestPage int
}

// Events records an unpaged read and serves it.
func (c *countingEvents) Events(ctx context.Context, id string) ([]event.Event, error) {
	c.wholeReads++
	return c.Store.Events(ctx, id)
}

// EventsAfter records a paged read and the size of the page it served.
func (c *countingEvents) EventsAfter(ctx context.Context, id string, after int64,
	limit int) ([]event.Event, error) {
	c.pagedReads++
	page, err := c.Store.EventsAfter(ctx, id, after, limit)
	if len(page) > c.largestPage {
		c.largestPage = len(page)
	}
	return page, err
}

// TestTheDossierDoesNotLoadEveryEvent covers the memory one evidence request could cost.
//
// A run across thousands of hosts carries hundreds of thousands of events, and this codebase measures
// unmarshaling them all at once in the hundreds of megabytes, which is why the run export was rewritten
// to page. The evidence document still read the whole stream to fold per-host outcomes, so a few
// concurrent evidence requests on a small control node could exhaust it, taking down the API and the runs
// it was in the middle of recording.
//
// The outcomes are already stored: whichever process executed the run folded them as its events streamed
// past and wrote them before the run went terminal. Reading those back is the answer, and a run that has
// none is folded a page at a time.
func TestTheDossierDoesNotLoadEveryEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("stored summaries are used", func(t *testing.T) {
		t.Parallel()
		base := run.NewMemStore()
		store := &countingEvents{Store: base}
		at := time.Now()
		seedRunWithEvents(t, base, "run_summarized", at, 5, true)

		in, err := Collect(ctx, store, audit.NewMemStore(), "", "run_summarized", at)
		if err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if len(in.Hosts) == 0 {
			t.Error("the dossier reports no hosts for a run that recorded them")
		}
		if store.wholeReads != 0 {
			t.Errorf("the dossier read the whole event stream %d time(s) for a run whose per-host "+
				"outcomes were already recorded", store.wholeReads)
		}
	})

	t.Run("a run with no summaries is folded a page at a time", func(t *testing.T) {
		t.Parallel()
		base := run.NewMemStore()
		store := &countingEvents{Store: base}
		at := time.Now()
		seedRunWithEvents(t, base, "run_unsummarized", at, 7, false)

		in, err := Collect(ctx, store, audit.NewMemStore(), "", "run_unsummarized", at)
		if err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if len(in.Hosts) == 0 {
			t.Error("a run with no stored summaries reports no hosts, so its evidence asserts by " +
				"omission that nothing ran")
		}
		if store.wholeReads != 0 {
			t.Errorf("the dossier read the whole event stream %d time(s) instead of paging",
				store.wholeReads)
		}
		if store.pagedReads == 0 {
			t.Error("the dossier folded no pages, so it did not rebuild the outcomes it reported")
		}
		if store.largestPage > eventPage {
			t.Errorf("a page held %d events, above the %d window, so the read is not actually bounded",
				store.largestPage, eventPage)
		}
	})
}

// seedRunWithEvents stores a finished run with events for hosts, optionally recording the per-host
// summaries the executor would have written.
func seedRunWithEvents(t *testing.T, store run.Store, id string, at time.Time, hosts int,
	summarize bool) {
	t.Helper()
	ctx := context.Background()
	r := &run.Run{
		ID: id, Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning, CreatedAt: at,
		StartedAt: &at,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	events := make([]event.Event, 0, hosts+1)
	summaries := make([]run.HostSummary, 0, hosts)
	stats := make(map[string]event.HostStats, hosts)
	for i := range hosts {
		host := fmt.Sprintf("web-%02d", i)
		events = append(events, event.Event{Type: event.TypeRunnerOK, Host: host, Task: "converge"})
		summaries = append(summaries, run.HostSummary{Host: host, OK: 1, Worst: "ok", RanAt: at})
		stats[host] = event.HostStats{OK: 1}
	}
	// The recap is what per-host outcomes are folded from, so a run whose summaries were never stored is
	// rebuilt from this.
	events = append(events, event.Event{Type: event.TypeStats, Stats: stats})
	if err := store.AppendEvents(ctx, id, events); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if summarize {
		if err := store.SaveHostSummary(ctx, id, summaries); err != nil {
			t.Fatalf("SaveHostSummary() error = %v", err)
		}
	}
	ended := at.Add(time.Minute)
	code := 0
	r.Status, r.EndedAt, r.ExitCode = run.StatusSucceeded, &ended, &code
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save(terminal) error = %v", err)
	}
}
