package run

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// badBytes is a string carrying both things a text column refuses: a NUL byte and a byte sequence
// that is not valid UTF-8. Both arrive from somebody else, in a tool's output, an imported
// inventory, or a JSON body carrying a legal escaped NUL.
const badBytes = "before\x00middle\xffafter"

// clean is what badBytes becomes once the unrepresentable bytes are replaced.
const clean = "before�middle�after"

// TestBoundInt32HoldsTheNarrowestColumnRange pins the numeric clamp at both edges.
//
// The numeric columns are declared INTEGER on both backends, which is 64 bits on SQLite and 32 on
// PostgreSQL. A run submitted with a timeout of three billion seconds was stored on one and refused
// by the other with an encoding error, so the same request answered 202 or 500 depending on which
// database was behind it, and nothing clamped it on the way in.
func TestBoundInt32HoldsTheNarrowestColumnRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       int
		WantKept int
	}{
		{In: 0, WantKept: 0},                             // Test 0: Zero is untouched.
		{In: 1, WantKept: 1},                             // Test 1: A small value is kept.
		{In: -1, WantKept: -1},                           // Test 2: Negative is kept.
		{In: math.MaxInt32, WantKept: math.MaxInt32},     // Test 3: The ceiling itself.
		{In: math.MinInt32, WantKept: math.MinInt32},     // Test 4: The floor itself.
		{In: math.MaxInt32 + 1, WantKept: math.MaxInt32}, // Test 5: One past clamps.
		{In: math.MinInt32 - 1, WantKept: math.MinInt32}, // Test 6: One below clamps.
		{In: math.MaxInt64, WantKept: math.MaxInt32},     // Test 7: The widest positive.
		{In: math.MinInt64, WantKept: math.MinInt32},     // Test 8: The widest negative.
		{In: 3_000_000_000, WantKept: math.MaxInt32},     // Test 9: The reported timeout.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := boundInt32(test.In); got != test.WantKept {
				t.Errorf("boundInt32(%d) = %d, want %d", test.In, got, test.WantKept)
			}
		})
	}
}

// TestSanitizeClampsEveryNumericField pins that every number a run carries is held inside the range
// the narrowest backend can store, including the ones behind pointers.
//
// A pointer field is the easy one to miss, since it needs its own branch, and a shard index or an
// exit code out of range fails the write on PostgreSQL the same way a timeout does. Losing that
// write on a terminal save loses the run's outcome.
func TestSanitizeClampsEveryNumericField(t *testing.T) {
	t.Parallel()
	huge, tiny := math.MaxInt64, math.MinInt64
	r := &Run{
		ID: "run_1", Timeout: huge, Verbosity: huge, Forks: huge, Attempt: tiny,
		ShardIndex: &huge, StepIndex: &tiny, ShardCount: &huge, ExitCode: &tiny,
		Steps: []PipelineStep{{Name: "a", Playbook: "a.yml", Retries: huge}},
	}
	r.Sanitize()

	if r.Timeout != math.MaxInt32 || r.Verbosity != math.MaxInt32 || r.Forks != math.MaxInt32 {
		t.Errorf("timeout/verbosity/forks = %d/%d/%d, want each clamped to the 32-bit ceiling",
			r.Timeout, r.Verbosity, r.Forks)
	}
	if r.Attempt != math.MinInt32 {
		t.Errorf("attempt = %d, want the 32-bit floor", r.Attempt)
	}
	pointers := map[string]int{
		"shard index": *r.ShardIndex, "step index": *r.StepIndex,
		"shard count": *r.ShardCount, "exit code": *r.ExitCode,
	}
	want := map[string]int{
		"shard index": math.MaxInt32, "step index": math.MinInt32,
		"shard count": math.MaxInt32, "exit code": math.MinInt32,
	}
	if diff := cmp.Diff(want, pointers, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("pointer fields (-want +got):\n%s", diff)
	}
	if r.Steps[0].Retries != math.MaxInt32 {
		t.Errorf("step retries = %d, want the 32-bit ceiling", r.Steps[0].Retries)
	}
	// Clamping never rewrites the pointer itself away, so a nil stays nil.
	bare := &Run{ID: "run_2"}
	bare.Sanitize()
	if bare.ShardIndex != nil || bare.ExitCode != nil {
		t.Error("sanitizing invented a shard index or an exit code where there was none")
	}
}

