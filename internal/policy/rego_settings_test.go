package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// adviceModule warns about a run with no change ticket, holds anything bound for production, and
// refuses an agent in production, so one bundle exercises every rule a warn setting could touch.
const adviceModule = `warn contains "no change ticket on the run" if not input.run.labels.ticket

hold contains "production change" if input.run.queue == "prod"

deny contains "agents stay out of production" if {
	input.actor.kind == "agent"
	input.run.queue == "prod"
}`

// slowModule evaluates for tens of milliseconds, long enough that a one millisecond timeout always
// cuts it off and short enough that ten seconds never does, even under the race detector.
const slowModule = `hold contains "x" if {
	count([y | some y in numbers.range(1, 20000); y < 0]) > 0
}`

// settingsPolicy wraps body, compiled with opts, in a policy the way the file store does.
func settingsPolicy(t *testing.T, name, body string, opts ...RegoOption) *Policy {
	t.Helper()
	prog, err := CompileRego("", "", []RegoModule{{
		File: "policy.rego", Source: "package switchtender\n\n" + body + "\n",
	}}, opts...)
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	return &Policy{ID: filePolicyID(name), Name: name, MaxDestroy: DisabledMaxDestroy, Rego: prog}
}

// noteOf is the note a policy named advice records with the given messages.
func noteOf(p *Policy, reasons string) string {
	return "advice (" + reasons + ", rego sha256:" + p.Rego.Digest()[:12] + ")"
}

// TestARegoWarningHoldsOrNotesAsThePolicySays pins decision 3. A warning holds the run by default
// and, under warn: note, is recorded on the run instead, while the bundle's hold and deny rules keep
// doing exactly what they did: the setting softens warn and nothing else.
func TestARegoWarningHoldsOrNotesAsThePolicySays(t *testing.T) {
	t.Parallel()
	hold := settingsPolicy(t, "advice", adviceModule)
	note := settingsPolicy(t, "advice", adviceModule, WithRegoWarn(RegoWarnNote))
	ticket := map[string]string{"ticket": "CHG-1"}
	tests := []struct {
		// Policy is the policy in force.
		Policy *Policy
		// Run is the run judged.
		Run *run.Run
		// WantVerdict is what the dispatcher reads.
		WantVerdict verdict
		// WantNotes are the notes recorded on the run.
		WantNotes []string
	}{{ // Test 0: By default a warning holds, and nothing is noted.
		Policy:      hold,
		Run:         &run.Run{ID: "r0", Tool: "bash", Command: "uptime"},
		WantVerdict: verdict{Held: true},
	}, { // Test 1: Set to note, the same warning is recorded and the run goes ahead.
		Policy:    note,
		Run:       &run.Run{ID: "r1", Tool: "bash", Command: "uptime"},
		WantNotes: []string{noteOf(note, "no change ticket on the run")},
	}, { // Test 2: A run the warn rule does not fire on carries nothing.
		Policy: note,
		Run:    &run.Run{ID: "r2", Tool: "bash", Command: "uptime", Labels: ticket},
	}, { // Test 3: The hold rule still holds, and the warning is noted beside it.
		Policy:      note,
		Run:         &run.Run{ID: "r3", Tool: "bash", Command: "uptime", Queue: "prod"},
		WantVerdict: verdict{Held: true},
		WantNotes:   []string{noteOf(note, "no change ticket on the run")},
	}, { // Test 4: The deny rule still refuses.
		Policy: note,
		Run: &run.Run{ID: "r4", Tool: "bash", Command: "uptime", Queue: "prod",
			ActorType: ActorKindAgent},
		WantVerdict: verdict{Denied: true},
		WantNotes:   []string{noteOf(note, "no change ticket on the run")},
	}, { // Test 5: Under the default a run in production is held by the warning and the hold alike.
		Policy:      hold,
		Run:         &run.Run{ID: "r5", Tool: "bash", Command: "uptime", Queue: "prod"},
		WantVerdict: verdict{Held: true},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			set := []*Policy{test.Policy}
			if diff := cmp.Diff(test.WantVerdict, verdictOf(set, test.Run)); diff != "" {
				t.Errorf("verdict mismatch (-want +got):\n%s", diff)
			}
			got := Noting(set, test.Run)
			if diff := cmp.Diff(test.WantNotes, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("notes mismatch (-want +got):\n%s", diff)
			}
		})
	}
	// The hold a default warning makes names the warning, so the two settings tell the approver
	// the same thing in the two places a warning can land.
	held := Requiring([]*Policy{hold}, &run.Run{ID: "r", Tool: "bash", Command: "uptime"})
	if held == nil || held.Label() != noteOf(hold, "no change ticket on the run") {
		t.Errorf("held by %v, want the warning named the way a note names it", held)
	}
}

