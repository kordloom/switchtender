package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// postPlanFile posts a propose request carrying plan to url as the worker pool would, and returns
// the status and the error the control node stated.
func postPlanFile(t *testing.T, url string, plan []byte) (int, string) {
	t.Helper()
	body, err := json.Marshal(proposeApplyRequest{Destroys: 3, Read: true, PlanFile: plan})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		url+"/relay/v1/runs/run_plan/propose-apply", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer swt_worker")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post propose: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, readErrorBody(resp.Body)
}

// TestAPlanFileOverTheRelayLimitIsRefusedWithTheLimitStated pins the largest plan file a relay
// worker can hand the control node, and that every refusal of a larger one says what the limit is.
// A worker refuses it before sending anything. A control node handed one anyway refuses it, whether
// the request arrived whole or was cut off at the upload limit. A plan file at the limit crosses.
func TestAPlanFileOverTheRelayLimitIsRefusedWithTheLimitStated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	over := make([]byte, MaxPlanFileBytes+1)

	// Test 0: The worker refuses before sending, with the size and the limit stated.
	var sent atomic.Int64
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(silent.Close)
	worker := NewHTTPTransport(silent.URL, "swt_worker", nil)
	_, err := worker.ProposeApply(ctx, "run_plan", 3, true, over)
	if !errors.Is(err, ErrPlanFileTooLarge) || !strings.Contains(err.Error(), "at most 19 MiB") {
		t.Errorf("worker ProposeApply() error = %v, want ErrPlanFileTooLarge stating 19 MiB", err)
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("the worker sent %d requests carrying a plan file past the limit, want none", n)
	}

	// Test 1: The control node refuses a whole request carrying one.
	_, _, baseURL := planFixture(t)
	status, msg := postPlanFile(t, baseURL, over)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "at most 19 MiB") {
		t.Errorf("control node answered %d %q, want 413 stating 19 MiB", status, msg)
	}

	// Test 2: The control node refuses a request its upload limit cut off, stating the limit.
	policies := policy.NewMemStore()
	handler := NewHandler(run.NewMemStore(), SinglePool("swt_worker"), zap.NewNop(), policies, nil,
		WithPlanSealer(testPlanSealer{}))
	capped := httptest.NewServer(http.MaxBytesHandler(handler, 1<<10))
	t.Cleanup(capped.Close)
	status, msg = postPlanFile(t, capped.URL, make([]byte, 4<<10))
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "at most 19 MiB") {
		t.Errorf("cut off request answered %d %q, want 413 stating 19 MiB", status, msg)
	}

	// Test 3: A plan file at the limit crosses and proposes its apply.
	client, _, _ := planFixture(t)
	proposal, err := client.ProposeApply(ctx, "run_plan", 3, true, make([]byte, MaxPlanFileBytes))
	if err != nil || proposal == nil || proposal.PlanSHA256 == "" {
		t.Errorf("ProposeApply() at the limit = %v, want a proposal carrying the plan file", err)
	}
}
