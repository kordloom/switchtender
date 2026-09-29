package main

import (
	"fmt"
	"time"
)

// checkAScheduleFiresOnItsOwn proves the deployed install turns a cadence into a run with nobody
// asking twice.
//
// Every other run this suite makes is submitted by the harness, so the scheduler, the one component
// whose whole job is to act when nobody is watching, was never exercised against a real install. It
// is covered heavily in unit tests, and that is the coverage that does not catch a deployment: the
// timezone field passed every unit test it had while every zone but UTC was refused in the image we
// publish, because the image carried no zone database and nothing ever asked it to resolve one.
//
// A cadence that never fires is the quietest failure this product can have. There is no error
// anywhere, no run to look at, and the operator finds out when something that should have happened
// nightly turns out not to have happened for a month.
func (h *harness) checkAScheduleFiresOnItsOwn(phase string) {
	const claim = "a schedule fires on its own and its run names the schedule that fired it"
	var created map[string]any
	if err := h.apiCall("POST", "/v1/schedules", &h.human, map[string]any{
		"name": "supertest cadence",
		"cron": "@every 20s",
		"steps": []map[string]any{
			{"name": "tick", "tool": "bash", "command": "echo scheduled"},
		},
	}, &created); err != nil {
		h.fail(phase, claim, err)
		return
	}
	scheduleID, _ := created["id"].(string)
	if scheduleID == "" {
		h.fail(phase, claim, fmt.Errorf("%w: the schedule", ErrNoID))
		return
	}
	// Removed however this ends. A cadence left behind fires every twenty seconds for the rest of
	// the suite, and the phases after this one count runs and chain entries and compare two installs
	// against each other.
	defer func() {
		if err := h.apiCall("DELETE", "/v1/schedules/"+scheduleID, &h.human, nil, nil); err != nil {
			h.fail(phase, "the cadence is removed once it has been proved", err)
		}
	}()

	// The install says when it will next act. Without that the row is inert and nothing downstream
	// would ever look at it again, which is the same silence as a scheduler that is not running.
	if next, _ := created["next_run_at"].(string); next == "" {
		h.fail(phase, claim, fmt.Errorf("%w: the schedule was created and names no next run, so "+
			"nothing will ever pick it up", ErrNothingRead))
		return
	}

	var fired map[string]any
	err := h.waitFor("a run fired by the schedule", 120*time.Second, func() error {
		var listed struct {
			Runs []map[string]any `json:"runs"`
		}
		if err := h.apiCall("GET", "/v1/runs?limit=50", &h.human, nil, &listed); err != nil {
			return err
		}
		for _, r := range listed.Runs {
			source, _ := r["source"].(string)
			sourceID, _ := r["source_id"].(string)
			if source == "schedule" && sourceID == scheduleID {
				fired = r
				return nil
			}
		}
		return fmt.Errorf("no run yet names schedule %s as what fired it", scheduleID)
	})
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	firedID, _ := fired["id"].(string)
	if firedID == "" {
		h.fail(phase, claim, fmt.Errorf("%w: the run the schedule fired", ErrNoID))
		return
	}
	// Attribution is half the claim. A run that appears without naming what fired it is a run an
	// auditor cannot tie to the cadence that was approved, and the provenance field exists for that.
	if _, err := h.awaitRunStatus(firedID, "succeeded", 120*time.Second); err != nil {
		h.fail(phase, claim, err)
		return
	}
	h.pass(phase, claim, fmt.Sprintf("schedule %s fired run %s, which reached succeeded with "+
		"nobody submitting it", scheduleID, firedID))
}