// TestANotedWarningFollowsTheCheckModeSecondPass pins that warn: note keeps the second pass a forcing
// dry run gets. A module exempting previews by run.dry_run notes a dry run whose playbook forces real
// tasks, exactly where the same module under the default holds it, and notes nothing for a dry run
// that changes nothing.
func TestANotedWarningFollowsTheCheckModeSecondPass(t *testing.T) {
	t.Parallel()
	body := `warn contains "production change" if {
	input.run.queue == "prod"
	not input.run.dry_run
}`
	hold := settingsPolicy(t, "advice", body)
	note := settingsPolicy(t, "advice", body, WithRegoWarn(RegoWarnNote))
	forced := []string{`site.yml: task "Restart" sets check_mode to false`}
	tests := []struct {
		// DryRun makes the run a dry run.
		DryRun bool
		// Forced is what the dry run's playbook forces under check mode.
		Forced []string
		// Want is whether a warning fires at all.
		Want bool
	}{{ // Test 0: A real run warns.
		DryRun: false, Want: true,
	}, { // Test 1: A change-free dry run does not.
		DryRun: true, Want: false,
	}, { // Test 2: A dry run that forces real tasks is judged as the real run it partly is.
		DryRun: true, Forced: forced, Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "r", Playbook: "site.yml", Queue: "prod", DryRun: test.DryRun}
			if len(test.Forced) > 0 {
				r.DryRunScans = []run.DryRunScan{run.DryRunScan{Tool: run.ToolAnsible,
					Scanner: run.CheckModeScanner, Version: run.CheckModeScannerVersion,
					Findings: test.Forced}.Classified()}
			}
			if held := Requiring([]*Policy{hold}, r) != nil; held != test.Want {
				t.Errorf("under the default held = %v, want %v", held, test.Want)
			}
			if held := Requiring([]*Policy{note}, r) != nil; held {
				t.Error("under warn: note the warning held the run")
			}
			var want []string
			if test.Want {
				want = []string{noteOf(note, "production change")}
			}
			if diff := cmp.Diff(want, Noting([]*Policy{note}, r), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("notes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNotingSoftensAWarningNeverAFailure pins that a policy set to note its warnings still refuses
// when it cannot decide. The setting changes what a warning does, and a policy that errored or ran
// out of time has not warned: it has decided nothing, which is a refusal under every setting.
func TestNotingSoftensAWarningNeverAFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the module body.
		Body string
		// Opts are the settings it is compiled with.
		Opts []RegoOption
		// WantText is a fragment the refusal must carry.
		WantText string
	}{{ // Test 0: A builtin error.
		Body:     "warn contains x if { x := to_number(input.run.command) }",
		Opts:     []RegoOption{WithRegoWarn(RegoWarnNote)},
		WantText: "to_number",
	}, { // Test 1: A warn rule of the wrong shape.
		Body:     "warn := 5",
		Opts:     []RegoOption{WithRegoWarn(RegoWarnNote)},
		WantText: "warn is a",
	}, { // Test 2: An evaluation cut off by its timeout.
		Body:     "warn contains \"slow\" if true\n\n" + slowModule,
		Opts:     []RegoOption{WithRegoWarn(RegoWarnNote), WithRegoTimeout(time.Millisecond)},
		WantText: "exceeded its 1ms timeout",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			set := []*Policy{settingsPolicy(t, "advice", test.Body, test.Opts...)}
			r := &run.Run{ID: "r", Tool: "bash", Command: "not a number"}
			denied := Denying(set, r)
			if denied == nil {
				t.Fatal("a noting policy that could not decide let the submission through")
			}
			if !strings.Contains(denied.Label(), test.WantText) {
				t.Errorf("refusal %q does not say why: want %q", denied.Label(), test.WantText)
			}
			if Requiring(set, r) == nil {
				t.Error("a noting policy that could not decide did not hold the run either")
			}
			if notes := Noting(set, r); len(notes) != 0 {
				t.Errorf("a policy that could not decide noted %v, as though it had decided", notes)
			}
		})
	}
}

// TestARegoSettingThisBuildCannotHonorIsRefusedAtLoad pins that every warn and timeout value is
// checked where it is written. A value outside what the evaluator honors would otherwise load and be
// enforced as something other than what the file says.
func TestARegoSettingThisBuildCannotHonorIsRefusedAtLoad(t *testing.T) {
	t.Parallel()
	withWarn := "package switchtender\n\nwarn contains \"x\" if true\n"
	withoutWarn := "package switchtender\n\nhold contains \"x\" if true\n"
	tests := []struct {
		// Source is the module.
		Source string
		// Opts are the settings.
		Opts []RegoOption
		// WantText is a fragment the refusal must carry, empty when the program must compile.
		WantText string
		// WantWarn and WantTimeout are the settings a compiled program must report.
		WantWarn    string
		WantTimeout time.Duration
	}{{ // Test 0: No settings is the default.
		Source: withWarn, WantWarn: RegoWarnHold, WantTimeout: DefaultRegoTimeout,
	}, { // Test 1: Note, with a timeout at the cap.
		Source:   withWarn,
		Opts:     []RegoOption{WithRegoWarn(RegoWarnNote), WithRegoTimeout(10 * time.Second)},
		WantWarn: RegoWarnNote, WantTimeout: MaxRegoTimeout,
	}, { // Test 2: Hold named explicitly, which needs no warn rule since it changes nothing.
		Source: withoutWarn, Opts: []RegoOption{WithRegoWarn(RegoWarnHold)},
		WantWarn: RegoWarnHold, WantTimeout: DefaultRegoTimeout,
	}, { // Test 3: A warn mode this build does not know.
		Source: withWarn, Opts: []RegoOption{WithRegoWarn("ignore")},
		WantText: `warn must be "hold" or "note", not "ignore"`,
	}, { // Test 4: A timeout of zero, which would refuse every run.
		Source: withWarn, Opts: []RegoOption{WithRegoTimeout(0)},
		WantText: "timeout must be more than zero and at most 10s, not 0s",
	}, { // Test 5: A negative timeout.
		Source: withWarn, Opts: []RegoOption{WithRegoTimeout(-time.Second)},
		WantText: "not -1s",
	}, { // Test 6: A timeout past the cap.
		Source: withWarn, Opts: []RegoOption{WithRegoTimeout(10*time.Second + time.Nanosecond)},
		WantText: "at most 10s",
	}, { // Test 7: Note on a package with no warn rule, which would read as softening its holds.
		Source: withoutWarn, Opts: []RegoOption{WithRegoWarn(RegoWarnNote)},
		WantText: "defines no warn rule",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			prog, err := CompileRego("", "", []RegoModule{{File: "p.rego", Source: test.Source}},
				test.Opts...)
			if test.WantText == "" {
				if err != nil {
					t.Fatalf("CompileRego() error = %v", err)
				}
				if prog.Warn() != test.WantWarn || prog.Timeout() != test.WantTimeout {
					t.Errorf("settings = %s, %s, want %s, %s", prog.Warn(), prog.Timeout(),
						test.WantWarn, test.WantTimeout)
				}
				return
			}
			if !errors.Is(err, ErrRego) {
				t.Fatalf("CompileRego() error = %v, want ErrRego", err)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("CompileRego() error = %v, want it to mention %q", err, test.WantText)
			}
		})
	}
}

