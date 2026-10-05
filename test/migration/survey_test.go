package migration

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestImportedSecretSurveyReachesThePlayAndNothingElse is scenario one. AWX exports a password
// survey question, and the import makes it a secret field. A launch's answer has to reach the play
// that asked for it, and nowhere else: not the run record, not its log, not the chain, the receipt,
// the dossier, an API answer, a server log, or the database, where it is held only sealed.
func TestImportedSecretSurveyReachesThePlayAndNothingElse(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	s := in.startServer("a")
	answer := "survey-answer-" + randomHex(t, 12)
	in.addSecret(answer)

	if r := in.launch(s, "operator", "rotate db password", nil); r.Status != 400 {
		t.Fatalf("a launch without the required secret answer = %d, want 400: %s", r.Status, r.Body)
	}
	rec := in.launched(s, "operator", "rotate db password",
		map[string]any{"answers": map[string]any{"db_password": answer}})
	done := in.waitDone(s, rec.ID)
	if done.Status != "succeeded" {
		t.Fatalf("the survey run = %s, want succeeded: %s", done.Status, describe(done.Raw))
	}

	sum := sha256.Sum256([]byte(answer))
	if got := strings.TrimSpace(in.marker("survey", "web1")); got != hex.EncodeToString(sum[:]) {
		t.Errorf("the play received an answer whose digest is %s, want the digest of the answer "+
			"the launch gave", got)
	}
	if diff := cmp.Diff([]string{"web1"}, in.marked("survey")); diff != "" {
		t.Errorf("hosts the survey run reached (-want +got):\n%s", diff)
	}
	if _, ok := done.Raw["extra_vars"].(map[string]any)["db_password"]; ok {
		t.Errorf("the run's extra_vars hold the secret variable: %v", done.Raw["extra_vars"])
	}
	if got := str(done.Raw["sealed_vars"]); !strings.Contains(got, "db_password") {
		t.Errorf("sealed_vars = %s, want the secret variable named so the record says an answer "+
			"was given", got)
	}
	logText := string(in.must(s, "admin", "GET", "/v1/runs/"+rec.ID+"/logs", nil, 200).Body)
	if !strings.Contains(logText, "rotating to") {
		t.Fatalf("the log lacks the task that prints the answer, so masking was not exercised:\n%s",
			logText)
	}

	ev := in.checkEvidence(s, rec.ID)
	got := ev.Receipts[rec.ID]
	requireRecord(t, got, recordWant{
		Launcher: "operator-laptop", OnBehalfOf: "operator", Playbook: "survey.yml",
		Hosts: []string{"web1"}, SealedVars: []string{"db_password"},
	})
}

// templateFields are the keys a template update accepts. A template read back carries more, its id
// and creation time among them, which a strict update refuses.
var templateFields = []string{
	"name", "project_id", "playbook", "inventory", "inventory_id", "tool", "command", "dry_run",
	"limit", "tags", "skip_tags", "verbosity", "forks", "diff_mode", "shards", "queue", "timeout",
	"image", "pull_credential_id", "credential_ids", "selectable_credential_ids", "extra_vars",
	"steps", "survey", "confirm_on_launch", "notifications", "org_id", "use_fact_cache",
	"fact_cache_timeout", "allow_callbacks",
}

// setSecretDefault gives the named template's secret survey question a default through the API, as
// an operator does in the template editor.
func (in *install) setSecretDefault(s *server, templateName, variable, def string) {
	in.t.Helper()
	id := in.template(s, templateName)
	var page struct {
		// Templates are every template, as the list serves them.
		Templates []map[string]any `json:"templates"`
	}
	in.must(s, "admin", "GET", "/v1/templates", nil, 200).decode(in.t, &page)
	var current map[string]any
	for _, tpl := range page.Templates {
		if tpl["id"] == id {
			current = tpl
		}
	}
	if current == nil {
		in.t.Fatalf("template %q is not in the list", templateName)
	}
	body := map[string]any{}
	for _, k := range templateFields {
		if v, ok := current[k]; ok {
			body[k] = v
		}
	}
	survey, _ := body["survey"].([]any)
	set := false
	for _, f := range survey {
		if q, ok := f.(map[string]any); ok && q["var"] == variable {
			q["default"] = def
			set = true
		}
	}
	if !set {
		in.t.Fatalf("template %q has no survey question %q", templateName, variable)
	}
	in.must(s, "admin", "PUT", "/v1/templates/"+id, body, 200)
}

