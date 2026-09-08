package outcome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// errStore stands for a store that cannot answer, so a test can tell a read failure from a run that
// simply had nothing to report.
var errStore = errors.New("the run store is unreachable")

// fakeRunStore answers only the reads the outcome record is built from, so a test can decide what
// each one returns. Every other method is left to the embedded interface and must never be called.
type fakeRunStore struct {
	run.Store
	// chunks are the log pages LogAfter serves, in order.
	chunks []run.LogChunk
	// hosts and tasks are the stored summaries.
	hosts []run.HostSummary
	tasks []run.TaskSummary
	// shards and steps are the children of a coordinator.
	shards []*run.Run
	steps  []*run.Run
	// logErrForID fails the log read for exactly one run, so a child's log can fail while its
	// coordinator's succeeds.
	logErrForID string
	// logErr, hostErr, taskErr, shardErr, and stepErr each fail one read.
	logErr   error
	hostErr  error
	taskErr  error
	shardErr error
	stepErr  error
	// limits records the page size asked for on each log read.
	limits []int
	// reads counts the log reads, so paging is visible.
	reads int
}

// LogAfter serves the chunks past afterSeq, capped at limit, the contract the real stores keep.
func (f *fakeRunStore) LogAfter(_ context.Context, id string, afterSeq int64,
	limit int) ([]run.LogChunk, error) {
	if f.logErr != nil {
		return nil, f.logErr
	}
	if f.logErrForID != "" && f.logErrForID == id {
		return nil, errStore
	}
	f.reads++
	f.limits = append(f.limits, limit)
	var out []run.LogChunk
	for _, c := range f.chunks {
		if c.Seq <= afterSeq {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// RunHostSummaries returns the stored per host summaries.
func (f *fakeRunStore) RunHostSummaries(context.Context, string) ([]run.HostSummary, error) {
	return f.hosts, f.hostErr
}

// RunTaskSummaries returns the stored per task summaries.
func (f *fakeRunStore) RunTaskSummaries(context.Context, string) ([]run.TaskSummary, error) {
	return f.tasks, f.taskErr
}

// Shards returns the split children.
func (f *fakeRunStore) Shards(context.Context, string) ([]*run.Run, error) {
	return f.shards, f.shardErr
}

// Steps returns the pipeline children.
func (f *fakeRunStore) Steps(context.Context, string) ([]*run.Run, error) {
	return f.steps, f.stepErr
}

// TestBodyRefusesToBuildARecordFromAPartialRead proves every store read the outcome record depends
// on is a hard failure rather than an omission.
//
// The record is what the chain commits as evidence of what a run did. A read that failed and was
// carried on from would commit a record saying the run touched no hosts, ran no tasks, and produced
// the log of an empty string, which is a false statement signed into the audit chain rather than a
// missing one.
func TestBodyRefusesToBuildARecordFromAPartialRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the read that fails.
		Name string
		// Break makes that one read fail.
		Break func(s *fakeRunStore)
	}{{ // Test 0: The log cannot be streamed, so the digest binding output to the record is unknown.
		Name: "log", Break: func(s *fakeRunStore) { s.logErr = errStore },
	}, { // Test 1: The per host summaries cannot be read.
		Name: "host summaries", Break: func(s *fakeRunStore) { s.hostErr = errStore },
	}, { // Test 2: The per task summaries cannot be read.
		Name: "task summaries", Break: func(s *fakeRunStore) { s.taskErr = errStore },
	}, { // Test 3: A coordinator's shards cannot be read.
		Name: "shards", Break: func(s *fakeRunStore) { s.shardErr = errStore },
	}, { // Test 4: A coordinator's pipeline steps cannot be read.
		Name: "steps", Break: func(s *fakeRunStore) { s.stepErr = errStore },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := &fakeRunStore{}
			test.Break(store)
			r := &run.Run{ID: "run_1", Status: run.StatusSucceeded}
			body, err := Body(context.Background(), store, r)
			if !errors.Is(err, errStore) {
				t.Fatalf("Body() error = %v, want the store's failure reported", err)
			}
			if body != nil {
				t.Errorf("Body() returned %s beside its error, want no record at all", body)
			}
		})
	}
}

