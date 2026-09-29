package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// tailFixture writes content to a sidecar file in a temp directory and returns its path.
func tailFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.ndjson")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// tailOnce runs the tailer over an already complete file and returns the events the store recorded
// for the run. The stop channel is closed straight away, so the run is the final drain: exactly what
// happens when a tool exits and the executor stops the tailer.
func tailOnce(t *testing.T, d *Dispatcher, id, parent, path string) []event.Event {
	t.Helper()
	stop := make(chan struct{})
	close(stop)
	d.tailEvents(id, parent, path, stop, &masker{}, run.NewSummaryFold(time.Now()))
	got, err := d.store.Events(context.Background(), id)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	return got
}

// newTailDispatcher builds the smallest dispatcher the tailer needs, over a run already in the store.
func newTailDispatcher(t *testing.T, id string, pub Publisher) *Dispatcher {
	t.Helper()
	store := run.NewMemStore()
	if err := store.Save(context.Background(), &run.Run{
		ID: id, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if pub == nil {
		pub = noopPublisher{}
	}
	return &Dispatcher{store: store, log: zap.NewNop(), publisher: pub}
}

// TestTailEventsKeepsTheLastLineOfAKilledTool pins the final drain's one special case. A tool that is
// killed mid-write leaves its last event without a trailing newline, and that event describes the
// moment the run was stopped, which is the part somebody investigating actually wants. Dropping it
// because the writer never finished the line loses exactly the evidence a cancel or a timeout is
// about.
func TestTailEventsKeepsTheLastLineOfAKilledTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says how the file ended.
		Name string
		// Content is what the tool managed to write.
		Content string
		// WantTasks are the task names of the events that must be recorded, in order.
		WantTasks []string
	}{{ // Test 0: A clean file ending in a newline records every line.
		Name: "a clean file",
		Content: `{"type":"task_start","task":"first"}` + "\n" +
			`{"type":"task_start","task":"second"}` + "\n",
		WantTasks: []string{"first", "second"},
	}, { // Test 1: A killed tool's unterminated last line is kept, not dropped.
		Name: "cut off mid-write",
		Content: `{"type":"task_start","task":"first"}` + "\n" +
			`{"type":"task_start","task":"cut off here"}`,
		WantTasks: []string{"first", "cut off here"},
	}, { // Test 2: A file holding only an unterminated line still records it.
		Name:      "one unterminated line",
		Content:   `{"type":"task_start","task":"only"}`,
		WantTasks: []string{"only"},
	}, { // Test 3: An empty file records nothing and does not fail.
		Name: "an empty file", Content: "", WantTasks: nil,
	}, { // Test 4: Blank lines are not events and are skipped without error.
		Name:      "blank lines only",
		Content:   "\n\n   \n",
		WantTasks: nil,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := newTailDispatcher(t, "run_tail", nil)
			got := tailOnce(t, d, "run_tail", "", tailFixture(t, test.Content))

			var tasks []string
			for _, e := range got {
				tasks = append(tasks, e.Task)
			}
			if diff := cmp.Diff(test.WantTasks, tasks, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("recorded task names (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTailEventsSkipsOneDamagedLineAndKeepsTheRest pins the batch's tolerance. A sidecar file can
// hold a truncated or malformed line from a tool that died partway through writing it, and refusing
// the whole batch over one bad line would throw away every good event that arrived with it, which is
// the run's entire timeline for that tick.
func TestTailEventsSkipsOneDamagedLineAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	content := `{"type":"task_start","task":"before"}` + "\n" +
		`{"type":"task_start","task":` + "\n" + // truncated JSON, unparseable
		`not json at all` + "\n" +
		`{"type":"task_start","task":"after"}` + "\n"

	d := newTailDispatcher(t, "run_damaged", nil)
	got := tailOnce(t, d, "run_damaged", "", tailFixture(t, content))

	var tasks []string
	for _, e := range got {
		tasks = append(tasks, e.Task)
	}
	if diff := cmp.Diff([]string{"before", "after"}, tasks, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("recorded task names (-want +got):\n%s\none damaged line must not discard the "+
			"good events in the same batch", diff)
	}
}

// TestTailEventsSurvivesAnUnreadableSidecar pins the two ways the sidecar can be unusable. Capture
// being unavailable degrades the run to producing no events, which the run records separately; what
// it must not do is block the tailer forever or crash the executor, since the tool itself is fine.
func TestTailEventsSurvivesAnUnreadableSidecar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which unusable sidecar is in play.
		Name string
		// Path is what the tailer is given.
		Path func(t *testing.T) string
	}{{ // Test 0: No path at all, which is what a failed temp-file creation leaves.
		Name: "no path", Path: func(*testing.T) string { return "" },
	}, { // Test 1: A path to a file that does not exist.
		Name: "a missing file",
		Path: func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "never-created.ndjson")
		},
	}, { // Test 2: A path that is a directory, which cannot be read as a line stream.
		Name: "a directory",
		Path: func(t *testing.T) string {
			t.Helper()
			return t.TempDir()
		},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := newTailDispatcher(t, "run_unreadable", nil)
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				d.tailEvents("run_unreadable", "", test.Path(t), stop,
					&masker{}, run.NewSummaryFold(time.Now()))
			}()

			// The tailer must still be waiting on stop rather than having returned or wedged.
			close(stop)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the tailer never returned, so every run with an unusable sidecar leaks a " +
					"goroutine and the executor never finishes")
			}

			got, err := d.store.Events(context.Background(), "run_unreadable")
			if err != nil {
				t.Fatalf("Events() error = %v", err)
			}
			if len(got) != 0 {
				t.Errorf("%d events recorded from an unreadable sidecar", len(got))
			}
		})
	}
}