// TestARegoTimeoutBoundsEachEvaluation pins decision 4: the policy's own timeout is the one
// evaluation is held to, in both directions, and the refusal names the limit that cut it off and
// where it can be raised to.
func TestARegoTimeoutBoundsEachEvaluation(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "r", Tool: "bash", Command: "uptime"}
	short := settingsPolicy(t, "slow", slowModule, WithRegoTimeout(time.Millisecond))
	d := short.Rego.Decide(r)
	if !errors.Is(d.Err, ErrRego) {
		t.Fatalf("a module cut off at 1ms decided: %+v", d)
	}
	for _, want := range []string{"exceeded its 1ms timeout", "at most 10s"} {
		if !strings.Contains(d.Err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", d.Err, want)
		}
	}
	denied := Denying([]*Policy{short}, r)
	if denied == nil || !strings.Contains(denied.Label(), "1ms") {
		t.Errorf("the refusal the dispatcher reads is %v, want one naming the 1ms limit", denied)
	}

	long := settingsPolicy(t, "slow", slowModule, WithRegoTimeout(MaxRegoTimeout))
	if d := long.Rego.Decide(r); d.Err != nil {
		t.Errorf("the same module under a 10s timeout could not decide: %v", d.Err)
	}

	// At the cap the refusal cannot point at a higher setting, so it says the cap was reached.
	capped := &RegoProgram{timeout: MaxRegoTimeout}
	got := capped.timedOut().Error()
	if !strings.Contains(got, "10s timeout, the most a policy may set") {
		t.Errorf("the refusal at the cap = %q, want it to say the cap was reached", got)
	}
}