// TestSanitizeCleansEveryTextFieldARunCarries pins which of a run's strings are cleaned.
//
// SQLite stores arbitrary bytes and PostgreSQL refuses them, so the same run finished on one and
// was stranded in running on the other, losing the outcome and the exit code the terminal write
// carried. The text is somebody else's, so the divergence was reachable without anybody doing
// anything unusual. The point is not to preserve exactly what a playbook printed, it is to keep the
// record of what the run did.
//
//nolint:funlen // Test function.
func TestSanitizeCleansEveryTextFieldARunCarries(t *testing.T) {
	t.Parallel()
	r := &Run{
		ID:           "run_1",
		Playbook:     badBytes,
		Inventory:    badBytes,
		Command:      badBytes,
		Error:        badBytes,
		Warning:      badBytes,
		Limit:        badBytes,
		StepName:     badBytes,
		Image:        badBytes,
		Intent:       badBytes,
		Actor:        badBytes,
		HeldByPolicy: badBytes,
		Tags:         []string{badBytes, "clean"},
		SkipTags:     []string{badBytes},
		ExtraVars:    map[string]any{badBytes: badBytes, "n": 3},
		Outputs:      map[string]any{"v": []any{badBytes}},
		Labels:       map[string]string{badBytes: badBytes},
		Steps: []PipelineStep{{
			Name: badBytes, Playbook: badBytes, Command: badBytes, Inventory: badBytes,
			DependsOn: []string{badBytes},
		}},
	}
	r.Sanitize()

	got := map[string]string{
		"playbook": r.Playbook, "inventory": r.Inventory, "command": r.Command,
		"error": r.Error, "warning": r.Warning, "limit": r.Limit, "step name": r.StepName,
		"image": r.Image, "intent": r.Intent, "actor": r.Actor, "held by": r.HeldByPolicy,
		"tag": r.Tags[0], "skip tag": r.SkipTags[0],
		"step name in graph": r.Steps[0].Name, "step playbook": r.Steps[0].Playbook,
		"step command": r.Steps[0].Command, "step inventory": r.Steps[0].Inventory,
		"step dependency": r.Steps[0].DependsOn[0],
	}
	for field, value := range got {
		if value != clean {
			t.Errorf("%s = %q, want the unrepresentable bytes replaced", field, value)
		}
	}
	if r.Tags[1] != "clean" {
		t.Errorf("a clean tag beside a dirty one became %q", r.Tags[1])
	}
	if _, ok := r.ExtraVars[clean]; !ok {
		t.Errorf("extra var keys were not cleaned: %v", r.ExtraVars)
	}
	if r.ExtraVars[clean] != clean {
		t.Errorf("extra var value = %v, want it cleaned", r.ExtraVars[clean])
	}
	if r.ExtraVars["n"] != 3 {
		t.Errorf("a non-text variable was changed to %v, want it carried through", r.ExtraVars["n"])
	}
	nested, ok := r.Outputs["v"].([]any)
	if !ok || len(nested) != 1 || nested[0] != clean {
		t.Errorf("nested output values were not cleaned: %v", r.Outputs["v"])
	}
	if r.Labels[clean] != clean {
		t.Errorf("labels were not cleaned: %v", r.Labels)
	}

	// The identifier this package mints is deliberately left alone, so a bug that put a stray byte
	// in one surfaces rather than being hidden.
	if r.ID != "run_1" {
		t.Errorf("ID = %q, want it untouched", r.ID)
	}
	// A nil run is a no-op rather than a panic, since Sanitize sits on every write path.
	(*Run)(nil).Sanitize()
}

