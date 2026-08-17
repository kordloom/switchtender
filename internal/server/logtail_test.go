package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/run"
)

// wholeLogRefused is a run store that refuses the whole-log read and serves the chunked one, so a test
// can prove which path a caller took. A store cannot tell a caller how much memory it just cost them;
// refusing the expensive call is how that becomes visible in a test.
type wholeLogRefused struct {
	run.Store
	// reads counts how many times the whole log was asked for.
	reads int
}

// Log refuses, standing in for a run whose log is far too large to hold.
func (w *wholeLogRefused) Log(context.Context, string) ([]byte, error) {
	w.reads++
	return nil, errors.New("the whole log was read into memory")
}

// TestExplainReadsOnlyTheLogTail covers a read whose cost scales with the wrong thing. Explaining a
// failure sends the model the last few kilobytes of output, and the handler got them by reading the
// entire log into memory and slicing the end off. A run that fails after producing a gigabyte of output,
// which is exactly the kind of run somebody asks for an explanation of, allocated that gigabyte on the
// control node to use six kilobytes of it, and any viewer could ask.
func TestExplainReadsOnlyTheLogTail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	// The run is saved as running first: the store fences appends to a terminal run, which is right for
	// a reclaimed worker and means a test has to write the log while the run is still open.
	failing := &run.Run{ID: "run_1", Status: run.StatusRunning, Tool: "ansible", Playbook: "site.yml"}
	if err := backing.Save(ctx, failing); err != nil {
		t.Fatalf("Save run: %v", err)
	}
	// Many chunks, as a long run produces: the tail is spread over the last few of them.
	for i := 0; i < 500; i++ {
		line := fmt.Sprintf("line %04d %s\n", i, strings.Repeat("x", 100))
		if err := backing.AppendLog(ctx, "run_1", []byte(line)); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}
	failing.Status = run.StatusFailed
	if err := backing.Save(ctx, failing); err != nil {
		t.Fatalf("Save run: %v", err)
	}
	store := &wholeLogRefused{Store: backing}

	tail := logTail(ctx, store, "run_1", explainLogTail)
	if store.reads != 0 {
		t.Errorf("the whole log was read %d time(s), so the cost of an explanation scales with the "+
			"size of the log rather than with the size of the tail", store.reads)
	}
	if len(tail) == 0 {
		t.Fatal("no log tail was read at all, so an explanation would carry no output")
	}
	if len(tail) > explainLogTail {
		t.Errorf("tail is %d bytes, want at most %d", len(tail), explainLogTail)
	}
	// It is the end of the log, which is the part that says how the run failed.
	if !strings.Contains(string(tail), "line 0499") {
		t.Errorf("the tail does not hold the last line written:\n%s", tail)
	}
	if strings.Contains(string(tail), "line 0000") {
		t.Error("the tail reaches back to the first line, so it is not a tail")
	}

	// The handler takes that path too, not only this helper. A whole-log read here is the actual
	// regression: the tail reader existing while the handler ignores it fixes nothing.
	var prompt string
	provider := ai.ProviderFunc(func(_ context.Context, _, user string) (string, error) {
		prompt = user
		return "the play failed on web-1.", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_1/explain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explain status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if store.reads != 0 {
		t.Errorf("explaining a run read the whole log %d time(s), so the handler does not use the "+
			"bounded tail read", store.reads)
	}
	if !strings.Contains(prompt, "line 0499") {
		t.Errorf("the prompt carries no log tail:\n%s", prompt)
	}

	// A run with no log at all is not an error, since plenty of runs fail before writing anything.
	if err := backing.Save(ctx, &run.Run{ID: "run_2", Status: run.StatusFailed}); err != nil {
		t.Fatalf("Save run: %v", err)
	}
	if got := logTail(ctx, store, "run_2", explainLogTail); len(got) != 0 {
		t.Errorf("a run with no log produced %d bytes of tail", len(got))
	}
}