// TestBodyRefusesARunWhoseSpecWillNotEncode proves an outcome is not committed for a run whose spec
// cannot be reduced to bytes. The spec digest is what ties the outcome to the change an approver
// decided on, so a record carrying an empty one would claim the run executed something nobody can
// identify.
func TestBodyRefusesARunWhoseSpecWillNotEncode(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run_1", Status: run.StatusSucceeded,
		ExtraVars: map[string]any{"ratio": math.NaN()}}
	body, err := Body(context.Background(), &fakeRunStore{}, r)
	if err == nil {
		t.Fatalf("Body() = %s, nil error, want the unencodable spec refused", body)
	}
	if body != nil {
		t.Errorf("Body() returned %s beside its error", body)
	}
}

// TestLogDigestPagesTheWholeLog pins the streaming digest at the page boundary. A run's log is
// hashed in bounded memory rather than loaded whole, so the loop has to keep reading until a short
// page says the log has ended. Stopping one page early would digest part of a log and bind the
// record to output that is not the output an operator reads.
func TestLogDigestPagesTheWholeLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Chunks is how many log chunks the store holds.
		Chunks int
		// WantReads is how many reads it should take to reach the end.
		WantReads int
	}{
		{Chunks: 0, WantReads: 1},                   // Test 0: An empty log, one read.
		{Chunks: 1, WantReads: 1},                   // Test 1: A short first page ends it.
		{Chunks: logDigestPage - 1, WantReads: 1},   // Test 2: One short of a full page.
		{Chunks: logDigestPage, WantReads: 2},       // Test 3: Exactly a page, so it reads again.
		{Chunks: logDigestPage + 1, WantReads: 2},   // Test 4: One over.
		{Chunks: logDigestPage * 2, WantReads: 3},   // Test 5: Two full pages then the empty read.
		{Chunks: logDigestPage*2 + 3, WantReads: 3}, // Test 6: Two pages and a short one.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := &fakeRunStore{}
			h := sha256.New()
			for i := 0; i < test.Chunks; i++ {
				data := []byte(fmt.Sprintf("chunk %d\n", i))
				store.chunks = append(store.chunks, run.LogChunk{Seq: int64(i + 1), Data: data})
				h.Write(data)
			}
			want := hex.EncodeToString(h.Sum(nil))

			got, err := logDigest(context.Background(), store, "run_1")
			if err != nil {
				t.Fatalf("logDigest() error = %v", err)
			}
			if got != want {
				t.Errorf("logDigest() = %q, want the digest of every chunk in order %q", got, want)
			}
			if store.reads != test.WantReads {
				t.Errorf("the log was read %d times, want %d; a loop that stops early digests "+
					"part of the log", store.reads, test.WantReads)
			}
			for _, limit := range store.limits {
				if limit != logDigestPage {
					t.Errorf("a read asked for %d chunks, want the bounded page %d", limit,
						logDigestPage)
				}
			}
		})
	}
}

// TestLogDigestOfNoOutputIsTheEmptyDigest pins what a run that printed nothing commits to. A
// coordinator executes nothing itself, so this is the value its own record carries, and it has to
// be the digest of the empty string rather than an empty field a verifier would skip checking.
func TestLogDigestOfNoOutputIsTheEmptyDigest(t *testing.T) {
	t.Parallel()
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	got, err := logDigest(context.Background(), &fakeRunStore{}, "run_1")
	if err != nil {
		t.Fatalf("logDigest() error = %v", err)
	}
	if got != emptySHA {
		t.Errorf("logDigest() over no output = %q, want the empty digest %q", got, emptySHA)
	}
}