// TestTailEventsEchoesToTheParentTopicOnly pins where a child's events are published. A split or
// pipeline coordinator keeps no event log of its own, so its page streams live only because each
// child echoes to the parent topic. The events belong to the child's own log, though, so the parent
// must receive them for streaming without them being stored twice.
func TestTailEventsEchoesToTheParentTopicOnly(t *testing.T) {
	t.Parallel()
	pub := newCapturingPublisher()
	d := newTailDispatcher(t, "run_child", pub)

	content := `{"type":"task_start","task":"one"}` + "\n" +
		`{"type":"task_start","task":"two"}` + "\n"
	got := tailOnce(t, d, "run_child", "run_parent", tailFixture(t, content))

	if len(got) != 2 {
		t.Fatalf("the child stored %d events, want 2", len(got))
	}
	if n := pub.eventCount("run_child"); n != 2 {
		t.Errorf("child topic received %d events, want 2", n)
	}
	if n := pub.eventCount("run_parent"); n != 2 {
		t.Errorf("parent topic received %d events, want 2: a coordinator has no event log of its "+
			"own, so its page goes blank without the echo", n)
	}

	// The parent's own stored events stay empty; the echo is for streaming, not a second copy.
	parentEvents, err := d.store.Events(context.Background(), "run_parent")
	if err == nil && len(parentEvents) != 0 {
		t.Errorf("%d events were stored against the parent, want none", len(parentEvents))
	}
}

// TestTailEventsWithNoParentPublishesOneTopic is the other half: a top-level run must not publish
// under an empty topic id, which would leak every run's events into one shared stream.
func TestTailEventsWithNoParentPublishesOneTopic(t *testing.T) {
	t.Parallel()
	pub := newCapturingPublisher()
	d := newTailDispatcher(t, "run_solo", pub)

	tailOnce(t, d, "run_solo", "", tailFixture(t,
		`{"type":"task_start","task":"one"}`+"\n"))

	if n := pub.eventCount("run_solo"); n != 1 {
		t.Errorf("own topic received %d events, want 1", n)
	}
	if n := pub.eventCount(""); n != 0 {
		t.Errorf("%d events were published under the empty topic, so every run's events land in "+
			"one shared stream", n)
	}
}

// appendFailStore refuses to store events and counts the refusals.
type appendFailStore struct {
	run.Store
	// calls counts refused appends.
	calls atomic.Int64
}

// AppendEvents refuses every append.
func (s *appendFailStore) AppendEvents(context.Context, string, []event.Event) error {
	s.calls.Add(1)
	return errors.New("database is locked")
}

