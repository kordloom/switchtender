package policy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// The project ids the policy page's per-project example names.
const (
	// docStagingProject is the staging project's id on the page.
	docStagingProject = "proj_5d0e1a2b3c4f"
	// docProductionProject is the production project's id on the page.
	docProductionProject = "proj_9a8b7c6d5e4f"
)

// warnExample is the per-project example as the policy page shows it.
type warnExample struct {
	// File is the policy file.
	File string
	// Modules are the modules it loads, by package name.
	Modules map[string]string
	// Blocking is the production module the page shows refusing instead of holding.
	Blocking string
	// Silent is the staging module the page shows leaving one finding out.
	Silent string
}

// readWarnExample collects the per-project example's blocks from the policy page.
func readWarnExample(t *testing.T) warnExample {
	t.Helper()
	ex := warnExample{Modules: map[string]string{}}
	for _, block := range docBlocks(t, "policy.md") {
		switch {
		case strings.HasPrefix(block, "rego:") && strings.Contains(block, "staging-advice"):
			ex.File = block
		case strings.HasPrefix(block, "package checks"):
			ex.Modules["checks"] = block
		case strings.HasPrefix(block, "package staging") && strings.Contains(block, "msg !="):
			ex.Silent = block
		case strings.HasPrefix(block, "package staging"):
			ex.Modules["staging"] = block
		case strings.HasPrefix(block, "package production") &&
			strings.Contains(block, "deny contains"):
			ex.Blocking = block
		case strings.HasPrefix(block, "package production"):
			ex.Modules["production"] = block
		}
	}
	if ex.File == "" || len(ex.Modules) != 3 || ex.Blocking == "" || ex.Silent == "" {
		t.Fatalf("the page is missing part of the per-project example: %+v", ex)
	}
	return ex
}

// loadWarnExample writes the example's policy file and modules, with any module replaced, and
// returns the policies it loads.
func loadWarnExample(t *testing.T, ex warnExample, replace map[string]string) []*Policy {
	t.Helper()
	files := map[string]string{"policies.yml": ex.File}
	for pkg, src := range ex.Modules {
		if alt, ok := replace[pkg]; ok {
			src = alt
		}
		files[filepath.Join("rego", pkg+".rego")] = src
	}
	store, err := NewFileStore(writeRegoFiles(t, t.TempDir(), files))
	if err != nil {
		t.Fatalf("the page's example does not load: %v", err)
	}
	policies, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	return policies
}

// exampleOutcome is what the gate decides about one run under the example, labels included.
type exampleOutcome struct {
	// Denied names what refused the run, empty when nothing did.
	Denied string
	// Held names what held the run, empty when nothing did.
	Held string
	// Notes are the notes the run carries.
	Notes []string
}

// outcomeOf asks the example's policies about r, with each bundle digest cut from the labels so the
// expectations read as the page does.
func outcomeOf(policies []*Policy, r *run.Run) exampleOutcome {
	strip := func(label string) string {
		if i := strings.Index(label, ", rego sha256:"); i >= 0 {
			return label[:i] + ")"
		}
		return label
	}
	var out exampleOutcome
	if p := Denying(policies, r); p != nil {
		out.Denied = strip(p.Label())
		return out
	}
	if p := Requiring(policies, r); p != nil {
		out.Held = strip(p.Label())
	}
	for _, note := range Noting(policies, r) {
		out.Notes = append(out.Notes, strip(note))
	}
	return out
}

