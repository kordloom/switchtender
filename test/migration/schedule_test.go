package migration

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// lastFridayClose are the fires of the fixture's AWX rule, "DTSTART;TZID=Europe/London:
// 20260130T170000 RRULE:FREQ=MONTHLY;BYDAY=-1FR": 17:00 London on the last Friday of every month.
// They are written out from the rule's definition rather than computed by any code under test.
// London keeps GMT in winter and BST, an hour ahead, from the last Sunday of March to the last
// Sunday of October, so the same wall clock lands on 17:00 or 16:00 UTC. The list crosses both
// transitions in each year, including 2026-10-30, five days after the clocks go back, and
// 2027-03-26, two days before they go forward.
var lastFridayClose = []string{
	"2026-10-30T17:00:00Z", "2026-11-27T17:00:00Z", "2026-12-25T17:00:00Z",
	"2027-01-29T17:00:00Z", "2027-02-26T17:00:00Z", "2027-03-26T17:00:00Z", "2027-04-30T16:00:00Z",
	"2027-05-28T16:00:00Z", "2027-06-25T16:00:00Z", "2027-07-30T16:00:00Z", "2027-08-27T16:00:00Z",
	"2027-09-24T16:00:00Z", "2027-10-29T16:00:00Z", "2027-11-26T17:00:00Z", "2027-12-31T17:00:00Z",
	"2028-01-28T17:00:00Z", "2028-02-25T17:00:00Z", "2028-03-31T16:00:00Z", "2028-04-28T16:00:00Z",
	"2028-05-26T16:00:00Z", "2028-06-30T16:00:00Z", "2028-07-28T16:00:00Z", "2028-08-25T16:00:00Z",
	"2028-09-29T16:00:00Z", "2028-10-27T16:00:00Z", "2028-11-24T17:00:00Z", "2028-12-29T17:00:00Z",
	"2029-01-26T17:00:00Z", "2029-02-23T17:00:00Z", "2029-03-30T16:00:00Z", "2029-04-27T16:00:00Z",
	"2029-05-25T16:00:00Z", "2029-06-29T16:00:00Z", "2029-07-27T16:00:00Z", "2029-08-31T16:00:00Z",
	"2029-09-28T16:00:00Z", "2029-10-26T16:00:00Z", "2029-11-30T17:00:00Z", "2029-12-28T17:00:00Z",
}

// scheduleRecord is the part of a schedule the scenario reads.
type scheduleRecord struct {
	// ID is the schedule's id.
	ID string `json:"id"`
	// Name is its name.
	Name string `json:"name"`
	// Cron is its cron expression, empty for a recurrence.
	Cron string `json:"cron"`
	// RRule is its recurrence.
	RRule string `json:"rrule"`
	// Timezone is the zone it is read in.
	Timezone string `json:"timezone"`
	// TemplateID is the template it fires.
	TemplateID string `json:"template_id"`
	// Enabled reports whether it fires.
	Enabled bool `json:"enabled"`
	// NextRunAt is when it fires next.
	NextRunAt time.Time `json:"next_run_at"`
}

// schedule reads the imported schedule called name.
func (in *install) schedule(s *server, name string) scheduleRecord {
	in.t.Helper()
	var page struct {
		// Schedules are the schedules.
		Schedules []scheduleRecord `json:"schedules"`
	}
	in.must(s, "admin", "GET", "/v1/schedules", nil, 200).decode(in.t, &page)
	for _, sc := range page.Schedules {
		if sc.Name == name {
			return sc
		}
	}
	in.t.Fatalf("no schedule named %q after the import", name)
	return scheduleRecord{}
}

// runsFrom lists the runs a source fired, by its id.
func (in *install) runsFrom(s *server, sourceID string) []runRecord {
	in.t.Helper()
	var page struct {
		// Runs are the runs, newest first.
		Runs []map[string]any `json:"runs"`
	}
	in.must(s, "admin", "GET", "/v1/runs?limit=200", nil, 200).decode(in.t, &page)
	var out []runRecord
	for _, r := range page.Runs {
		if str(r["source_id"]) == sourceID {
			out = append(out, runRecord{ID: str(r["id"]), Status: str(r["status"]), Raw: r})
		}
	}
	return out
}

