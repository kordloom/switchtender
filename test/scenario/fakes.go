package scenario

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// Fake is a stand-in for an outside system, built from a scenario's behavior knobs.
//
// A fake is selected by name from a registry rather than loaded dynamically, so the compiler still
// checks it and a scenario naming one that does not exist is refused at load rather than passing
// vacuously. Every fake that has a real counterpart the product also talks to carries a contract
// test holding the two to the same answers on the paths both support, because a fake that has
// drifted from the real thing is a suite testing itself.
type Fake interface {
	// Start brings the stand-in up and returns the address or handle the install reaches it at,
	// empty when the fake is not addressed by URL.
	Start() (address string, err error)
	// Stop releases whatever Start took.
	Stop()
	// Calls reports how many times the install reached it, which is how a scenario asserts that a
	// dependency was consulted at all.
	Calls() int
}

// FakeFactory builds a fake from the behavior a scenario declared.
type FakeFactory func(Behavior) Fake

// fakes is the registry. A name here is what a scenario's "go:name" provider resolves to.
var fakes = map[string]FakeFactory{
	"submitter": func(b Behavior) Fake { return &fakeSubmitter{gate: gate{behavior: b}} },
	"vault":     func(b Behavior) Fake { return newFakeHTTP(b, vaultRoutes) },
	"http":      func(b Behavior) Fake { return newFakeHTTP(b, echoRoutes) },
}

// knownFake reports whether a name is registered, so the loader can refuse one that is not.
func knownFake(name string) bool {
	_, ok := fakes[name]
	return ok
}

// FakeNames lists the registered stand-ins, for the guard that every one is contract tested.
func FakeNames() []string {
	out := make([]string, 0, len(fakes))
	for name := range fakes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// newFake builds the stand-in a dependency declaration selects.
func newFake(name string, dep Dependency) (Fake, error) {
	provider := dep.Provider
	if provider == "" {
		provider = "go:" + name
	}
	const prefix = "go:"
	if len(provider) <= len(prefix) || provider[:len(prefix)] != prefix {
		return nil, fmt.Errorf("dependency %q: only go: providers run on the in-process target, "+
			"and this one names %q", name, provider)
	}
	factory, ok := fakes[provider[len(prefix):]]
	if !ok {
		return nil, fmt.Errorf("dependency %q names unregistered fake %q", name, provider)
	}
	behavior := dep.Behavior
	// A broken dependency that says nothing else about itself is one that is simply down, which is
	// the failure an operator meets first.
	if dep.Mode == ModeBroken && behavior.FailAfter == 0 && !behavior.Unavailable {
		behavior.Unavailable = true
	}
	return factory(behavior), nil
}

// gate decides what a stand-in does on each call, from the behavior a scenario declared. Every fake
// embeds one, so failure injection means the same thing everywhere rather than being reinvented per
// dependency with its own off-by-one.
type gate struct {
	// behavior is what the scenario asked for.
	behavior Behavior
	// mu guards calls.
	mu sync.Mutex
	// calls is how many times the fake has been reached.
	calls int
}

// admit records a call and reports whether it succeeds, applying any declared latency first.
func (g *gate) admit() bool {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if g.behavior.Latency > 0 {
		time.Sleep(g.behavior.Latency)
	}
	if g.behavior.Unavailable {
		return false
	}
	if g.behavior.FailAfter > 0 && n > g.behavior.FailAfter {
		return false
	}
	return true
}

// count reports how many calls the fake has taken.
func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// status is the HTTP status a failing call answers with, defaulting to service unavailable, which
// is what an outside system that is down actually returns.
func (g *gate) status() int {
	if g.behavior.Status != 0 {
		return g.behavior.Status
	}
	return http.StatusServiceUnavailable
}

// message is the error text a failing call returns.
func (g *gate) message() string {
	if g.behavior.Message != "" {
		return g.behavior.Message
	}
	return "the dependency is unavailable"
}

// fakeSubmitter stands in for the dispatcher, so a scenario can drive the path where a run is
// accepted by the API and refused by what actually executes it.
//
// That path is the one worth declaring. A submit that fails at the boundary is an ordinary 4xx; a
// submit the API accepts and the dispatcher then refuses is where a run can be recorded that never
// ran, or run without being recorded.
type fakeSubmitter struct {
	gate
	// mu guards submitted.
	mu sync.Mutex
	// submitted is every run handed back, so a scenario can assert what the API believed it made.
	submitted []*run.Run
}

// Start is a no-op: the submitter is wired in directly rather than reached over the network.
func (f *fakeSubmitter) Start() (string, error) { return "", nil }

// Stop is a no-op.
func (f *fakeSubmitter) Stop() {}

// Calls reports how many submissions were attempted.
func (f *fakeSubmitter) Calls() int { return f.count() }

// Submit records one run, or fails when the gate says the dispatcher is refusing.
func (f *fakeSubmitter) Submit(_ context.Context, playbook, inventory string,
	_ ...run.SubmitOption) (*run.Run, error) {
	return f.record(playbook, inventory)
}

// SubmitSplit records a sharded run under the same rules as Submit.
func (f *fakeSubmitter) SubmitSplit(_ context.Context, playbook, inventory string, _ int,
	_ ...run.SubmitOption) (*run.Run, error) {
	return f.record(playbook, inventory)
}

// SubmitPipeline records a pipeline under the same rules as Submit.
func (f *fakeSubmitter) SubmitPipeline(_ context.Context, name, inventory string,
	_ []run.PipelineStep, _ ...run.SubmitOption) (*run.Run, error) {
	return f.record(name, inventory)
}

// record is the shared body: one gate decision, one recorded run.
func (f *fakeSubmitter) record(playbook, inventory string) (*run.Run, error) {
	if !f.admit() {
		return nil, fmt.Errorf("%s", f.message())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &run.Run{
		ID:        fmt.Sprintf("run_submitted_%d", len(f.submitted)+1),
		Playbook:  playbook,
		Inventory: inventory,
		Status:    run.StatusPending,
		CreatedAt: time.Now().UTC(),
	}
	f.submitted = append(f.submitted, r)
	return r, nil
}

// fakeHTTP stands in for an outside system reached over HTTP at an address the install is
// configured with, which is how every secret source the product addresses by URL is reached.
type fakeHTTP struct {
	gate
	// routes answers a successful call.
	routes func(http.ResponseWriter, *http.Request)
	// srv is the running server, nil until Start.
	srv *httptest.Server
}

// newFakeHTTP builds an HTTP stand-in answering with the given routes.
func newFakeHTTP(b Behavior, routes func(http.ResponseWriter, *http.Request)) *fakeHTTP {
	return &fakeHTTP{gate: gate{behavior: b}, routes: routes}
}

// Start brings the stand-in up and returns its base URL.
func (f *fakeHTTP) Start() (string, error) {
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.admit() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status())
			fmt.Fprintf(w, `{"errors":[%q]}`, f.message())
			return
		}
		f.routes(w, r)
	}))
	return f.srv.URL, nil
}

// Stop shuts the stand-in down.
func (f *fakeHTTP) Stop() {
	if f.srv != nil {
		f.srv.Close()
	}
}

// Calls reports how many requests reached it.
func (f *fakeHTTP) Calls() int { return f.count() }

// vaultRoutes answers a KV v2 read the way Vault does, which is the shape the product's Vault
// source parses.
func vaultRoutes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"data":{"data":{"value":"fake-vault-secret"}}}`)
}

// echoRoutes answers a generic HTTP secret source with a fixed body.
func echoRoutes(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"value":"fake-http-secret"}`)
}