// TestAPolicyFileReadsWarnAndTimeout pins the file syntax for both settings and every refusal the
// loader adds around them, each of which would otherwise load a policy enforcing something the file
// does not say.
func TestAPolicyFileReadsWarnAndTimeout(t *testing.T) {
	t.Parallel()
	module := "package switchtender\n\nwarn contains \"x\" if true\n"
	noWarn := "package switchtender\n\nhold contains \"x\" if true\n"
	tests := []struct {
		// Entry is the rego entry's settings, appended to its name and files.
		Entry string
		// Module is the module it loads.
		Module string
		// WantText is a fragment the refusal must carry, empty for a file that loads.
		WantText string
		// WantWarn and WantTimeout are the settings the loaded policy must carry.
		WantWarn    string
		WantTimeout time.Duration
	}{{ // Test 0: Neither setting.
		Module: module, WantWarn: RegoWarnHold, WantTimeout: DefaultRegoTimeout,
	}, { // Test 1: Both settings.
		Entry: "    warn: note\n    timeout: 2s\n", Module: module,
		WantWarn: RegoWarnNote, WantTimeout: 2 * time.Second,
	}, { // Test 2: A fractional duration.
		Entry: "    timeout: 1.5s\n", Module: module,
		WantWarn: RegoWarnHold, WantTimeout: 1500 * time.Millisecond,
	}, { // Test 3: A bare number, which names no unit.
		Entry: "    timeout: 500\n", Module: module, WantText: "write it with a unit",
	}, { // Test 4: Not a duration at all.
		Entry: "    timeout: soon\n", Module: module, WantText: `timeout "soon" is not a duration`,
	}, { // Test 5: Past the cap.
		Entry: "    timeout: 30s\n", Module: module, WantText: "at most 10s, not 30s",
	}, { // Test 6: Zero.
		Entry: "    timeout: 0s\n", Module: module, WantText: "more than zero",
	}, { // Test 7: A warn mode this build does not know.
		Entry: "    warn: ignore\n", Module: module, WantText: `not "ignore"`,
	}, { // Test 8: Note on a package with nothing to note.
		Entry: "    warn: note\n", Module: noWarn, WantText: "defines no warn rule",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := writeRegoFiles(t, t.TempDir(), map[string]string{
				"policies.yml": "rego:\n  - name: advice\n    files: [advice.rego]\n" + test.Entry,
				"advice.rego":  test.Module,
			})
			store, err := NewFileStore(path)
			if test.WantText != "" {
				if err == nil {
					t.Fatal("NewFileStore() loaded a file it should refuse")
				}
				if !strings.Contains(err.Error(), test.WantText) ||
					!strings.Contains(err.Error(), `rego policy "advice"`) {
					t.Errorf("NewFileStore() error = %v, want it to name the policy and %q", err,
						test.WantText)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFileStore() error = %v", err)
			}
			all, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if got := all[0].Rego; got.Warn() != test.WantWarn || got.Timeout() != test.WantTimeout {
				t.Errorf("settings = %s, %s, want %s, %s", got.Warn(), got.Timeout(), test.WantWarn,
					test.WantTimeout)
			}
		})
	}
}