// TestBodySortsAndNormalizesWhateverTheStoreHandsBack proves the record reduces to fixed bytes
// however the store answered. The digest is taken over these bytes and a receipt holder recomputes
// them from a rebuilt record, so an order that followed the store's answer would make the same run
// digest two ways and every receipt from one of them report a failed outcome.
func TestBodySortsAndNormalizesWhateverTheStoreHandsBack(t *testing.T) {
	t.Parallel()
	store := &fakeRunStore{
		hosts: []run.HostSummary{
			{Host: "web03", Worst: "ok", OK: 3},
			{Host: "web01", Worst: "changed", OK: 1, Changed: 1},
			{Host: "db02", Worst: "failed", Failures: 2, Unreachable: 1, Skipped: 4},
		},
		tasks: []run.TaskSummary{
			{Task: "restart", Seconds: 0.0015},
			{Task: "apply", Seconds: 3},
			{Task: "gather", Seconds: 0.013500213},
		},
	}
	zone := time.FixedZone("CST", -6*60*60)
	started := time.Date(2026, 8, 17, 9, 30, 15, 0, zone)
	ended := started.Add(time.Minute)
	r := &run.Run{ID: "run_sorted", Status: run.StatusFailed, Playbook: "site.yml",
		StartedAt: &started, EndedAt: &ended}

	body, err := Body(context.Background(), store, r)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	rec, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	wantHosts := []RecordHost{
		{Host: "db02", Worst: "failed", Failures: 2, Unreachable: 1, Skipped: 4},
		{Host: "web01", Worst: "changed", OK: 1, Changed: 1},
		{Host: "web03", Worst: "ok", OK: 3},
	}
	if diff := cmp.Diff(wantHosts, rec.Hosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("hosts mismatch (-want +got):\n%s", diff)
	}
	wantTasks := []RecordTask{
		{Task: "apply", Milliseconds: 3000},
		{Task: "gather", Milliseconds: 14},
		{Task: "restart", Milliseconds: 2},
	}
	if diff := cmp.Diff(wantTasks, rec.Tasks, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("tasks mismatch (-want +got):\n%s", diff)
	}
	if rec.StartedAt == nil || rec.StartedAt.Location() != time.UTC {
		t.Errorf("started_at = %v, want it normalized to UTC so a rebuild matches", rec.StartedAt)
	}
	if !rec.StartedAt.Equal(started) || !rec.EndedAt.Equal(ended) {
		t.Errorf("timestamps moved: %v and %v, want the same instants as %v and %v",
			rec.StartedAt, rec.EndedAt, started, ended)
	}

	// The whole point is stable bytes, so building it twice must produce the same body.
	again, err := Body(context.Background(), store, r)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	if diff := cmp.Diff(string(body), string(again)); diff != "" {
		t.Errorf("the same run produced two records (-first +second):\n%s", diff)
	}
}

// TestBodyStatesAnAbsentExitCodeRatherThanOmittingIt pins that a run which never produced an exit
// code says so with a null rather than by leaving the field out. A verifier rebuilds these bytes,
// so the field has to be present either way; and "no exit code" and "exit code zero" are different
// claims about whether the run ever ran.
func TestBodyStatesAnAbsentExitCodeRatherThanOmittingIt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// ExitCode is what the run recorded.
		ExitCode *int
		// WantJSON is the fragment the body must carry.
		WantJSON string
	}{
		{Name: "absent", ExitCode: nil, WantJSON: `"exit_code":null`},     // Test 0: Never produced.
		{Name: "zero", ExitCode: intPtr(0), WantJSON: `"exit_code":0`},    // Test 1: A clean finish.
		{Name: "nonzero", ExitCode: intPtr(2), WantJSON: `"exit_code":2`}, // Test 2: A failure.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_1", Status: run.StatusFailed, ExitCode: test.ExitCode}
			body, err := Body(context.Background(), &fakeRunStore{}, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if !strings.Contains(string(body), test.WantJSON) {
				t.Errorf("body = %s, want it to carry %s", body, test.WantJSON)
			}
		})
	}
}

// intPtr returns a pointer to n, for the optional fields a record carries.
func intPtr(n int) *int { return &n }