// TestImportedRecurrenceFiresWhenAWXWouldAndOnlyOnce is scenario six. AWX exports two schedules
// whose rules no cron expression can say: the last Friday of every month in London time, and half
// past every minute. The first has to keep the rule and fire on exactly the instants the rule
// defines, across daylight saving changes. The second, enabled on two servers sharing one
// PostgreSQL database, has to fire each occurrence once, not once per scheduler.
func TestImportedRecurrenceFiresWhenAWXWouldAndOnlyOnce(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onPostgres})
	a := in.startServer("a", "--schedule-interval", "1s")
	b := in.startServer("b", "--schedule-interval", "1s")

	monthly := in.schedule(a, "last friday close")
	if monthly.Cron != "" || monthly.RRule == "" || monthly.Timezone != "Europe/London" {
		t.Fatalf("the last-Friday schedule = %+v, want its recurrence kept in London time", monthly)
	}
	now := time.Now().UTC()
	var want []string
	for _, at := range lastFridayClose {
		if fire, _ := time.Parse(time.RFC3339, at); fire.After(now.Add(time.Minute)) {
			want = append(want, at)
		}
	}
	if len(want) < 5 {
		t.Fatalf("fewer than five expected fires remain after %s: extend lastFridayClose from the "+
			"rule's definition", now.Format(time.RFC3339))
	}
	want = want[:5]
	var preview struct {
		// Next are the next fires.
		Next []time.Time `json:"next"`
	}
	q := url.Values{"rrule": {monthly.RRule}, "timezone": {monthly.Timezone}}
	in.must(b, "operator", "GET", "/v1/schedules/preview?"+q.Encode(), nil, 200).decode(t, &preview)
	got := make([]string, 0, len(preview.Next))
	for _, fire := range preview.Next {
		got = append(got, fire.UTC().Format(time.RFC3339))
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the imported rule's next fires (-want +got):\n%s", diff)
	}
	if got := monthly.NextRunAt.UTC().Format(time.RFC3339); got != want[0] {
		t.Errorf("the imported schedule fires next at %s, want %s", got, want[0])
	}

	beat := in.schedule(a, "half past every minute")
	if beat.Enabled || beat.Cron != "" {
		t.Fatalf("the minutely schedule = %+v, want its recurrence, switched off as AWX had it", beat)
	}
	enable := func(on bool) {
		in.must(a, "admin", "PUT", "/v1/schedules/"+beat.ID, map[string]any{
			"name": beat.Name, "cron": "", "rrule": beat.RRule, "timezone": beat.Timezone,
			"template_id": beat.TemplateID, "playbook": "", "inventory": "", "enabled": on,
		}, 200)
	}
	enable(true)
	deadline := time.Now().Add(waitLimit)
	var fired []runRecord
	for time.Now().Before(deadline) && len(fired) == 0 {
		fired = in.runsFrom(b, beat.ID)
		time.Sleep(500 * time.Millisecond)
	}
	if len(fired) == 0 {
		t.Fatalf("the minutely schedule never fired")
	}
	// Both schedulers tick every second, so a second fire of the same occurrence lands within a
	// few seconds of the first. Waiting past that window, and stopping short of the next
	// occurrence, is what makes the count a measurement.
	created, err := time.Parse(time.RFC3339Nano, str(fired[0].Raw["created_at"]))
	if err != nil {
		t.Fatalf("read when the schedule fired: %v", err)
	}
	time.Sleep(time.Until(created.Add(15 * time.Second)))
	enable(false)
	fired = in.runsFrom(a, beat.ID)
	if len(fired) != 1 {
		raw, _ := json.Marshal(fired)
		t.Fatalf("one occurrence fired %d runs across two schedulers, want one: %s", len(fired), raw)
	}
	if sec := created.UTC().Second(); sec < 30 || sec > 33 {
		t.Errorf("the occurrence at half past fired at second %d", sec)
	}
	done := in.waitDone(a, fired[0].ID)
	if done.Status != "succeeded" {
		t.Fatalf("the scheduled run = %s: %s", done.Status, describe(done.Raw))
	}
	if diff := cmp.Diff([]string{"web1"}, in.marked("heartbeat")); diff != "" {
		t.Errorf("hosts the scheduled run reached (-want +got):\n%s", diff)
	}

	ev := in.checkEvidence(b, done.ID)
	rec := ev.Receipts[done.ID]
	requireRecord(t, rec, recordWant{Playbook: "mark.yml", Hosts: []string{"web1"}})
	if c := rec.launch(t); c.ActorType != "system" && c.ActorType != "schedule" {
		t.Errorf("the scheduled run's receipt starts with %s %s by %s (%s), want the scheduler",
			c.Method, c.Path, c.Actor, c.ActorType)
	}
}