// TestRegoSettingsTravelToAWorker pins that a relay worker evaluates with the settings the control
// node loaded. A worker that dropped them would hold what the control node notes and time out where
// the control node does not, so the same run would be judged two ways depending on who claimed it.
func TestRegoSettingsTravelToAWorker(t *testing.T) {
	t.Parallel()
	sent := settingsPolicy(t, "advice", adviceModule, WithRegoWarn(RegoWarnNote),
		WithRegoTimeout(2*time.Second))
	raw, err := json.Marshal([]*Policy{sent})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got []*Policy
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got[0].Rego.Warn() != RegoWarnNote || got[0].Rego.Timeout() != 2*time.Second {
		t.Fatalf("the worker's copy has %s, %s, want note, 2s", got[0].Rego.Warn(),
			got[0].Rego.Timeout())
	}
	r := &run.Run{ID: "r", Tool: "bash", Command: "uptime"}
	if diff := cmp.Diff(Noting([]*Policy{sent}, r), Noting(got, r)); diff != "" {
		t.Errorf("the worker's copy notes differently (-control +worker):\n%s", diff)
	}

	tests := []struct {
		// Tamper edits the encoded policy.
		Tamper func(s string) string
		// WantWarn and WantTimeout are what the decoded copy must carry, unless WantErr.
		WantWarn    string
		WantTimeout time.Duration
		// WantErr is whether the decode must be refused.
		WantErr bool
	}{{ // Test 0: A control node older than the settings sends neither, which is what it enforced.
		Tamper: func(s string) string {
			s = strings.Replace(s, `"warn":"note",`, "", 1)
			return strings.Replace(s, `,"timeout":"2s"`, "", 1)
		},
		WantWarn: RegoWarnHold, WantTimeout: DefaultRegoTimeout,
	}, { // Test 1: A timeout past the cap.
		Tamper: func(s string) string {
			return strings.Replace(s, `"timeout":"2s"`, `"timeout":"1m"`, 1)
		},
		WantErr: true,
	}, { // Test 2: A timeout that is not a duration.
		Tamper: func(s string) string {
			return strings.Replace(s, `"timeout":"2s"`, `"timeout":"2"`, 1)
		},
		WantErr: true,
	}, { // Test 3: A warn mode this build does not know.
		Tamper: func(s string) string {
			return strings.Replace(s, `"warn":"note"`, `"warn":"off"`, 1)
		},
		WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tampered := test.Tamper(string(raw))
			if tampered == string(raw) {
				t.Fatal("the tamper changed nothing, so the case proves nothing")
			}
			var out []*Policy
			err := json.Unmarshal([]byte(tampered), &out)
			if test.WantErr {
				if !errors.Is(err, ErrRego) {
					t.Errorf("Unmarshal(tampered) error = %v, want ErrRego", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if out[0].Rego.Warn() != test.WantWarn || out[0].Rego.Timeout() != test.WantTimeout {
				t.Errorf("settings = %s, %s, want %s, %s", out[0].Rego.Warn(), out[0].Rego.Timeout(),
					test.WantWarn, test.WantTimeout)
			}
		})
	}
}

// TestTheRuleSetRecordCoversTheRegoSettings pins the evidence half of both decisions. Noting a
// warning instead of holding it, and changing the timeout, change what the same bundle does to a
// run, so each moves the in-force digest every run records and is named in the rule's description.
// A setting written out at its default changes nothing and moves nothing.
func TestTheRuleSetRecordCoversTheRegoSettings(t *testing.T) {
	t.Parallel()
	body := "warn contains \"x\" if true"
	of := func(opts ...RegoOption) InForceSet {
		return InForce([]*Policy{settingsPolicy(t, "advice", body, opts...)})
	}
	base := of()
	digest := settingsPolicy(t, "advice", body).Rego.Digest()
	wantBase := "advice: decided by Rego package data.switchtender, bundle sha256:" + digest
	if diff := cmp.Diff([]string{wantBase}, base.Rules); diff != "" {
		t.Errorf("a policy at its defaults is described differently (-want +got):\n%s", diff)
	}
	tests := []struct {
		// Opts are the settings.
		Opts []RegoOption
		// WantMoved is whether the set digest must move.
		WantMoved bool
		// WantRule is the rule's exact description.
		WantRule string
	}{{ // Test 0: Notes instead of holds.
		Opts: []RegoOption{WithRegoWarn(RegoWarnNote)}, WantMoved: true,
		WantRule: wantBase + ", warnings noted without holding",
	}, { // Test 1: A longer timeout.
		Opts: []RegoOption{WithRegoTimeout(2 * time.Second)}, WantMoved: true,
		WantRule: wantBase + ", timeout 2s",
	}, { // Test 2: Both.
		Opts:      []RegoOption{WithRegoWarn(RegoWarnNote), WithRegoTimeout(750 * time.Millisecond)},
		WantMoved: true, WantRule: wantBase + ", warnings noted without holding, timeout 750ms",
	}, { // Test 3: The default timeout written out.
		Opts: []RegoOption{WithRegoTimeout(DefaultRegoTimeout)}, WantMoved: false, WantRule: wantBase,
	}, { // Test 4: The default warn mode written out.
		Opts: []RegoOption{WithRegoWarn(RegoWarnHold)}, WantMoved: false, WantRule: wantBase,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := of(test.Opts...)
			if moved := got.Digest != base.Digest; moved != test.WantMoved {
				t.Errorf("digest moved = %v, want %v", moved, test.WantMoved)
			}
			if diff := cmp.Diff([]string{test.WantRule}, got.Rules); diff != "" {
				t.Errorf("rule description mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestANoteCarriesABoundedNumberOfMessages pins that a module emitting a message per variable name
// cannot grow every run record and outcome it notes without limit: a note carries maxNoteReasons
// messages and says how many more there were.
func TestANoteCarriesABoundedNumberOfMessages(t *testing.T) {
	t.Parallel()
	p := settingsPolicy(t, "advice", `warn contains msg if {
	some i in numbers.range(10, 24)
	msg := sprintf("variable %d", [i])
}`, WithRegoWarn(RegoWarnNote))
	notes := Noting([]*Policy{p}, &run.Run{ID: "r", Tool: "bash", Command: "x"})
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one note for the one policy", notes)
	}
	want := noteOf(p, "variable 10, variable 11, variable 12, variable 13, variable 14, "+
		"variable 15, variable 16, variable 17, variable 18, variable 19, and 5 more")
	if notes[0] != want {
		t.Errorf("note = %q, want %q", notes[0], want)
	}
}