// TestChildRecordsOrderAndIdentity pins the order a coordinator's children are recorded in and the
// dedupe that keeps one child from being counted twice.
//
// The record is hashed into the chain, so two orderings of the same runs would produce two digests
// for one history. A store that answers the shard read and the step read with the same children,
// which the in-memory store does, must still produce one entry per child.
func TestChildRecordsOrderAndIdentity(t *testing.T) {
	t.Parallel()
	step := func(id string, index *int, attempt int, name string) *run.Run {
		parent := "run_parent"
		return &run.Run{ID: id, ParentID: &parent, StepName: name, StepIndex: index,
			Attempt: attempt, Status: run.StatusSucceeded}
	}
	shard := func(id string, index *int) *run.Run {
		parent := "run_parent"
		return &run.Run{ID: id, ParentID: &parent, ShardIndex: index, Status: run.StatusSucceeded}
	}
	one, two := 1, 2

	tests := []struct {
		// Name labels the shape.
		Name string
		// Shards and Steps are what the store answers.
		Shards []*run.Run
		// Steps are the pipeline children.
		Steps []*run.Run
		// WantIDs are the child run ids in the order they must be recorded.
		WantIDs []string
	}{{ // Test 0: Steps out of order come back ordered by index.
		Name:    "ordered by index",
		Steps:   []*run.Run{step("run_b", &two, 0, "smoke"), step("run_a", &one, 0, "build")},
		WantIDs: []string{"run_a", "run_b"},
	}, { // Test 1: A retried step sorts after the attempt it retried, so the two are distinguishable.
		Name:    "ordered by attempt",
		Steps:   []*run.Run{step("run_retry", &one, 1, "build"), step("run_first", &one, 0, "build")},
		WantIDs: []string{"run_first", "run_retry"},
	}, { // Test 2: With index and attempt equal the run id breaks the tie, so the order is fixed
		// rather than incidental.
		Name:    "ordered by id",
		Steps:   []*run.Run{step("run_z", &one, 0, "s"), step("run_a", &one, 0, "s")},
		WantIDs: []string{"run_a", "run_z"},
	}, { // Test 3: A child carrying no index sorts ahead of the indexed ones, at minus one.
		Name:    "unindexed first",
		Steps:   []*run.Run{step("run_indexed", &one, 0, "s"), step("run_plain", nil, 0, "s")},
		WantIDs: []string{"run_plain", "run_indexed"},
	}, { // Test 4: The same children answered by both reads are recorded once, which is what the
		// in-memory store does.
		Name:    "deduped across reads",
		Shards:  []*run.Run{shard("run_s0", &one)},
		Steps:   []*run.Run{shard("run_s0", &one)},
		WantIDs: []string{"run_s0"},
	}, { // Test 5: A nil entry in the store's answer is skipped rather than dereferenced.
		Name:    "nil child skipped",
		Steps:   []*run.Run{nil, step("run_a", &one, 0, "s"), nil},
		WantIDs: []string{"run_a"},
	}, { // Test 6: No children at all leaves the field off an ordinary run's record.
		Name: "no children", WantIDs: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := &fakeRunStore{shards: test.Shards, steps: test.Steps}
			parent := &run.Run{ID: "run_parent", Kind: "pipeline", Status: run.StatusSucceeded}
			got, err := childRecords(context.Background(), store, parent)
			if err != nil {
				t.Fatalf("childRecords() error = %v", err)
			}
			var ids []string
			for _, c := range got {
				ids = append(ids, c.RunID)
			}
			if diff := cmp.Diff(test.WantIDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("child order mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestChildRecordsTakeTheShardIndexWhenThereIsNoStepIndex pins which position a child is recorded
// under. A run is either a split or a pipeline, so the record has one index field and it has to
// come from whichever the child actually carries; reading only the step index would leave every
// shard unindexed and collapse a fan-out into an arbitrary order.
func TestChildRecordsTakeTheShardIndexWhenThereIsNoStepIndex(t *testing.T) {
	t.Parallel()
	parent := "run_parent"
	zero, three := 0, 3
	store := &fakeRunStore{shards: []*run.Run{
		{ID: "run_shard3", ParentID: &parent, ShardIndex: &three, Status: run.StatusSucceeded},
		{ID: "run_shard0", ParentID: &parent, ShardIndex: &zero, Status: run.StatusFailed,
			ExitCode: intPtr(2)},
	}}
	got, err := childRecords(context.Background(), store,
		&run.Run{ID: parent, Kind: "split", Status: run.StatusFailed})
	if err != nil {
		t.Fatalf("childRecords() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("childRecords() returned %d children, want two shards", len(got))
	}
	if got[0].Index == nil || *got[0].Index != 0 || got[1].Index == nil || *got[1].Index != 3 {
		t.Errorf("shard indexes = %v and %v, want the shard positions 0 and 3",
			got[0].Index, got[1].Index)
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 2 {
		t.Errorf("the failed shard's exit code is %v, want 2", got[0].ExitCode)
	}
	if got[0].Name != "" {
		t.Errorf("shard name = %q, want empty, since a shard is identified by its index", got[0].Name)
	}
}

// TestChildRecordsNeverRollUpUnderAChild proves a run that is itself somebody's child records no
// children of its own. A child's own outcome commit is skipped precisely so its coordinator can
// roll it up, and a child that also rolled up would double count the same executions in the chain.
func TestChildRecordsNeverRollUpUnderAChild(t *testing.T) {
	t.Parallel()
	parent := "run_parent"
	store := &fakeRunStore{steps: []*run.Run{{ID: "run_grandchild", ParentID: &parent}}}
	child := &run.Run{ID: "run_child", ParentID: &parent, Status: run.StatusSucceeded}
	got, err := childRecords(context.Background(), store, child)
	if err != nil {
		t.Fatalf("childRecords() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("childRecords() under a child returned %+v, want nothing", got)
	}
}

// TestUtcOrNil pins the timestamp normalization the whole receipt path depends on. The record has
// to reduce to the same bytes whichever copy of the run it is built from, and the store reads every
// timestamp back as UTC while a run in memory carries the server's local offset.
func TestUtcOrNil(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// In is the timestamp the run carries.
		In *time.Time
	}{
		{Name: "nil", In: nil}, // Test 0: Never set.
		{Name: "utc", In: timePtr(time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC))},
		{Name: "west", In: timePtr(time.Date(2026, 8, 4, 6, 0, 0, 0, time.FixedZone("CST", -21600)))},
		{Name: "east", In: timePtr(time.Date(2026, 8, 4, 21, 0, 0, 0, time.FixedZone("JST", 32400)))},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := utcOrNil(test.In)
			if test.In == nil {
				if got != nil {
					t.Fatalf("utcOrNil(nil) = %v, want nil", got)
				}
				return
			}
			if got.Location() != time.UTC {
				t.Errorf("utcOrNil() location = %v, want UTC", got.Location())
			}
			if !got.Equal(*test.In) {
				t.Errorf("utcOrNil() = %v, want the same instant as %v", got, test.In)
			}
			if test.In.Location() != time.UTC && test.In.Location() == got.Location() {
				t.Error("the caller's own timestamp was rewritten in place")
			}
		})
	}
}

// timePtr returns a pointer to t, for the optional timestamps a run carries.
func timePtr(t time.Time) *time.Time { return &t }

// TestCommitRecordsTheOutcomeAgainstTheRunAndItsActor pins the chain entry a finished run writes:
// which run, which terminal status, who observed it, and on whose behalf. It is the only record
// that says what a run did rather than what was asked of it, so the entry has to name the run's
// path, the system actor that saw it finish, and the person the run was fired by.
func TestCommitRecordsTheOutcomeAgainstTheRunAndItsActor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Status is the terminal state the run reached.
		Status run.Status
	}{
		{Name: "succeeded", Status: run.StatusSucceeded},     // Test 0: The ordinary finish.
		{Name: "failed", Status: run.StatusFailed},           // Test 1: A non-zero exit.
		{Name: "canceled", Status: run.StatusCanceled},       // Test 2: Stopped by a person.
		{Name: "interrupted", Status: run.StatusInterrupted}, // Test 3: Settled by the sweep.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			audits := audit.NewMemStore()
			store := &fakeRunStore{}
			seeded := time.Date(2019, 3, 2, 1, 0, 0, 0, time.UTC)
			r := &run.Run{ID: "run_out", Status: test.Status, Playbook: "site.yml",
				Actor: "operator-jane"}

			if err := Commit(ctx, audits, store, r, "system:dispatcher",
				func() time.Time { return seeded }); err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			chain, err := audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			if len(chain) != 1 {
				t.Fatalf("the chain holds %d entries, want the one outcome", len(chain))
			}
			e := chain[0]
			if e.Method != audit.MethodRun {
				t.Errorf("method = %q, want %q", e.Method, audit.MethodRun)
			}
			wantPath := "/runs/run_out/outcome/" + string(test.Status)
			if diff := cmp.Diff(wantPath, e.Path); diff != "" {
				t.Errorf("path mismatch (-want +got):\n%s", diff)
			}
			if e.Actor != "system:dispatcher" || e.ActorType != "system" {
				t.Errorf("actor = %q/%q, want the observing process as a system actor",
					e.Actor, e.ActorType)
			}
			if e.OnBehalfOf != "operator-jane" {
				t.Errorf("on behalf of = %q, want the person the run was fired by", e.OnBehalfOf)
			}
			if !e.At.Equal(seeded) {
				t.Errorf("entry time = %v, want the clock the caller supplied %v", e.At, seeded)
			}
			body, err := Body(ctx, store, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if !audit.VerifyContentDigest(e.ContentDigest, e.Nonce, body) {
				t.Errorf("the rebuilt outcome does not match the digest the chain committed:\n%s",
					body)
			}
		})
	}
}

// TestCommitFallsBackToTheWallClock pins that a caller passing no clock still gets a timestamp. The
// dispatcher supplies its own so a seeded demo run's entry carries the same past instant its record
// does, and every other caller passes nil.
func TestCommitFallsBackToTheWallClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	before := time.Now().Add(-time.Second)
	r := &run.Run{ID: "run_now", Status: run.StatusSucceeded}
	if err := Commit(ctx, audits, &fakeRunStore{}, r, "system:dispatcher", nil); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("the chain holds %d entries, want one", len(chain))
	}
	if chain[0].At.Before(before) || chain[0].At.After(time.Now().Add(time.Second)) {
		t.Errorf("entry time = %v, want the current wall clock", chain[0].At)
	}
}