// scheduleState is what a schedule says about its last fire.
type scheduleState struct {
	// LastError is why the last fire started no run.
	LastError string `json:"last_error"`
	// LastRunID is the run the last fire that started one created.
	LastRunID string `json:"last_run_id"`
}

// waitScheduleRefused waits until the schedule records a refusal containing want.
func (in *install) waitScheduleRefused(s *server, id, want string) scheduleState {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	var st scheduleState
	for time.Now().Before(deadline) {
		in.must(s, "admin", "GET", "/v1/schedules/"+id, nil, 200).decode(in.t, &st)
		if strings.Contains(st.LastError, want) {
			return st
		}
		time.Sleep(200 * time.Millisecond)
	}
	in.t.Fatalf("schedule %s never recorded a refusal naming %q, last error %q", id, want,
		st.LastError)
	return st
}

// waitScheduledRun waits for a run the schedule fired that is not one of seen, and returns it.
func (in *install) waitScheduledRun(s *server, id string, seen ...string) runRecord {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		for _, r := range in.runsFrom(s, id) {
			if !slices.Contains(seen, r.ID) {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	in.t.Fatalf("schedule %s fired no new run", id)
	return runRecord{}
}

// sealedColumn reads a run's stored sealed answers straight from the database, the way an
// attacker with write access to it would.
func (in *install) sealedColumn(db *sql.DB, runID string) string {
	in.t.Helper()
	var sealed string
	if err := db.QueryRow("SELECT sealed_vars FROM runs WHERE id = $1", runID).Scan(&sealed); err != nil {
		in.t.Fatalf("read the sealed answers of %s: %v", runID, err)
	}
	return sealed
}

// TestImportedSecretSurveyFiresOnScheduleWithItsSealedDefault is scenario one, fired by a schedule
// rather than a person. Nobody is there to answer the imported password question, so a schedule
// fires with its default or not at all.
//
// With no default the schedule refuses every fire, says why on the schedule, and records the
// refusal on the chain naming the question, and no run exists. Given a default in the template
// editor, the next fire carries it sealed: the run binds that exact ciphertext by digest, the
// approval of the run covers the digest, and the play receives the default. A later fire whose
// sealed answer is swapped in the database for another run's, under the same name, is approved
// all the same and still never opens it: the run fails saying so, and the play receives nothing.
// The whole chain and both receipts verify offline afterward, and neither the default nor the
// swapped answer appears in any record.
func TestImportedSecretSurveyFiresOnScheduleWithItsSealedDefault(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onPostgres, Policy: holdAnsible})
	s := in.startServer("a", "--schedule-interval", "1s")
	tplID := in.template(s, "rotate db password")
	db, err := sql.Open("pgx", in.db)
	if err != nil {
		t.Fatalf("open the scenario database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var created struct {
		// ID is the schedule's id.
		ID string `json:"id"`
	}
	in.must(s, "admin", "POST", "/v1/schedules", map[string]any{
		"name": "rotate on schedule", "cron": "@every 2s", "template_id": tplID, "playbook": "",
		"inventory": "", "enabled": true,
	}, 201).decode(t, &created)
	sched := created.ID

	refused := in.waitScheduleRefused(s, sched, `"db_password" is required and has no default`)
	if !strings.Contains(refused.LastError, "scheduled fire") || refused.LastRunID != "" {
		t.Errorf("schedule state = %+v, want a scheduled fire refused and no run", refused)
	}
	if runs := in.runsFrom(s, sched); len(runs) != 0 {
		t.Fatalf("a schedule nobody could answer started %d runs", len(runs))
	}

	def := "scheduled-default-" + randomHex(t, 12)
	in.addSecret(def)
	in.setSecretDefault(s, "rotate db password", "db_password", def)

	first := in.waitScheduledRun(s, sched)
	held := in.waitStatus(s, first.ID, "pending_approval")
	if got := str(held.Raw["sealed_vars"]); !strings.Contains(got, "db_password") {
		t.Errorf("sealed_vars = %s, want the secret variable named", got)
	}
	digests, _ := held.Raw["sealed_var_digests"].([]any)
	if len(digests) != 1 {
		t.Fatalf("sealed_var_digests = %v, want one digest binding the sealed default", digests)
	}
	bound := str(field(digests[0].(map[string]any), "sha256"))
	var stored map[string]string
	if err := json.Unmarshal([]byte(in.sealedColumn(db, first.ID)), &stored); err != nil {
		t.Fatalf("decode the stored sealed answers: %v", err)
	}
	sum := sha256.Sum256([]byte(stored["db_password"]))
	if bound != hex.EncodeToString(sum[:]) {
		t.Errorf("the run binds %s, want the digest of the ciphertext it carries", bound)
	}
	var survey string
	if err := db.QueryRow("SELECT survey FROM templates WHERE id = $1", tplID).Scan(&survey); err != nil {
		t.Fatalf("read the template's survey: %v", err)
	}
	if !strings.Contains(survey, stored["db_password"]) {
		t.Error("the scheduled run's sealed answer is not the template's sealed default")
	}

	in.must(s, "approver", "POST", "/v1/runs/"+first.ID+"/approve", nil, 200)
	if done := in.waitDone(s, first.ID); done.Status != "succeeded" {
		t.Fatalf("the approved scheduled run = %s: %s", done.Status, describe(done.Raw))
	}
	defSum := sha256.Sum256([]byte(def))
	if got := strings.TrimSpace(in.marker("survey", "web1")); got != hex.EncodeToString(defSum[:]) {
		t.Errorf("the play received an answer whose digest is %s, want the digest of the default", got)
	}

	// The next fire waits for approval too. Another run's sealed answer, a person's launch with an
	// answer of their own, is copied into its row before anybody approves it.
	second := in.waitStatus(s, in.waitScheduledRun(s, sched, first.ID).ID, "pending_approval")
	in.must(s, "admin", "PUT", "/v1/schedules/"+sched, map[string]any{
		"name": "rotate on schedule", "cron": "@every 2s", "template_id": tplID, "playbook": "",
		"inventory": "", "enabled": false,
	}, 200)
	other := "swapped-in-answer-" + randomHex(t, 12)
	in.addSecret(other)
	donor := in.launched(s, "operator", "rotate db password",
		map[string]any{"answers": map[string]any{"db_password": other}})
	in.waitStatus(s, donor.ID, "pending_approval")
	if _, err := db.Exec("UPDATE runs SET sealed_vars = $1 WHERE id = $2",
		in.sealedColumn(db, donor.ID), second.ID); err != nil {
		t.Fatalf("swap the sealed answer: %v", err)
	}
	in.must(s, "approver", "POST", "/v1/runs/"+second.ID+"/approve", nil, 200)
	swapped := in.waitDone(s, second.ID)
	if swapped.Status != "failed" ||
		!strings.Contains(str(swapped.Raw["error"]), "not the one this run was created with") {
		t.Errorf("the run with a swapped sealed answer = %s (%s), want failed saying so",
			swapped.Status, str(swapped.Raw["error"]))
	}
	if got := strings.TrimSpace(in.marker("survey", "web1")); got != hex.EncodeToString(defSum[:]) {
		t.Errorf("the play ran again and received an answer whose digest is %s", got)
	}
	in.must(s, "approver", "POST", "/v1/runs/"+donor.ID+"/reject", nil, 200)

	ev := in.checkEvidence(s, first.ID, second.ID)
	refusals := ev.entries("/schedules/" + sched + "/refused/survey/db_password")
	if len(refusals) == 0 {
		t.Error("the chain holds no refusal for the fires nobody could answer")
	}
	for _, e := range refusals {
		if e.Method != "SCHEDULE" || e.Actor != "system:scheduler" {
			t.Errorf("refusal entry %+v, want the scheduler's SCHEDULE entry", e)
		}
	}
	rec := ev.Receipts[first.ID]
	requireRecord(t, rec, recordWant{
		Approver: "approver-laptop", Playbook: "survey.yml", Hosts: []string{"web1"},
		SealedVars: []string{"db_password"},
	})
	specDigests, _ := rec.outcome(t).Spec["sealed_var_digests"].([]any)
	if len(specDigests) != 1 || str(field(specDigests[0].(map[string]any), "sha256")) != bound {
		t.Errorf("the receipt's spec binds %v, want the digest the approval covered, %s", specDigests,
			bound)
	}
	requireRecord(t, ev.Receipts[second.ID], recordWant{Status: "failed", Approver: "approver-laptop"})
}