// TestFinalizationAndProgressCleanWhatTheyWrite pins that the two fenced writes clean their own
// text, since neither goes through Run.Sanitize.
//
// The error text on a terminal write is the field that matters most: it carries whatever the tool
// printed as it failed, which is exactly where a stray byte comes from, and losing that write loses
// the run's outcome altogether.
func TestFinalizationAndProgressCleanWhatTheyWrite(t *testing.T) {
	t.Parallel()
	huge := math.MaxInt64
	fin := &Finalization{
		Error: badBytes, Warning: badBytes, Image: badBytes,
		Outputs: map[string]any{badBytes: badBytes}, ExitCode: &huge,
	}
	fin.SanitizeText()
	if fin.Error != clean || fin.Warning != clean || fin.Image != clean {
		t.Errorf("finalization text = %q / %q / %q, want each cleaned",
			fin.Error, fin.Warning, fin.Image)
	}
	if fin.Outputs[clean] != clean {
		t.Errorf("finalization outputs = %v, want cleaned", fin.Outputs)
	}
	if *fin.ExitCode != math.MaxInt32 {
		t.Errorf("exit code = %d, want clamped to the 32-bit ceiling", *fin.ExitCode)
	}

	p := &Progress{Warning: badBytes, Outputs: map[string]any{badBytes: badBytes}}
	p.SanitizeText()
	if p.Warning != clean {
		t.Errorf("progress warning = %q, want cleaned", p.Warning)
	}
	if p.Outputs[clean] != clean {
		t.Errorf("progress outputs = %v, want cleaned", p.Outputs)
	}

	// Neither panics on a nil receiver, since both sit on write paths that may be handed nothing.
	(*Finalization)(nil).SanitizeText()
	(*Progress)(nil).SanitizeText()
}

// TestSummarySanitizersCleanTheNamesTheyDidNotChoose pins that the per-host and per-task summaries
// written beside a run are cleaned too.
//
// A host name comes from an imported or dynamic inventory and a task name comes from somebody's
// playbook, so neither is a name this install chose. They did not go through any cleaning, so the
// same finished run recorded its fleet summary on SQLite and, on PostgreSQL, failed the insert
// while the run itself finalized. The caller logs and continues, so the run looked complete and was
// silently missing from fleet health, drift, host history, task trends, and host costs.
func TestSummarySanitizersCleanTheNamesTheyDidNotChoose(t *testing.T) {
	t.Parallel()
	hosts := []HostSummary{{Host: badBytes, Worst: badBytes}, {Host: "clean", Worst: "ok"}}
	SanitizeHostSummaries(hosts)
	if hosts[0].Host != clean || hosts[0].Worst != clean {
		t.Errorf("host summary = %+v, want cleaned", hosts[0])
	}
	if hosts[1].Host != "clean" {
		t.Errorf("a clean host summary was changed to %+v", hosts[1])
	}

	tasks := []TaskSummary{{Task: badBytes}}
	SanitizeTaskSummaries(tasks)
	if tasks[0].Task != clean {
		t.Errorf("task summary = %+v, want cleaned", tasks[0])
	}

	facts := []HostFacts{{Host: badBytes, Facts: map[string]string{badBytes: badBytes}}}
	SanitizeHostFacts(facts)
	if facts[0].Host != clean || facts[0].Facts[clean] != clean {
		t.Errorf("host facts = %+v, want the keys and values from the target machine cleaned",
			facts[0])
	}

	// Nil receivers and empty batches are no-ops on every one of them.
	(*HostSummary)(nil).Sanitize()
	(*TaskSummary)(nil).Sanitize()
	(*HostFacts)(nil).Sanitize()
	SanitizeHostSummaries(nil)
	SanitizeTaskSummaries(nil)
	SanitizeHostFacts(nil)
}