// TestCommitWritesNothingWhenTheRecordCannotBeBuilt proves a run whose evidence cannot be read
// leaves no outcome entry behind. An entry naming a terminal status whose body could not be built
// would be a claim about what a run did with nothing to check it against, which is worse than the
// caller being told the commit failed and retrying.
func TestCommitWritesNothingWhenTheRecordCannotBeBuilt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the failure.
		Name string
		// Store is the run store the commit reads from.
		Store *fakeRunStore
		// Run is the finished run.
		Run *run.Run
	}{{ // Test 0: The log cannot be streamed.
		Name: "log unreadable", Store: &fakeRunStore{logErr: errStore},
		Run: &run.Run{ID: "run_1", Status: run.StatusSucceeded},
	}, { // Test 1: The summaries cannot be read.
		Name: "summaries unreadable", Store: &fakeRunStore{hostErr: errStore},
		Run: &run.Run{ID: "run_1", Status: run.StatusSucceeded},
	}, { // Test 2: The spec will not encode, so the record cannot name what ran.
		Name: "spec unencodable", Store: &fakeRunStore{},
		Run: &run.Run{ID: "run_1", Status: run.StatusSucceeded,
			ExtraVars: map[string]any{"ratio": math.NaN()}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			audits := audit.NewMemStore()
			if err := Commit(ctx, audits, test.Store, test.Run, "system:dispatcher", nil); err == nil {
				t.Fatal("Commit() = nil error, want the failure surfaced to the caller")
			}
			chain, err := audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			if len(chain) != 0 {
				t.Errorf("the chain holds %d entries, want none for an outcome that failed to build",
					len(chain))
			}
		})
	}
}

