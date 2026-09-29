package importer

import (
	"strings"
	"testing"
	"time"
)

// TestNothingSwitchedOffAtTheSourceArrivesArmed is one rule over every format that has an off switch.
//
// Two importers got this wrong in opposite directions and both looked fine in the summary. Semaphore
// hardcoded a schedule as enabled, so one somebody had deactivated came across live and started firing
// a template on a cadence its own estate had turned off. Rundeck went the other way and left a paused
// schedule behind entirely, which is safe and loses the expression: turning the job back on a year
// later means reconstructing the cadence from memory.
//
// So there is one policy. A schedule disabled at the source comes across, and comes across disabled.
// Never armed, because that runs something nobody asked to run, and never absent, because that hides a
// migration loss inside a clean report.
func TestNothingSwitchedOffAtTheSourceArrivesArmed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Name is the format and what in it is switched off.
		Name string
		// From parses the export.
		From func([]byte, time.Time) (*Plan, error)
		// Doc holds one schedule, switched off at the source.
		Doc string
	}{{
		Name: "awx schedule with enabled false",
		From: FromAWX,
		Doc: `{"projects":[{"name":"web","scm_type":"git","scm_url":"https://example.invalid/w.git"}],` +
			`"job_templates":[{"name":"Deploy","playbook":"site.yml","project":"web","related":{"schedules":[` +
			`{"name":"nightly","rrule":"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1",` +
			`"enabled":false}]}}]}`,
	}, {
		Name: "semaphore schedule with active false",
		From: FromSemaphore,
		Doc: `{"repositories":[{"name":"web","git_url":"https://example.invalid/w.git"}],` +
			`"templates":[{"name":"Deploy","playbook":"site.yml","repository":"web"}],` +
			`"schedules":[{"name":"nightly","cron_format":"0 2 * * *","template":"Deploy",` +
			`"active":false}]}`,
	}, {
		Name: "rundeck schedule with scheduleEnabled false",
		From: FromRundeck("prod"),
		Doc: "- name: Deploy\n  scheduleEnabled: false\n  schedule:\n    crontab: \"0 0 2 * * ?\"\n" +
			"  sequence:\n    commands:\n      - exec: /usr/bin/deploy\n",
	}, {
		Name: "jenkins job disabled with a timer trigger",
		From: FromJenkins("prod"),
		Doc: `<project><disabled>true</disabled>` +
			`<triggers><hudson.triggers.TimerTrigger><spec>0 2 * * *</spec>` +
			`</hudson.triggers.TimerTrigger></triggers><builders>` +
			`<hudson.tasks.Shell><command>/usr/bin/deploy</command></hudson.tasks.Shell>` +
			`</builders></project>`,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan, err := test.From([]byte(test.Doc), now)
			if err != nil {
				t.Fatalf("the export does not import, so this case tests nothing: %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("imported %d schedule(s), want the one the export holds carried rather than "+
					"dropped. A cadence left behind is a migration loss inside a clean report: %v",
					len(plan.Schedules), plan.Warnings)
			}
			got := plan.Schedules[0]
			if got.Enabled {
				t.Error("a schedule switched off at the source imported armed. This is the import " +
					"error that does something rather than failing to: work the estate had stopped " +
					"starts again on its own, and nobody is watching a job they turned off.")
			}
			if got.Cron == "" {
				t.Error("the schedule carries no expression, so carrying it gained nothing over " +
					"dropping it")
			}
			if !strings.Contains(strings.Join(plan.Warnings, "\n"), "switched off at the source") {
				t.Errorf("nothing said the schedule arrives switched off, so a count of schedules "+
					"reads as a count of cadences that fire: %v", plan.Warnings)
			}
		})
	}
}

// TestADisabledAWXHostIsNotImportedAsALiveTarget covers the one finding in this class that reaches a
// machine rather than a report.
//
// AWX keeps a disabled host in its inventory and runs nothing against it. The field was not on the
// struct at all, so it could not be read, and the host came across in the INI as an ordinary member.
// The next play targeting all then reached a machine the estate had deliberately held back, and the
// person who disabled it had no reason to be looking.
//
// The host is left out rather than carried and marked, because INI inventory content has no off switch
// for a host, and the warning names what went so that a host disappearing is not itself a surprise.
func TestADisabledAWXHostIsNotImportedAsALiveTarget(t *testing.T) {
	t.Parallel()
	const export = `{
	  "inventories": [{
	    "name": "prod",
	    "hosts": [
	      {"name": "web1.prod"},
	      {"name": "decommissioning.prod", "enabled": false}
	    ],
	    "groups": [{
	      "name": "db",
	      "hosts": [{"name": "db1.prod"}, {"name": "quarantined.prod", "enabled": false}]
	    }]
	  }],
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.invalid/w.git"}],
	  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "project": "web",
	    "inventory": "prod"}]
	}`
	plan, err := FromAWX([]byte(export), time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Inventories) != 1 {
		t.Fatalf("inventories = %d, want 1", len(plan.Inventories))
	}
	content := plan.Inventories[0].Content
	for _, live := range []string{"web1.prod", "db1.prod"} {
		if !strings.Contains(content, live) {
			t.Errorf("the enabled host %q is missing from the inventory, so this refuses more than "+
				"it should:\n%s", live, content)
		}
	}
	for _, off := range []string{"decommissioning.prod", "quarantined.prod"} {
		if strings.Contains(content, off) {
			t.Errorf("the host %q is disabled in AWX and is in the imported inventory, so a play "+
				"targeting all reaches a machine AWX runs nothing against:\n%s", off, content)
		}
	}
	warnings := strings.Join(plan.Warnings, "\n")
	for _, off := range []string{"decommissioning.prod", "quarantined.prod"} {
		if !strings.Contains(warnings, off) {
			t.Errorf("the host %q was left out with nothing said about it, which is its own kind of "+
				"wrong: %v", off, plan.Warnings)
		}
	}
	if !strings.Contains(warnings, `group "db"`) {
		t.Errorf("the warning does not say which group the host came out of, so an operator holding "+
			"forty groups cannot find it: %v", plan.Warnings)
	}
}