// TestTailEventsStillStreamsWhenTheStoreRefusesThem pins that a storage fault degrades rather than
// tears down. The run is executing on real hosts; losing its event log is bad, but stopping the tool
// because the log could not be written would be worse, and a live viewer should still see what is
// happening.
func TestTailEventsStillStreamsWhenTheStoreRefusesThem(t *testing.T) {
	t.Parallel()
	pub := newCapturingPublisher()
	base := run.NewMemStore()
	if err := base.Save(context.Background(), &run.Run{
		ID: "run_nostore", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	store := &appendFailStore{Store: base}
	d := &Dispatcher{store: store, log: zap.NewNop(), publisher: pub}

	stop := make(chan struct{})
	close(stop)
	d.tailEvents("run_nostore", "", tailFixture(t,
		`{"type":"task_start","task":"one"}`+"\n"), stop, &masker{}, run.NewSummaryFold(time.Now()))

	if store.calls.Load() == 0 {
		t.Fatal("the store was never asked to record the events")
	}
	if n := pub.eventCount("run_nostore"); n != 1 {
		t.Errorf("live topic received %d events, want 1: a storage fault must not blind the viewer "+
			"watching a change land on real hosts", n)
	}
}

// TestTailEventsRedactsBeforeItStores pins the order of masking and storage. The masker holds the
// values the run's credentials resolved to, and an event field carries whatever a task printed, so a
// secret echoed into a task result must never reach the stored log or a live viewer. Storing first
// and masking later would leave the plaintext in the database forever.
func TestTailEventsRedactsBeforeItStores(t *testing.T) {
	t.Parallel()
	pub := newCapturingPublisher()
	d := newTailDispatcher(t, "run_secret", pub)

	mask := &masker{}
	mask.set([]string{"hunter2-the-real-password"})

	content := `{"type":"runner_ok","host":"web01","task":"echo",` +
		`"stdout":"token=hunter2-the-real-password done"}` + "\n"
	stop := make(chan struct{})
	close(stop)
	d.tailEvents("run_secret", "", tailFixture(t, content), stop, mask,
		run.NewSummaryFold(time.Now()))

	got, err := d.store.Events(context.Background(), "run_secret")
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d events, want 1", len(got))
	}
	if got[0].Stdout == "" || got[0].Stdout == "token=hunter2-the-real-password done" {
		t.Errorf("stored stdout = %q, want the secret replaced before it reached the store",
			got[0].Stdout)
	}
	if want := "token=" + maskToken + " done"; got[0].Stdout != want {
		t.Errorf("stored stdout = %q, want %q", got[0].Stdout, want)
	}
}

// TestTailEventsFollowsAFileWhileItGrows pins the polling half of the tailer, which is how a live run
// streams at all. The file is written after the tailer has started, exactly as a running tool writes
// it, and every complete line must arrive before the final drain rather than only at the end.
func TestTailEventsFollowsAFileWhileItGrows(t *testing.T) {
	t.Parallel()
	pub := newCapturingPublisher()
	d := newTailDispatcher(t, "run_growing", pub)

	path := tailFixture(t, "")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.tailEvents("run_growing", "", path, stop, &masker{}, run.NewSummaryFold(time.Now()))
	}()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	for i := range 3 {
		if _, werr := fmt.Fprintf(f, `{"type":"task_start","task":"t%d"}`+"\n", i); werr != nil {
			t.Fatalf("write %d error = %v", i, werr)
		}
		time.Sleep(2 * tailPollInterval)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close() error = %v", cerr)
	}

	// The lines were flushed while the run was still live, before the tailer was told to stop.
	deadline := time.Now().Add(10 * time.Second)
	for pub.eventCount("run_growing") < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := pub.eventCount("run_growing"); n < 3 {
		t.Errorf("only %d events streamed while the run was live, want 3: a run's page stays blank "+
			"until it finishes if the tailer only drains at the end", n)
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tailer did not stop when it was told to")
	}
}

// TestFlushEventLinesIgnoresAnEmptyBatch pins the short-circuit before the store write. The tailer
// calls this on every poll tick, most of which have nothing new, so a batch of nothing must not cost
// a write against a store whose single writer the executing runs are contending for.
func TestFlushEventLinesIgnoresAnEmptyBatch(t *testing.T) {
	t.Parallel()
	base := run.NewMemStore()
	store := &appendFailStore{Store: base}
	pub := newCapturingPublisher()
	d := &Dispatcher{store: store, log: zap.NewNop(), publisher: pub}

	fold := run.NewSummaryFold(time.Now())
	d.flushEventLines("run_x", "run_p", nil, &masker{}, fold)
	d.flushEventLines("run_x", "run_p", [][]byte{}, &masker{}, fold)
	// Lines that parse to nothing are the same as no lines at all.
	d.flushEventLines("run_x", "run_p", [][]byte{[]byte("\n"), []byte("   \n")}, &masker{}, fold)

	if got := store.calls.Load(); got != 0 {
		t.Errorf("%d store writes for a batch with no events in it", got)
	}
	if got := pub.eventCount("run_x"); got != 0 {
		t.Errorf("%d events published from an empty batch", got)
	}
}