// TestCommitSurfacesAnUnwritableChain proves an outcome that cannot be recorded is reported rather
// than swallowed. The dispatcher is the only thing that knows the run finished, so a silent failure
// here leaves the chain with a run that was asked for and no record of what it did.
func TestCommitSurfacesAnUnwritableChain(t *testing.T) {
	t.Parallel()
	audits := &failingAudits{Store: audit.NewMemStore()}
	r := &run.Run{ID: "run_1", Status: run.StatusSucceeded}
	err := Commit(context.Background(), audits, &fakeRunStore{}, r, "system:dispatcher", nil)
	if !errors.Is(err, errAppend) {
		t.Fatalf("Commit() error = %v, want the store's refusal reported", err)
	}
}

// TestBodyCarriesThePolicySetThatWasInForce proves the approval rules a run was submitted under
// reach the record. Without them the evidence could only show what a gate stopped: for a run
// nothing stopped, "no rule applied" and "there were no rules" left the same trace, so a gate
// deleted shortly beforehand was invisible afterward.
func TestBodyCarriesThePolicySetThatWasInForce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// PolicySet is what the run recorded at submission.
		PolicySet *run.PolicySet
	}{{ // Test 0: A run submitted before this was recorded carries nothing, and says so.
		Name: "absent", PolicySet: nil,
	}, { // Test 1: An install with no rules at all, which is a different claim from absent.
		Name: "no rules", PolicySet: &run.PolicySet{Digest: "sha256:empty", Count: 0},
	}, { // Test 2: The rules as they read at submission, so the evidence needs no server to explain
		// what a digest meant.
		Name: "rules in force", PolicySet: &run.PolicySet{Digest: "sha256:abc", Count: 2,
			Rules: []string{"hold production", "require distinct approver"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_1", Status: run.StatusSucceeded, PolicySet: test.PolicySet}
			body, err := Body(context.Background(), &fakeRunStore{}, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if diff := cmp.Diff(test.PolicySet, rec.PolicySet); diff != "" {
				t.Errorf("policy set mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBodyCommitsTheResolvedImageEvenThoughTheSpecDoesNot proves the container the run actually
// executed in reaches the record. The spec deliberately leaves the image out, because it is
// resolved onto the run after approval from a project or server default, so this record is the only
// place the chain learns which image ran.
func TestBodyCommitsTheResolvedImageEvenThoughTheSpecDoesNot(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run_1", Status: run.StatusSucceeded, Tool: "ansible",
		Image: "registry.example/ansible:2026.09", CommitSHA: "cafebabe", DryRun: true}
	body, err := Body(context.Background(), &fakeRunStore{}, r)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	rec, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if rec.Image != r.Image {
		t.Errorf("image = %q, want the resolved %q, which the spec digest does not cover",
			rec.Image, r.Image)
	}
	if rec.CommitSHA != r.CommitSHA {
		t.Errorf("commit sha = %q, want the content that ran %q", rec.CommitSHA, r.CommitSHA)
	}
	if !rec.DryRun {
		t.Error("dry_run is absent, so a preview could later be presented as the change itself")
	}
	specDigest, err := SpecDigest(r)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	if rec.SpecDigest != specDigest {
		t.Errorf("spec digest = %q, want the run's own %q so the approval and the outcome meet",
			rec.SpecDigest, specDigest)
	}
}

// TestBodyIsValidJSONForEveryTerminalStatus is a cheap guard that the record a verifier is handed
// parses whatever the run reached, including the statuses that produce no exit code and no
// timestamps at all.
func TestBodyIsValidJSONForEveryTerminalStatus(t *testing.T) {
	t.Parallel()
	statuses := []run.Status{run.StatusSucceeded, run.StatusFailed, run.StatusCanceled,
		run.StatusInterrupted, run.StatusRejected}
	for testNum, status := range statuses {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_1", Status: status}
			body, err := Body(context.Background(), &fakeRunStore{}, r)
			if err != nil {
				t.Fatalf("Body() error = %v", err)
			}
			if !json.Valid(body) {
				t.Fatalf("Body() produced invalid JSON: %s", body)
			}
			rec, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if rec.Status != string(status) {
				t.Errorf("status = %q, want %q", rec.Status, status)
			}
		})
	}
}

// TestChildRecordsRefuseAChildWhoseLogCannotBeDigested proves a coordinator's record is not built
// when one of its children's output cannot be read. Each child's log digest is what lets a reader
// hold the stored output of any one step against the receipt, so a record that dropped the digest
// of the step it could not read would be a receipt quietly missing the evidence for one execution.
func TestChildRecordsRefuseAChildWhoseLogCannotBeDigested(t *testing.T) {
	t.Parallel()
	parent := "run_parent"
	index := 0
	store := &fakeRunStore{
		logErrForID: "run_step_a",
		steps: []*run.Run{{ID: "run_step_a", ParentID: &parent, StepName: "build",
			StepIndex: &index, Status: run.StatusSucceeded}},
	}
	coordinator := &run.Run{ID: parent, Kind: "pipeline", Status: run.StatusSucceeded}

	got, err := childRecords(context.Background(), store, coordinator)
	if !errors.Is(err, errStore) {
		t.Fatalf("childRecords() error = %v, want the child log failure reported", err)
	}
	if got != nil {
		t.Errorf("childRecords() returned %+v beside its error, want nothing", got)
	}
	if _, err := Body(context.Background(), store, coordinator); !errors.Is(err, errStore) {
		t.Errorf("Body() error = %v, want the same failure to stop the whole record", err)
	}
}
