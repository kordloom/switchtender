package migration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// skipRecord is the part of a schedule the skip scenario reads.
type skipRecord struct {
	// ID is the schedule's id.
	ID string `json:"id"`
	// Name is its name.
	Name string `json:"name"`
	// LastSkip is why its last fire was skipped.
	LastSkip string `json:"last_skip"`
	// SkippedFires counts its skips in a row.
	SkippedFires int `json:"skipped_fires"`
	// LastError is why its last fire failed.
	LastError string `json:"last_error"`
}

// skipState reads the schedule called name.
func (in *install) skipState(s *server, name string) skipRecord {
	in.t.Helper()
	var page struct {
		// Schedules are the schedules.
		Schedules []skipRecord `json:"schedules"`
	}
	in.must(s, "admin", "GET", "/v1/schedules", nil, 200).decode(in.t, &page)
	for _, sc := range page.Schedules {
		if sc.Name == name {
			return sc
		}
	}
	in.t.Fatalf("no schedule named %q", name)
	return skipRecord{}
}

// TestComposedInventoryThatMatchesNothingIsRefusedOrSkipped is the zero-host scenario. An imported
// template is pointed at a smart inventory whose filter matches no host. A person launching it is
// refused with a link to the inventory's host preview. A schedule firing it every ten seconds
// records each fire as skipped, never as failed, starts no run, and is flagged by the doctor once
// three fires in a row have been skipped. Each skip is its own chain entry after its fire's, and
// the exported chain still verifies offline.
func TestComposedInventoryThatMatchesNothingIsRefusedOrSkipped(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a", "--schedule-interval", "1s")

	var inv struct {
		// ID is the new inventory's id.
		ID string `json:"id"`
	}
	in.must(s, "admin", "POST", "/v1/inventories", map[string]any{
		"name": "nobody home", "kind": "smart", "host_filter": "name=no-such-host",
	}, 201).decode(t, &inv)
	in.updateTemplate(s, "smart mark", func(tpl map[string]any) { tpl["inventory_id"] = inv.ID })
	preview := "/ui/inventories?preview=" + inv.ID

	refused := in.launch(s, "operator", "smart mark", nil)
	var answer struct {
		// Error is the refusal.
		Error string `json:"error"`
		// PreviewURL is the host preview it links to.
		PreviewURL string `json:"preview_url"`
	}
	refused.decode(t, &answer)
	if refused.Status != 400 || answer.PreviewURL != preview ||
		!strings.HasSuffix(answer.Error, preview) {
		t.Fatalf("a launch that matches no hosts = %d %s, want 400 linking to %s", refused.Status,
			refused.Body, preview)
	}

	var sched struct {
		// ID is the new schedule's id.
		ID string `json:"id"`
	}
	in.must(s, "admin", "POST", "/v1/schedules", map[string]any{
		"name": "nobody nightly", "template_id": in.template(s, "smart mark"),
		"rrule": "DTSTART:20260101T000000Z\nRRULE:FREQ=MINUTELY;BYSECOND=0,10,20,30,40,50",
	}, 201).decode(t, &sched)

	deadline := time.Now().Add(waitLimit)
	var got skipRecord
	for time.Now().Before(deadline) {
		if got = in.skipState(s, "nobody nightly"); got.SkippedFires >= 3 {
			break
		}
		time.Sleep(time.Second)
	}
	in.must(s, "admin", "PUT", "/v1/schedules/"+sched.ID, map[string]any{
		"name": "nobody nightly", "template_id": in.template(s, "smart mark"), "enabled": false,
		"rrule": "DTSTART:20260101T000000Z\nRRULE:FREQ=MINUTELY;BYSECOND=0,10,20,30,40,50",
	}, 200)
	if got.SkippedFires < 3 || got.LastSkip != "no hosts matched" || got.LastError != "" {
		t.Fatalf("the schedule after its fires = %+v, want three or more skips and no failure", got)
	}
	if runs := in.runsFrom(s, sched.ID); len(runs) != 0 {
		raw, _ := json.Marshal(runs)
		t.Fatalf("a schedule whose inventory matches nothing started runs: %s", raw)
	}

	var doctor struct {
		// Findings are the doctor's findings.
		Findings []struct {
			// ObjectID is the object the finding is about.
			ObjectID string `json:"object_id"`
			// FixPath is where it is fixed.
			FixPath string `json:"fix_path"`
		} `json:"findings"`
	}
	in.must(s, "admin", "GET", "/v1/doctor", nil, 200).decode(t, &doctor)
	flagged := false
	for _, f := range doctor.Findings {
		flagged = flagged || (f.ObjectID == sched.ID && f.FixPath == preview)
	}
	if !flagged {
		t.Errorf("the doctor does not flag the schedule that keeps matching no hosts: %+v",
			doctor.Findings)
	}

	ev := in.checkEvidence(s)
	fired := ev.entries("/schedules/" + sched.ID + "/fired")
	skipped := ev.entries("/schedules/" + sched.ID + "/skipped")
	if len(skipped) < 3 || len(fired) < len(skipped) {
		t.Fatalf("the chain holds %d fires and %d skips of the schedule, want a fire before each of "+
			"three or more skips", len(fired), len(skipped))
	}
	for _, e := range skipped {
		if e.Method != "SCHEDULE" || e.ActorType != "system" {
			t.Errorf("skip entry %d = %s by %s, want SCHEDULE by the scheduler", e.Seq, e.Method,
				e.ActorType)
		}
	}
}
