package dispatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheGateSeesEveryWayAPlaybookArrives covers the shapes a run comes in, not just the plain one.
//
// A gate that holds a destructive playbook submitted directly and misses the same playbook wrapped
// in a pipeline step is not a gate, it is a speed bump with a documented way around it. The other
// three ways a run reaches an executor are a split's parent, a pipeline's step, and a retry, and
// each builds its run differently: a step carries its own playbook rather than the parent's.
func TestTheGateSeesEveryWayAPlaybookArrives(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	wipe := filepath.Join(dir, "wipe.yml")
	if err := os.WriteFile(wipe, []byte(`
- hosts: all
  tasks:
    - ansible.builtin.file:
        path: /srv/archive
        state: absent
`), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	gate := &policy.Policy{Name: "hold the permanent", Effect: policy.EffectRequireApproval,
		Reversibility: run.Irreversible, MaxDestroy: policy.DisabledMaxDestroy}
	rules := []*policy.Policy{gate}

	tests := []struct {
		Name string
		Run  *run.Run
	}{{
		Name: "submitted directly",
		Run:  &run.Run{Tool: run.ToolAnsible, Playbook: wipe},
	}, {
		// A split's parent carries the playbook every shard runs, so grading it covers them.
		Name: "the parent of a split",
		Run:  &run.Run{Tool: run.ToolAnsible, Playbook: wipe, Kind: run.KindSplit},
	}, {
		// A shard runs the parent's playbook against a slice of the hosts. Narrowing the host list
		// does not make deleting an archive recoverable.
		Name: "one shard of a split",
		Run:  &run.Run{Tool: run.ToolAnsible, Playbook: wipe, Limit: "web01", ParentID: strPtr("run_1")},
	}, {
		// A retry inherits the parent's whole execution spec, which is exactly why it has to face
		// the same rules: otherwise retrying is a way to run a spec an approver would have held.
		Name: "a retry of a failed split",
		Run:  &run.Run{Tool: run.ToolAnsible, Playbook: wipe, Kind: run.KindSplit, RetryOf: strPtr("run_1")},
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if p := policy.Requiring(rules, graded(test.Run)); p == nil {
				t.Errorf("a playbook that deletes an archive was not held when it arrived as %q. "+
					"The gate can be walked around by choosing how to submit", test.Name)
			}
		})
	}

	// And the pipeline step, which is the one that carries its own playbook rather than inheriting
	// one. A step is built by stepRun, so it is graded through exactly what the dispatcher builds.
	parent := &run.Run{Tool: run.ToolAnsible, Inventory: "hosts.ini", Kind: run.KindPipeline}
	step := run.PipelineStep{Name: "wipe", Playbook: wipe}
	if p := policy.Requiring(rules, graded(stepRun(parent, step, 0, 0, nil))); p == nil {
		t.Error("a destructive playbook wrapped in a pipeline step was not held. Wrapping a " +
			"refused command in a one-step pipeline is the documented way around a gate, and this " +
			"is the same door")
	}
}

// strPtr returns a pointer to s, for the run fields that take one.
func strPtr(s string) *string { return &s }
