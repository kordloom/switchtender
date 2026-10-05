package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// fakeLimits answers limit evaluations from a fixed table, the way ansible-inventory would for the
// callback inventory, and counts how often it was asked.
type fakeLimits struct {
	// mu guards calls.
	mu sync.Mutex
	// byLimit maps a limit pattern to the hosts it selects.
	byLimit map[string][]string
	// err fails every evaluation when set.
	err error
	// calls counts evaluations.
	calls int
}

// LimitHosts returns the hosts the table gives limit, or the configured error.
func (f *fakeLimits) LimitHosts(_ context.Context, _, limit string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	hosts, ok := f.byLimit[limit]
	if !ok {
		return nil, errors.New("no such limit in the table")
	}
	return hosts, nil
}

// asked returns how many evaluations the fake answered.
func (f *fakeLimits) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// webLimit is the evaluation of the limits the callback tests' templates carry, against
// callbackInventory.
func webLimit() *fakeLimits {
	return &fakeLimits{byLimit: map[string][]string{
		"web":         {"db01.example.com", "web01", "web02"},
		"web:!web02":  {"db01.example.com", "web01"},
		"pair":        {"dup-a", "dup-b"},
		"nothing-yet": nil,
	}}
}

// TestCallbackKeepsTheTemplatesLimit pins decision seventeen. By default a callback launches only
// when the calling host also falls within the template's own limit, decided by Ansible's reading of
// the pattern, and is refused with the reason otherwise. A template set to replace launches for any
// matched host, as AWX does, and a template with no limit launches either way without asking
// Ansible anything.
func TestCallbackKeepsTheTemplatesLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Limits      func() *fakeLimits
		Mode        string
		Limit       string
		Addr        string
		WantBody    string
		WantHost    string
		WantStatus  int
		WantAsked   bool
		NoEvaluator bool
	}{{ // Test 0: The default keeps the limit, and a host inside it launches.
		Limit: "web", Addr: "10.0.0.11", WantStatus: http.StatusCreated, WantHost: "web01",
		WantAsked: true,
	}, { // Test 1: A host outside the limit is refused, and told how to change that.
		Limit: "web:!web02", Addr: "10.0.0.12", WantStatus: http.StatusForbidden,
		WantBody: "outside this template's limit", WantAsked: true,
	}, { // Test 2: Saying intersect is the same as saying nothing.
		Mode: template.CallbackLimitIntersect, Limit: "web:!web02", Addr: "10.0.0.12",
		WantStatus: http.StatusForbidden, WantBody: "callback_limit intersect", WantAsked: true,
	}, { // Test 3: Replace launches for the matched host whatever the limit says, as AWX does.
		Mode: template.CallbackLimitReplace, Limit: "web:!web02", Addr: "10.0.0.12",
		WantStatus: http.StatusCreated, WantHost: "web02",
	}, { // Test 4: A template with no limit launches without asking Ansible.
		Addr: "10.0.0.12", WantStatus: http.StatusCreated, WantHost: "web02",
	}, { // Test 5: A limit nothing can evaluate refuses rather than guessing.
		Limit: "web", Addr: "10.0.0.11", NoEvaluator: true, WantStatus: http.StatusConflict,
		WantBody: "needs ansible-inventory",
	}, { // Test 6: An evaluation that fails refuses rather than guessing.
		Limit: "web", Addr: "10.0.0.11", WantStatus: http.StatusServiceUnavailable,
		WantBody: "could not be checked", WantAsked: true,
		Limits: func() *fakeLimits { return &fakeLimits{err: errors.New("ansible failed")} },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			limits := webLimit()
			if test.Limits != nil {
				limits = test.Limits()
			}
			var opts []Option
			if test.NoEvaluator {
				opts = append(opts, func(s *Server) { s.callbackCfg.limits = nil })
			} else {
				opts = append(opts, WithCallbackLimitMatcher(limits))
			}
			sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
			f := newCallbackFixture(t, sub, fakeResolver{}, &template.Template{ID: "tpl_cb",
				Name: "boot", Playbook: "boot.yml", InventoryID: "inv_1", AllowCallbacks: true,
				Limit: test.Limit, CallbackLimit: test.Mode, CreatedAt: time.Now()}, opts...)
			key := f.mintKey(t, "tpl_cb", nil)
			rec := f.callBack("tpl_cb", test.Addr, "application/json", keyBody(key))
			if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantBody) {
				t.Fatalf("status = %d body %s, want %d containing %q", rec.Code, rec.Body.String(),
					test.WantStatus, test.WantBody)
			}
			if asked := limits.asked() > 0; asked != test.WantAsked {
				t.Errorf("Ansible asked = %v, want %v", asked, test.WantAsked)
			}
			if test.WantHost == "" {
				if sub.gotRun != nil {
					t.Errorf("a refused callback launched %+v", sub.gotRun)
				}
				return
			}
			if sub.gotRun == nil || sub.gotRun.Limit != test.WantHost {
				t.Errorf("launched %+v, want a run limited to %s", sub.gotRun, test.WantHost)
			}
		})
	}
}

// TestLimitCacheAsksOncePerInventoryAndLimit pins that a fleet booting at once costs one
// evaluation per inventory and limit, not one per host: callers arriving together share the
// answer, a different inventory or limit is asked afresh, an answer is reused only while it is
// young, and a failure is never kept.
func TestLimitCacheAsksOncePerInventoryAndLimit(t *testing.T) {
	t.Parallel()
	limits := webLimit()
	cache := newLimitCache(limits)
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.hosts(ctx, callbackInventory, "web"); err != nil {
				t.Errorf("hosts() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := limits.asked(); got != 1 {
		t.Errorf("twenty callers together asked Ansible %d times, want once", got)
	}
	steps := []struct {
		Content   string
		Limit     string
		Advance   time.Duration
		WantAsked int
	}{{ // Test 0: The same inventory and limit again is answered from the cache.
		Content: callbackInventory, Limit: "web", WantAsked: 1,
	}, { // Test 1: Another limit is asked afresh.
		Content: callbackInventory, Limit: "web:!web02", WantAsked: 2,
	}, { // Test 2: An edited inventory is asked afresh.
		Content: callbackInventory + "web03\n", Limit: "web", WantAsked: 3,
	}, { // Test 3: An answer older than its life is asked afresh.
		Content: callbackInventory, Limit: "web", Advance: time.Minute, WantAsked: 4,
	}, { // Test 4: A failed evaluation is asked once.
		Content: callbackInventory, Limit: "missing", WantAsked: 5,
	}, { // Test 5: A failure is never kept, so the same question is asked again.
		Content: callbackInventory, Limit: "missing", WantAsked: 6,
	}}
	for testNum, step := range steps {
		now = now.Add(step.Advance)
		_, _ = cache.hosts(ctx, step.Content, step.Limit)
		if got := limits.asked(); got != step.WantAsked {
			t.Errorf("test %d: Ansible asked %d times in all, want %d", testNum, got, step.WantAsked)
		}
	}
}