// TestTheStoreCleansOnEveryWritePath pins that cleaning happens at the store boundary rather than
// wherever each value is set, which is the point: no write path can miss it.
//
// A caller can reach every one of these writes with text it did not choose, so each is checked
// through the store rather than by calling the sanitizer directly.
func TestTheStoreCleansOnEveryWritePath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	saveRun(t, store, &Run{
		ID: "run_1", Playbook: badBytes, Status: StatusRunning, CreatedAt: time.Now(),
		ClaimedBy: "worker-a",
	})
	if got := getRun(t, store, "run_1"); got.Playbook != clean {
		t.Errorf("saved playbook = %q, want cleaned at the store boundary", got.Playbook)
	}
	if _, err := store.ApplyRunningProgress(ctx, "run_1", "worker-a",
		Progress{Warning: badBytes}); err != nil {
		t.Fatalf("ApplyRunningProgress() error = %v", err)
	}
	if got := getRun(t, store, "run_1"); got.Warning != clean {
		t.Errorf("progress warning = %q, want cleaned", got.Warning)
	}
	if err := store.SaveHostSummary(ctx, "run_1", []HostSummary{
		{Host: badBytes, Worst: badBytes},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	hosts, err := store.RunHostSummaries(ctx, "run_1")
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(hosts) != 1 || hosts[0].Host != clean {
		t.Errorf("stored host summary = %+v, want cleaned", hosts)
	}
	if err := store.SaveTaskSummary(ctx, "run_1", []TaskSummary{{Task: badBytes}}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}
	tasks, err := store.RunTaskSummaries(ctx, "run_1")
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Task != clean {
		t.Errorf("stored task summary = %+v, want cleaned", tasks)
	}
	if err := store.SaveHostFacts(ctx, "run_1", []HostFacts{
		{Host: badBytes, Facts: map[string]string{"os": badBytes}},
	}); err != nil {
		t.Fatalf("SaveHostFacts() error = %v", err)
	}
	if got, err := store.HostFactsFor(ctx, clean); err != nil || got.Facts["os"] != clean {
		t.Errorf("stored host facts = (%+v, %v), want cleaned", got, err)
	}
	if _, err := store.FinalizeRunning(ctx, "run_1", Finalization{
		Status: StatusFailed, Error: badBytes, Owner: "worker-a", EndedAt: time.Now(),
	}); err != nil {
		t.Fatalf("FinalizeRunning() error = %v", err)
	}
	if got := getRun(t, store, "run_1"); got.Error != clean {
		t.Errorf("terminal error = %q, want cleaned: losing this write loses the outcome", got.Error)
	}
}

// TestSanitizeAcceptsOrdinaryUnicodeUnchanged pins that cleaning leaves text alone when there is
// nothing wrong with it, so a foreign-language inventory or playbook name is stored as written.
//
// Replacing is only justified where the alternative is losing the record. A sanitizer that mangled
// legitimate text would corrupt the evidence it exists to preserve.
func TestSanitizeAcceptsOrdinaryUnicodeUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
	}{
		{Name: "ascii", In: "site.yml"},                         // Test 0: Plain.
		{Name: "accented", In: "déploiement.yml"},               // Test 1: Latin with accents.
		{Name: "cjk", In: "配置/部署.yml"},                          // Test 2: Wide characters.
		{Name: "emoji", In: "deploy 🚀"},                         // Test 3: Astral plane.
		{Name: "rtl", In: "نشر.yml"},                            // Test 4: Right to left.
		{Name: "combining", In: "étape.yml"},                   // Test 5: Combining marks.
		{Name: "newlines and tabs", In: "line one\n\tline two"}, // Test 6: Control characters that
		// a text column does accept.
		{Name: "empty", In: ""}, // Test 7: Nothing at all.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := &Run{ID: "run_1", Playbook: test.In, Command: test.In, Error: test.In}
			r.Sanitize()
			if r.Playbook != test.In || r.Command != test.In || r.Error != test.In {
				t.Errorf("Sanitize() changed %q to %q, want it stored as written",
					test.In, r.Playbook)
			}
		})
	}
}

// TestSanitizeHandlesVeryLongText pins that cleaning a large field neither truncates it nor takes a
// pathological amount of work, since a tool's output is what lands in the error text and a run that
// printed for a long time before failing produces a very large one.
func TestSanitizeHandlesVeryLongText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 1<<20)
	r := &Run{ID: "run_1", Error: long}
	r.Sanitize()
	if len(r.Error) != len(long) {
		t.Errorf("a megabyte of clean text became %d bytes, want it kept whole", len(r.Error))
	}
	dirty := strings.Repeat("a\x00", 1<<19)
	r = &Run{ID: "run_1", Error: dirty}
	r.Sanitize()
	if strings.ContainsRune(r.Error, 0) {
		t.Error("a NUL byte survived cleaning, so the write would be refused by PostgreSQL")
	}
}