// TestThePerProjectWarningExampleDecidesAsDocumented loads the staging and production example the
// policy page shows, exactly as written, and holds every sentence the page says about it to what the
// gate decides: staging notes, production holds, other projects are decided by neither, a finding
// moved to deny refuses, and a finding left out is silent.
func TestThePerProjectWarningExampleDecidesAsDocumented(t *testing.T) {
	t.Parallel()
	ex := readWarnExample(t)
	judged := func(project string, agent, ticket bool) *run.Run {
		r := &run.Run{ID: "run_doc", Tool: "bash", Command: "deploy", ProjectID: project,
			ActorType: "session"}
		if agent {
			r.ActorType = ActorKindAgent
		}
		if ticket {
			r.Labels = map[string]string{"ticket": "CHG-42"}
		}
		return r
	}
	const (
		noTicket = "no change ticket on the run"
		agent    = "an agent asked for this run"
	)
	tests := []struct {
		// Replace swaps modules for the variants the page shows, by package.
		Replace map[string]string
		// Run is the run judged.
		Run *run.Run
		// WantOutcome is what the page says happens.
		WantOutcome exampleOutcome
	}{{ // Test 0: Staging without a ticket goes ahead carrying the note.
		Run:         judged(docStagingProject, false, false),
		WantOutcome: exampleOutcome{Notes: []string{"staging-advice (" + noTicket + ")"}},
	}, { // Test 1: The same run in production is held by the production entry.
		Run:         judged(docProductionProject, false, false),
		WantOutcome: exampleOutcome{Held: "production-advice (" + noTicket + ")"},
	}, { // Test 2: Any other project is decided by neither entry.
		Run: judged("proj_000000000000", false, false),
	}, { // Test 3: A clean staging run carries nothing.
		Run: judged(docStagingProject, false, true),
	}, { // Test 4: Each finding is its own message, and an agent's run is held by default.
		Run: judged(docStagingProject, true, false),
		WantOutcome: exampleOutcome{Held: AgentDefaultName, Notes: []string{
			"staging-advice (" + agent + ", " + noTicket + ")"}},
	}, { // Test 5: Moved to deny, the finding refuses the production run outright.
		Replace:     map[string]string{"production": ex.Blocking},
		Run:         judged(docProductionProject, false, false),
		WantOutcome: exampleOutcome{Denied: "production-advice (" + noTicket + ")"},
	}, { // Test 6: Left out, the agent finding is silent for staging.
		Replace:     map[string]string{"staging": ex.Silent},
		Run:         judged(docStagingProject, true, true),
		WantOutcome: exampleOutcome{Held: AgentDefaultName},
	}, { // Test 7: The other finding is still noted.
		Replace: map[string]string{"staging": ex.Silent},
		Run:     judged(docStagingProject, true, false),
		WantOutcome: exampleOutcome{Held: AgentDefaultName,
			Notes: []string{"staging-advice (" + noTicket + ")"}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			policies := loadWarnExample(t, ex, test.Replace)
			got := outcomeOf(policies, test.Run)
			if diff := cmp.Diff(test.WantOutcome, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the page's example decides differently (-page +gate):\n%s", diff)
			}
		})
	}
}

// TestTheTimeoutExampleIsTrue loads the timeout entry the policy page shows and holds the refusal
// the page quotes to the one evaluation produces, so the page cannot show a message the server does
// not give.
func TestTheTimeoutExampleIsTrue(t *testing.T) {
	t.Parallel()
	var entry, refusal string
	for _, block := range docBlocks(t, "policy.md") {
		switch {
		case strings.HasPrefix(block, "rego:") && strings.Contains(block, "timeout: 2s"):
			entry = block
		case strings.HasPrefix(block, "inventory-checks (rego policy:"):
			refusal = strings.TrimSpace(block)
		}
	}
	if entry == "" || refusal == "" {
		t.Fatal("the page has no timeout example or no refusal to check")
	}
	store, err := NewFileStore(writeRegoFiles(t, t.TempDir(), map[string]string{
		"policies.yml":        entry,
		"rego/inventory.rego": "package switchtender\n\nhold contains \"x\" if false\n",
	}))
	if err != nil {
		t.Fatalf("the page's timeout entry does not load: %v", err)
	}
	policies, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got := policies[0].Rego.Timeout(); got != 2*time.Second {
		t.Errorf("the page's entry loads with a %s timeout, want 2s", got)
	}
	want := policies[0].regoLabel(policies[0].Rego.timedOut().Error())
	cut := strings.Index(want, ", rego sha256:")
	if cut < 0 || !strings.HasPrefix(refusal, want[:cut]+", rego sha256:") {
		t.Errorf("the page quotes the refusal as\n  %s\nbut evaluation gives\n  %s", refusal, want)
	}
}
