package dispatch

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// recordingPublisher captures everything the dispatcher streams, so a test can hold the live
// output path to its contract instead of trusting the noop default to stand in for it.
type recordingPublisher struct {
	// mu guards the three captures.
	mu sync.Mutex
	// logs collects every chunk per run id.
	logs map[string][]byte
	// events counts events per run id.
	events map[string]int
	// closed counts CloseRun calls per run id.
	closed map[string]int
}

// newRecordingPublisher returns an empty recorder.
func newRecordingPublisher() *recordingPublisher {
	return &recordingPublisher{logs: map[string][]byte{}, events: map[string]int{}, closed: map[string]int{}}
}

// PublishEvents counts delivered events.
func (p *recordingPublisher) PublishEvents(id string, evs []event.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events[id] += len(evs)
}

// PublishLog appends the chunk.
func (p *recordingPublisher) PublishLog(id string, chunk []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs[id] = append(p.logs[id], chunk...)
}

// CloseRun counts the close.
func (p *recordingPublisher) CloseRun(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed[id]++
}

// TestPublisherReceivesOutputAndExactlyOneClose pins the streaming seam a browser tab lives on:
// a run's output reaches the publisher as it is written, its events follow, and the stream is
// closed exactly once when the run ends.
//
// The seam had no test driving a real publisher; every suite ran on the noop default, so a
// dispatcher that stopped publishing, published after close, or closed twice, which a client
// treats as the stream ending early, would have kept every test green while live views went
// blank.
func TestPublisherReceivesOutputAndExactlyOneClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	pub := newRecordingPublisher()
	// The runner writes both channels the way ansible does: text to stdout, and structured
	// events as ndjson into the side file the dispatcher tails, which is the real event path.
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "PLAY RECAP *****\nweb01 : ok=1 changed=0\n")
			if spec.EventsPath != "" {
				lines := `{"type":"task_start","ts":1719000000,"play":"seam","task":"touch"}
{"type":"runner_ok","ts":1719000001,"play":"seam","task":"touch","host":"web01","changed":false}
`
				if err := os.WriteFile(spec.EventsPath, []byte(lines), 0o600); err != nil {
					return roundhouse.Result{ExitCode: 1}, err
				}
			}
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithPublisher(pub))
	defer d.Close()

	created, err := d.Submit(ctx, "site.yml", "hosts.ini")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := waitTerminal(t, store, created.ID); got.Status != run.StatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pub.mu.Lock()
		closed := pub.closed[created.ID]
		pub.mu.Unlock()
		if closed > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if !strings.Contains(string(pub.logs[created.ID]), "PLAY RECAP") {
		t.Errorf("the publisher never received the run's output; a live view of this run is blank")
	}
	if pub.events[created.ID] == 0 {
		t.Errorf("the publisher received no parsed events; the live matrix has nothing to draw")
	}
	if got := pub.closed[created.ID]; got != 1 {
		t.Errorf("CloseRun was called %d time(s), want exactly 1: zero strands every client on a "+
			"dead stream, and a second close ends a reconnected client's stream early", got)
	}
}
