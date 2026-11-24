package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// stepVarsRunner publishes fixed outputs for the playbook "build.yml" and records the extra vars
// every step ran with, keyed by its playbook file name or, for a step with none, its tool.
type stepVarsRunner struct {
	// publish is what build.yml prints with set_stats.
	publish map[string]any
	// onBuild, when set, runs after build.yml, before the next step starts.
	onBuild func()
	// mu guards received.
	mu sync.Mutex
	// received maps a step's playbook file name or tool to the extra vars it ran with.
	received map[string]map[string]any
}

// Run records the step's vars and writes build.yml's stats event.
func (r *stepVarsRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	// A step that is not Ansible carries the project checkout in Playbook, so it is keyed by tool.
	key := filepath.Base(spec.Playbook)
	if run.NormalizeTool(spec.Tool) != run.ToolAnsible {
		key = spec.Tool
	}
	r.mu.Lock()
	if r.received == nil {
		r.received = map[string]map[string]any{}
	}
	r.received[key] = spec.ExtraVars
	r.mu.Unlock()
	if key == "build.yml" && r.onBuild != nil {
		defer r.onBuild()
	}
	if key == "build.yml" && spec.EventsPath != "" {
		data, err := json.Marshal(map[string]any{"type": "stats", "ts": 1719000000,
			"outputs": r.publish})
		if err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
		if err := os.WriteFile(spec.EventsPath, append(data, '\n'), 0o600); err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// ran returns the vars a step ran with and whether it ran at all.
func (r *stepVarsRunner) ran(key string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vars, ok := r.received[key]
	return vars, ok
}

// stepOutputShape is how the two steps are joined: one after the other, as a graph, or as a graph
// with an approval between them, so the second step starts on a resumed walk that read the first
// step's outputs back from its stored row.
type stepOutputShape string

// The three entries a step's outputs reach the next step through.
const (
	outputsLinear stepOutputShape = "linear"
	outputsDAG    stepOutputShape = "dag"
	outputsResume stepOutputShape = "resume"
)

// stepOutputSteps returns build then next, joined the way shape says.
func stepOutputSteps(shape stepOutputShape, next run.PipelineStep) []run.PipelineStep {
	build := run.PipelineStep{Name: "build", Playbook: "build.yml"}
	switch shape {
	case outputsDAG:
		next.DependsOn = []string{"build"}
		return []run.PipelineStep{build, next}
	case outputsResume:
		next.DependsOn = []string{"gate"}
		return []run.PipelineStep{build, {Name: "gate", Type: run.StepApproval,
			Description: "Ship it?", DependsOn: []string{"build"}}, next}
	default:
		return []run.PipelineStep{build, next}
	}
}

// TestStepOutputStaysData drives a workflow whose first step publishes values with set_stats into
// a second step, on each of the three entries.
//
// A published value reached an Ansible step as an extra var, which Ansible renders on the
// controller, so a value holding a lookup ran it there, and a key named ansible_ chose how the next
// step connected. The workflow was approved with its own vars and never with these. A bash step
// reads its vars as plain strings and still gets both. A published key that names one of the
// workflow's own vars replaced the approved value for every later step, whatever its tool, so it
// stops the workflow, even when the value is the same.
func TestStepOutputStaysData(t *testing.T) {
	t.Parallel()
	lookup := "{{ lookup('pipe','id') }}"
	tests := []struct {
		Parent    map[string]any
		Publish   map[string]any
		Next      run.PipelineStep
		WantStop  string
		WantVars  map[string]any
		WantNoKey string
	}{{ // Test 0: Template code in a value stops the workflow before the Ansible step.
		Publish:  map[string]any{"version": lookup, "plain": "1.2.3"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `step "deploy" cannot take "version" from step "build"`,
	}, { // Test 1: A plain value reaches the Ansible step.
		Publish:  map[string]any{"plain": "1.2.3"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantVars: map[string]any{"plain": "1.2.3"},
	}, { // Test 2: A bash step gets both values as plain strings.
		Publish:  map[string]any{"version": lookup, "plain": "1.2.3"},
		Next:     run.PipelineStep{Name: "notify", Tool: run.ToolBash, Command: "notify"},
		WantVars: map[string]any{"version": lookup, "plain": "1.2.3"},
	}, { // Test 3: A published ansible_ name stops the workflow.
		Publish:  map[string]any{"ansible_python_interpreter": "/tmp/x"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `cannot take "ansible_python_interpreter" from step "build"`,
	}, { // Test 4: Template code nested in a list stops it too.
		Publish:  map[string]any{"hosts": []any{"web1", map[string]any{"name": lookup}}},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `cannot take "hosts" from step "build"`,
	}, { // Test 5: A published key that names an approved var stops the Ansible step.
		Parent:   map[string]any{"version": "1.0"},
		Publish:  map[string]any{"version": "2.0"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `step "deploy" cannot take "version" from step "build": the workflow was approved`,
	}, { // Test 6: The same collision stops a bash step, which the template checks do not cover.
		Parent:   map[string]any{"version": "1.0"},
		Publish:  map[string]any{"version": "2.0"},
		Next:     run.PipelineStep{Name: "notify", Tool: run.ToolBash, Command: "notify"},
		WantStop: `step "notify" cannot take "version" from step "build"`,
	}, { // Test 7: A published name the workflow was not approved with reaches the step beside it.
		Parent:   map[string]any{"region": "us"},
		Publish:  map[string]any{"version": "2.0"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantVars: map[string]any{"region": "us", "version": "2.0"},
	}, { // Test 8: A published value equal to the approved one stops the workflow too.
		Parent:   map[string]any{"version": "1.0"},
		Publish:  map[string]any{"version": "1.0"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `cannot take "version" from step "build": the workflow was approved`,
	}}
	for _, shape := range []stepOutputShape{outputsLinear, outputsDAG, outputsResume} {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", shape, testNum), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				store := run.NewMemStore()
				audits := audit.NewMemStore()
				runner := &stepVarsRunner{publish: test.Publish}
				// An approval binds what the steps run, so the playbooks come from a project.
				p := dispatchtest.LocalProject(t, map[string]string{
					"build.yml": "- hosts: all\n  tasks: []\n", "deploy.yml": "- hosts: all\n  tasks: []\n"})
				d := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
					WithProjects(p.Store, p.Syncer))
				t.Cleanup(d.Close)
				parent, err := d.SubmitPipeline(ctx, "release", "inv",
					stepOutputSteps(shape, test.Next), run.WithProject(p.ID),
					run.WithExtraVars(test.Parent))
				if err != nil {
					t.Fatalf("SubmitPipeline() error = %v", err)
				}
				if shape == outputsResume {
					node := waitParked(t, store, parent.ID)
					if _, err := d.DecideStep(ctx, node.ID, StepDecision{Approve: true,
						By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
						t.Fatalf("DecideStep() error = %v", err)
					}
				}
				got := dispatchtest.WaitTerminal(t, store, parent.ID)
				nextKey := test.Next.Playbook
				if nextKey == "" {
					nextKey = test.Next.Tool
				}
				vars, ran := runner.ran(nextKey)
				if test.WantStop != "" {
					if got.Status != run.StatusFailed {
						t.Errorf("workflow status = %s, want failed", got.Status)
					}
					if !strings.Contains(got.Error, test.WantStop) {
						t.Errorf("workflow error = %q, want it to say %q", got.Error, test.WantStop)
					}
					if ran {
						t.Errorf("the next step ran with %v, want it never started", vars)
					}
					return
				}
				if got.Status != run.StatusSucceeded {
					t.Fatalf("workflow status = %s (%s), want succeeded", got.Status, got.Error)
				}
				if !ran {
					t.Fatal("the next step never ran")
				}
				for k, want := range test.WantVars {
					if vars[k] != want {
						t.Errorf("next step got %s = %v, want %v (all: %v)", k, vars[k], want, vars)
					}
				}
			})
		}
	}
}

// TestStepOutputRefusalKeepsApprovedVars confirms the check judges only what a step published. A
// workflow approved with an extra var that holds template code still runs, since a person approved
// it, and an approval step takes nothing.
func TestStepOutputRefusalKeepsApprovedVars(t *testing.T) {
	t.Parallel()
	parent := &run.Run{ExtraVars: map[string]any{"msg": "{{ greeting }}"}}
	vars := map[string]any{"msg": "{{ greeting }}", "plain": "1"}
	from := map[string]string{"plain": "build"}
	deploy := run.PipelineStep{Name: "deploy"}
	if got := outputRefusal(deploy, parent, credentialVars{}, vars, from); got != "" {
		t.Errorf("an approved var with template code was refused: %s", got)
	}
	from["msg"] = "build"
	if got := outputRefusal(run.PipelineStep{Name: "gate", Type: run.StepApproval}, parent,
		credentialVars{}, vars, from); got != "" {
		t.Errorf("an approval step was refused: %s", got)
	}
	if got := outputRefusal(deploy, parent, credentialVars{}, vars, from); got == "" {
		t.Error("a published value replaced an approved var at an Ansible step")
	}
}

// TestStepOutputRefusalApprovedNames confirms which names count as approved. A secret survey
// answer is not in the parent's extra vars, since it is laid over a step's vars at execution, so
// its name has to be read from the sealed names, or a published value of that name would pass the
// check and then lose to the answer with no word.
func TestStepOutputRefusalApprovedNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Parent *run.Run
		Step   run.PipelineStep
		From   map[string]string
		Want   string
	}{{ // Test 0: A secret survey answer's name is approved, for a Terraform step too.
		Parent: &run.Run{SealedNames: []string{"db_password"}},
		Step:   run.PipelineStep{Name: "apply", Tool: run.ToolTerraform},
		From:   map[string]string{"db_password": "build"},
		Want:   `step "apply" cannot take "db_password" from step "build": the workflow was approved`,
	}, { // Test 1: A name only an earlier step published is not approved.
		Parent: &run.Run{ExtraVars: map[string]any{"region": "us"}},
		Step:   run.PipelineStep{Name: "apply", Tool: run.ToolTerraform},
		From:   map[string]string{"version": "build"},
	}, { // Test 2: The match is exact, as Ansible's own var names are.
		Parent: &run.Run{ExtraVars: map[string]any{"Version": "1.0"}},
		Step:   run.PipelineStep{Name: "notify", Tool: run.ToolBash},
		From:   map[string]string{"version": "build"},
	}, { // Test 3: The collision is named before an ansible_ key, so the reason says what replaced what.
		Parent: &run.Run{ExtraVars: map[string]any{"target": "web"}},
		Step:   run.PipelineStep{Name: "deploy"},
		From:   map[string]string{"ansible_user": "build", "target": "build"},
		Want:   `step "deploy" cannot take "target" from step "build"`,
	}, { // Test 4: No parent, no approved names.
		Step: run.PipelineStep{Name: "notify", Tool: run.ToolBash},
		From: map[string]string{"version": "build"},
	}}
	for testNum, test := range tests {
		got := outputRefusal(test.Step, test.Parent, credentialVars{}, map[string]any{}, test.From)
		if test.Want == "" {
			if got != "" {
				t.Errorf("test %d: refused with %q, want no refusal", testNum, got)
			}
			continue
		}
		if !strings.Contains(got, test.Want) {
			t.Errorf("test %d: reason = %q, want it to say %q", testNum, got, test.Want)
		}
	}
}

// credOutputType names the custom credential type a credential output test attaches.
type credOutputType string

// The two custom types: one that injects api_host as an Ansible extra var, and one that sets the
// Terraform variable API_HOST as TF_VAR_API_HOST. A type names environment variables in capitals.
const (
	credOutputExtraVar credOutputType = "ctype_extra"
	credOutputTFVar    credOutputType = "ctype_tfvar"
)

// credOutputStores holds the credential, type, and inventory stores a credential output test wires
// into its dispatcher.
type credOutputStores struct {
	// creds holds the one credential, "prod-datadog".
	creds credential.Store
	// types holds both custom types.
	types credential.TypeStore
	// invs holds the inventory "inv_dd", which attaches the credential.
	invs inventory.Store
	// sealer seals the credential's fields.
	sealer *credential.Sealer
}

// newCredOutputStores saves both custom types, the credential "prod-datadog" of type typ, and an
// inventory that attaches it.
func newCredOutputStores(t *testing.T, typ credOutputType) credOutputStores {
	t.Helper()
	ctx := context.Background()
	st := credOutputStores{creds: credential.NewMemStore(), types: credential.NewMemTypeStore(),
		invs: inventory.NewMemStore(), sealer: credential.NewSealer("pass", "salt")}
	fields := []credential.Field{{Name: "host"}, {Name: "token", Secret: true}}
	for _, ct := range []*credential.CredentialType{{
		ID: string(credOutputExtraVar), Name: "Datadog", Fields: fields,
		ExtraVarInjectors: map[string]string{"api_host": "{{host}}", "api_token": "{{token}}"},
	}, {
		ID: string(credOutputTFVar), Name: "Datadog for Terraform", Fields: fields,
		EnvInjectors: map[string]string{
			"TF_VAR_API_HOST": "{{host}}", "TF_VAR_API_TOKEN": "{{token}}",
		},
	}} {
		if err := ct.Validate(); err != nil {
			t.Fatalf("Validate(%s) error = %v", ct.ID, err)
		}
		if err := st.types.Save(ctx, ct); err != nil {
			t.Fatalf("Save(%s) error = %v", ct.ID, err)
		}
	}
	sealed, err := st.sealer.Seal(`{"host":"api.datadoghq.com","token":"dd-s3cret"}`)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if err := st.creds.Save(ctx, &credential.Credential{ID: "cred_dd", Name: "prod-datadog",
		TypeID: string(typ), Secret: sealed}); err != nil {
		t.Fatalf("Save(credential) error = %v", err)
	}
	if err := st.invs.Save(ctx, &inventory.Inventory{ID: "inv_dd", Name: "fleet",
		Content: "[all]\nlocalhost\n", CredentialIDs: []string{"cred_dd"}}); err != nil {
		t.Fatalf("Save(inventory) error = %v", err)
	}
	return st
}

// TestStepOutputCredentialVars drives a workflow whose first step publishes values into a second
// step while a custom credential injects api_host, on each of the three entries.
//
// Ansible reads the run's extra vars after a credential's vars file, and Terraform reads a run
// var's TF_VAR_ entry after the credential environment, so a published api_host replaced the
// credential's own and could send its token to a server the publishing host chose. A person's own
// var still outranks the credential's, as in AWX, and a tool the credential does not inject into
// takes the value.
//
//nolint:funlen // Test function.
func TestStepOutputCredentialVars(t *testing.T) {
	t.Parallel()
	deploy := run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"}
	notify := run.PipelineStep{Name: "notify", Tool: run.ToolBash, Command: "notify"}
	apply := run.PipelineStep{Name: "apply", Tool: run.ToolTerraform, Command: "infra"}
	tests := []struct {
		// Type is the custom type of the credential the workflow runs with.
		Type credOutputType
		// ByInventory attaches the credential through the workflow's inventory, not the workflow.
		ByInventory bool
		// DropType deletes the credential's type once the first step has run.
		DropType bool
		// Parent is the workflow's own extra vars.
		Parent map[string]any
		// Publish is what the first step publishes.
		Publish map[string]any
		// Next is the step that takes the published values.
		Next run.PipelineStep
		// WantStop is part of the workflow's error, empty when the next step runs.
		WantStop string
		// WantVars are vars the next step runs with.
		WantVars map[string]any
	}{{ // Test 0: A published name a credential injects stops the Ansible step and names it.
		Type: credOutputExtraVar, Publish: map[string]any{"api_host": "evil.example"}, Next: deploy,
		WantStop: `step "deploy" cannot take "api_host" from step "build": credential ` +
			`"prod-datadog" injects a var of that name`,
	}, { // Test 1: The same, with the credential attached through the inventory.
		Type: credOutputExtraVar, ByInventory: true,
		Publish: map[string]any{"api_host": "evil.example"}, Next: deploy,
		WantStop: `credential "prod-datadog" injects a var of that name`,
	}, { // Test 2: A name no credential injects reaches the Ansible step.
		Type: credOutputExtraVar, Publish: map[string]any{"other": "1"}, Next: deploy,
		WantVars: map[string]any{"other": "1"},
	}, { // Test 3: The person's own var of that name runs, as in AWX.
		Type: credOutputExtraVar, Parent: map[string]any{"api_host": "eu.datadoghq.com"},
		Publish: map[string]any{"build_id": "7"}, Next: deploy,
		WantVars: map[string]any{"api_host": "eu.datadoghq.com", "build_id": "7"},
	}, { // Test 4: A bash step reads no credential vars file and takes the value.
		Type: credOutputExtraVar, Publish: map[string]any{"api_host": "evil.example"}, Next: notify,
		WantVars: map[string]any{"api_host": "evil.example"},
	}, { // Test 5: A type that cannot be read refuses the step, whatever it publishes.
		Type: credOutputExtraVar, DropType: true, Publish: map[string]any{"other": "1"},
		Next: deploy, WantStop: `step "deploy" cannot take "other" from step "build": the ` +
			`workflow's credentials could not be read`,
	}, { // Test 6: A credential setting TF_VAR_API_HOST stops a Terraform step taking API_HOST.
		Type: credOutputTFVar, Publish: map[string]any{"API_HOST": "evil.example"}, Next: apply,
		WantStop: `step "apply" cannot take "API_HOST" from step "build": credential ` +
			`"prod-datadog" injects TF_VAR_API_HOST`,
	}, { // Test 7: A Terraform step takes API_HOSTNAME, which no credential sets.
		Type: credOutputTFVar, Publish: map[string]any{"API_HOSTNAME": "evil.example"}, Next: apply,
		WantVars: map[string]any{"API_HOSTNAME": "evil.example"},
	}, { // Test 8: The match is exact, as Terraform's is, so api_host is another variable.
		Type: credOutputTFVar, Publish: map[string]any{"api_host": "x"}, Next: apply,
		WantVars: map[string]any{"api_host": "x"},
	}, { // Test 9: A credential's TF_VAR_ name does not stop an Ansible step.
		Type: credOutputTFVar, Publish: map[string]any{"API_HOST": "x"}, Next: deploy,
		WantVars: map[string]any{"API_HOST": "x"},
	}, { // Test 10: A credential's extra var does not stop a Terraform step.
		Type: credOutputExtraVar, Publish: map[string]any{"api_host": "x"}, Next: apply,
		WantVars: map[string]any{"api_host": "x"},
	}}
	for _, shape := range []stepOutputShape{outputsLinear, outputsDAG, outputsResume} {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", shape, testNum), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				store := run.NewMemStore()
				st := newCredOutputStores(t, test.Type)
				runner := &stepVarsRunner{publish: test.Publish}
				if test.DropType {
					runner.onBuild = func() {
						if err := st.types.Delete(ctx, string(test.Type)); err != nil {
							t.Errorf("Delete(type) error = %v", err)
						}
					}
				}
				p := dispatchtest.LocalProject(t, map[string]string{
					"build.yml":     "- hosts: all\n  tasks: []\n",
					"deploy.yml":    "- hosts: all\n  tasks: []\n",
					"infra/main.tf": "terraform {}\n"})
				d := New(store, runner, nil, WithNoJanitor(), WithAudits(audit.NewMemStore()),
					WithProjects(p.Store, p.Syncer), WithCredentials(st.creds, st.sealer),
					WithCredentialTypes(st.types), WithInventories(st.invs))
				t.Cleanup(d.Close)
				opts := []run.SubmitOption{run.WithProject(p.ID), run.WithExtraVars(test.Parent)}
				if test.ByInventory {
					opts = append(opts, run.WithInventory("inv_dd"))
				} else {
					opts = append(opts, run.WithCredentialIDs([]string{"cred_dd"}))
				}
				parent, err := d.SubmitPipeline(ctx, "release", "inv",
					stepOutputSteps(shape, test.Next), opts...)
				if err != nil {
					t.Fatalf("SubmitPipeline() error = %v", err)
				}
				if shape == outputsResume {
					node := waitParked(t, store, parent.ID)
					if _, err := d.DecideStep(ctx, node.ID, StepDecision{Approve: true,
						By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
						t.Fatalf("DecideStep() error = %v", err)
					}
				}
				got := dispatchtest.WaitTerminal(t, store, parent.ID)
				nextKey := test.Next.Playbook
				if nextKey == "" {
					nextKey = test.Next.Tool
				}
				if _, built := runner.ran("build.yml"); !built {
					t.Fatalf("the first step never ran (workflow %s: %s)", got.Status, got.Error)
				}
				vars, ran := runner.ran(nextKey)
				if test.WantStop != "" {
					if got.Status != run.StatusFailed {
						t.Errorf("workflow status = %s, want failed", got.Status)
					}
					if !strings.Contains(got.Error, test.WantStop) {
						t.Errorf("workflow error = %q, want it to say %q", got.Error, test.WantStop)
					}
					if ran {
						t.Errorf("the next step ran with %v, want it never started", vars)
					}
					return
				}
				if got.Status != run.StatusSucceeded {
					t.Fatalf("workflow status = %s (%s), want succeeded", got.Status, got.Error)
				}
				if !ran {
					t.Fatal("the next step never ran")
				}
				for k, want := range test.WantVars {
					if vars[k] != want {
						t.Errorf("next step got %s = %v, want %v (all: %v)", k, vars[k], want, vars)
					}
				}
			})
		}
	}
}

// TestInjectedVarNames confirms what the check reads from credential records and types: a custom
// type's extra vars and TF_VAR_ names, from the workflow's credentials and its inventory's, nothing
// from a built-in kind, and an error, never an empty set, when a record or type cannot be read.
func TestInjectedVarNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newCredOutputStores(t, credOutputExtraVar)
	if err := st.creds.Save(ctx, &credential.Credential{ID: "cred_tf", Name: "tf-datadog",
		TypeID: string(credOutputTFVar)}); err != nil {
		t.Fatal(err)
	}
	if err := st.creds.Save(ctx, &credential.Credential{ID: "cred_env", Name: "cloud",
		Kind: credential.KindEnv}); err != nil {
		t.Fatal(err)
	}
	if err := st.creds.Save(ctx, &credential.Credential{ID: "cred_gone", Name: "orphan",
		TypeID: "ctype_gone"}); err != nil {
		t.Fatal(err)
	}
	full := &Dispatcher{credentials: st.creds, credentialTypes: st.types, inventories: st.invs}
	tests := []struct {
		D             *Dispatcher
		Parent        *run.Run
		WantAnsible   map[string]string
		WantTerraform map[string]string
		WantErr       string
	}{{ // Test 0: No credentials, no names and no error.
		D: full, Parent: &run.Run{},
	}, { // Test 1: A custom type's extra vars, on the workflow.
		D: full, Parent: &run.Run{CredentialIDs: []string{"cred_dd"}},
		WantAnsible: map[string]string{"api_host": "prod-datadog", "api_token": "prod-datadog"},
	}, { // Test 2: The same credential through the inventory.
		D: full, Parent: &run.Run{InventoryID: "inv_dd"},
		WantAnsible: map[string]string{"api_host": "prod-datadog", "api_token": "prod-datadog"},
	}, { // Test 3: A custom type's TF_VAR_ names, without the prefix. A built-in kind adds none.
		D: full, Parent: &run.Run{CredentialIDs: []string{"cred_tf", "cred_env"}},
		WantTerraform: map[string]string{"API_HOST": "tf-datadog", "API_TOKEN": "tf-datadog"},
	}, { // Test 4: A credential that is not in the store is an error.
		D: full, Parent: &run.Run{CredentialIDs: []string{"cred_missing"}},
		WantErr: "credential cred_missing",
	}, { // Test 5: A type that is not in the store is an error.
		D: full, Parent: &run.Run{CredentialIDs: []string{"cred_dd", "cred_gone"}},
		WantErr: `credential "orphan": read type ctype_gone`,
	}, { // Test 6: A custom type with no type store is an error.
		D:      &Dispatcher{credentials: st.creds},
		Parent: &run.Run{CredentialIDs: []string{"cred_dd"}}, WantErr: "none are configured",
	}, { // Test 7: Credentials with no credential store are an error.
		D: &Dispatcher{}, Parent: &run.Run{CredentialIDs: []string{"cred_dd"}},
		WantErr: "no credential store",
	}}
	for testNum, test := range tests {
		got := test.D.injectedVarNames(ctx, test.Parent)
		if test.WantErr != "" {
			if got.err == nil || !strings.Contains(got.err.Error(), test.WantErr) {
				t.Errorf("test %d: err = %v, want it to say %q", testNum, got.err, test.WantErr)
			}
			continue
		}
		if got.err != nil {
			t.Errorf("test %d: err = %v, want nil", testNum, got.err)
		}
		if diff := cmp.Diff(test.WantAnsible, got.ansible, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: ansible names mismatch (-want +got):\n%s", testNum, diff)
		}
		if diff := cmp.Diff(test.WantTerraform, got.terraform, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: terraform names mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}

// TestCredentialVarsRefusal confirms which steps a credential's names refuse: Ansible by extra var,
// Terraform and OpenTofu by TF_VAR_ name, never a bash step, and an unread set only on a tool a
// credential injects into and only when the step takes a published value.
func TestCredentialVarsRefusal(t *testing.T) {
	t.Parallel()
	creds := credentialVars{ansible: map[string]string{"api_host": "dd"},
		terraform: map[string]string{"api_host": "dd-tf"}}
	unread := credentialVars{err: errors.New("credential cred_x: not found")}
	tests := []struct {
		Creds credentialVars
		Step  run.PipelineStep
		From  map[string]string
		Want  string
	}{{ // Test 0: An OpenTofu step is held to the TF_VAR_ names, as Terraform is.
		Creds: creds, Step: run.PipelineStep{Name: "apply", Tool: run.ToolOpenTofu},
		From: map[string]string{"api_host": "build"},
		Want: `step "apply" cannot take "api_host" from step "build": credential "dd-tf" injects ` +
			`TF_VAR_api_host`,
	}, { // Test 1: A Python step reads neither, so it is not refused.
		Creds: creds, Step: run.PipelineStep{Name: "script", Tool: run.ToolPython},
		From: map[string]string{"api_host": "build"},
	}, { // Test 2: An unread set refuses an Ansible step that takes a published value.
		Creds: unread, Step: run.PipelineStep{Name: "deploy"},
		From: map[string]string{"other": "build"},
		Want: `step "deploy" cannot take "other" from step "build": the workflow's credentials ` +
			`could not be read to check that none of them injects that name (credential cred_x: ` +
			`not found)`,
	}, { // Test 3: An unread set does not refuse a bash step.
		Creds: unread, Step: run.PipelineStep{Name: "notify", Tool: run.ToolBash},
		From: map[string]string{"other": "build"},
	}, { // Test 4: An unread set does not refuse a step that takes nothing published.
		Creds: unread, Step: run.PipelineStep{Name: "deploy"}, From: map[string]string{},
	}, { // Test 5: The approved name is named before the credential's, as the rule reads in order.
		Creds: creds, Step: run.PipelineStep{Name: "deploy"},
		From: map[string]string{"api_host": "build", "version": "build"},
		Want: `cannot take "version" from step "build": the workflow was approved`,
	}}
	parent := &run.Run{ExtraVars: map[string]any{"version": "1.0"}}
	for testNum, test := range tests {
		got := outputRefusal(test.Step, parent, test.Creds, map[string]any{}, test.From)
		if test.Want == "" {
			if got != "" {
				t.Errorf("test %d: refused with %q, want no refusal", testNum, got)
			}
			continue
		}
		if !strings.Contains(got, test.Want) {
			t.Errorf("test %d: reason = %q, want it to say %q", testNum, got, test.Want)
		}
	}
}
