package dispatch

import (
	"reflect"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// stepFieldRule says what building a pipeline step does with one field of the run it comes from.
type stepFieldRule int

const (
	// carries means the pipeline governs this and the parent's value must arrive on the step. Dropping
	// one of these does not make a step narrower, it makes it wider: a pipeline submitted to skip a tag
	// whose steps skip nothing runs the plays the operator excluded.
	carries stepFieldRule = iota
	// theStepsOwn means the step names its own, so the parent's must not show through.
	theStepsOwn
	// fresh means the step gets its own, built where it is built: its id, its place in the pipeline,
	// its attempt, the variables it was handed.
	fresh
	// notOnAStep means a newly built step must not carry the parent's at all. Outcomes it has not had
	// yet, leases nobody has taken, approval stamps that cover the pipeline's own spec rather than the
	// step's, and the pipeline-level provenance that would read as a second request if copied down.
	notOnAStep
)

// stepFields is what happens to every field of run.Run when a pipeline step is built.
//
// This exists because the split was a comment over a hand-kept list of assignments. Nothing held the
// list to the struct, so each field added to a run since defaulted to being dropped, quietly, and the
// default was the dangerous direction: Tags and SkipTags went that way, so a pipeline ran with the
// scope an operator had narrowed thrown away, and the policy gate graded each step off the same
// builder and therefore decided about a run that was not the one that would execute.
//
// A field of run.Run that is not named here fails the test below rather than being dropped by default.
var stepFields = map[string]stepFieldRule{
	// What the step itself is.
	"Playbook":  theStepsOwn,
	"Inventory": theStepsOwn,
	"Tool":      theStepsOwn,
	"Command":   theStepsOwn,

	// What the step is within its pipeline.
	"ID":        fresh,
	"Status":    fresh,
	"CreatedAt": fresh,
	"ParentID":  fresh,
	"StepName":  fresh,
	"StepIndex": fresh,
	"Attempt":   fresh,
	"ExtraVars": fresh,
	// What the gate's scan of the step's own dry run read, its share of the pipeline's record.
	"DryRunScans": fresh,
	// What a policy noted about the step, its share of the pipeline's record the same way.
	"PolicyNotes": fresh,

	// How the run executes, which the pipeline decides for all of it.
	"DryRun":           carries,
	"Limit":            carries,
	"Tags":             carries,
	"SkipTags":         carries,
	"Verbosity":        carries,
	"Forks":            carries,
	"DiffMode":         carries,
	"UseFactCache":     carries,
	"FactCacheTimeout": carries,
	"Timeout":          carries,
	"Queue":            carries,
	"Image":            carries,
	"PullCredentialID": carries,
	"CredentialIDs":    carries,
	"ProjectID":        carries,
	"InventoryID":      carries,
	// The hosts a composed inventory resolved to at launch, which every step is held to.
	"InventoryResolution": carries,
	// The plain stored inventory the pipeline was submitted with, which every step executes.
	"InventorySnapshot": carries,
	"InventorySealed":   carries,
	"PinnedCommit":      carries,
	"GitRef":            carries,
	"TemplateID":        carries,
	"SealedNames":       carries,
	"SealedVars":        carries,
	"SealedDigests":     carries,

	// Who asked, which tenant it belongs to, and what the record and the rules say about it.
	"OrgID":        carries,
	"Actor":        carries,
	"ActorUserID":  carries,
	"ActorType":    carries,
	"Labels":       carries,
	"AuditReceipt": carries,
	"PolicySet":    carries,

	// Outcomes and leases a step has not reached yet.
	"ExitCode":        notOnAStep,
	"Error":           notOnAStep,
	"Warning":         notOnAStep,
	"StartedAt":       notOnAStep,
	"EndedAt":         notOnAStep,
	"Outputs":         notOnAStep,
	"ClaimedBy":       notOnAStep,
	"ClaimedAt":       notOnAStep,
	"ClaimSecret":     notOnAStep,
	"CancelRequested": notOnAStep,
	"CommitSHA":       notOnAStep,
	// The cross-check each step's own execution makes before its play.
	"InventoryCheck": notOnAStep,
	// What each step's own execution learns: the image digest it pulled and the hosts a dynamic
	// source resolved to.
	"ImageDigest":   notOnAStep,
	"ResolvedHosts": notOnAStep,

	// Shape and provenance that belong to the pipeline run, not to a step under it.
	"Kind":         notOnAStep,
	"Steps":        notOnAStep,
	"ShardIndex":   notOnAStep,
	"ShardCount":   notOnAStep,
	"RetryOf":      notOnAStep,
	"RerunOf":      notOnAStep,
	"ProposedFrom": notOnAStep,
	"PlanDestroys": notOnAStep,
	// A plan file belongs to the apply a plan gate proposes, never to a step of a pipeline.
	"PlanSHA256":     notOnAStep,
	"PlanSealed":     notOnAStep,
	"Intent":         notOnAStep,
	"Source":         notOnAStep,
	"SourceID":       notOnAStep,
	"IdempotencyKey": notOnAStep,
	"Notifications":  notOnAStep,
	// The agent identity is the pipeline's provenance, recorded once on the run its outcome commits.
	"Initiator": notOnAStep,

	// Decisions, which are made about the pipeline. A child is not approvable on its own, and these
	// stamps cover the spec that was decided on rather than any step's.
	"HeldByPolicy":            notOnAStep,
	"ApprovalRequested":       notOnAStep,
	"HoldNote":                notOnAStep,
	"RequireDistinctApprover": notOnAStep,
	"RequireReason":           notOnAStep,
	"ApprovedSpecDigest":      notOnAStep,
	"ApprovedSpecBinding":     notOnAStep,
	// The decision that won the pipeline, and one still claiming it, are the pipeline's. A step is
	// decided on its own, when it is an approval step, by a decision that claims that step.
	"DecisionID":    notOnAStep,
	"DecisionClaim": notOnAStep,

	// Grades a handler computes when something reads the run.
	"Risk":          notOnAStep,
	"Reversibility": notOnAStep,

	// Notices a notification carries on its own copy of the run, never stored.
	"AwaitingStep": notOnAStep,
	"Attention":    notOnAStep,
}

// TestEveryRunFieldSaysWhatAPipelineStepDoesWithIt fails the build for an unclassified field.
//
// The point is the direction of the default. Adding a field to run.Run used to mean pipeline steps
// silently did not get it, and nothing anywhere said so; now it means this test fails by name until
// somebody decides, which is the same decision they were always supposed to make.
func TestEveryRunFieldSaysWhatAPipelineStepDoesWithIt(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeOf(run.Run{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if _, ok := stepFields[name]; !ok {
			t.Errorf("run.Run has the field %q and nothing says what a pipeline step does with it. "+
				"Add it to stepFields: carries if the pipeline governs it, theStepsOwn if the step names "+
				"its own, fresh if the step gets its own, notOnAStep if a new step must not have it. "+
				"Leaving it out is how Tags and SkipTags came to be dropped.", name)
		}
	}
	for name := range stepFields {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("stepFields names %q, which run.Run no longer has, so this table is describing a "+
				"field that is gone and could be hiding a real one", name)
		}
	}
}

// TestAPipelineStepCarriesEverythingThePipelineGoverns checks the classification against the builder.
//
// The table alone is documentation, and documentation is what was there before: a comment naming two
// categories over a list that had drifted from it. This fills every field of a parent run with a value
// nothing else would produce, builds a step from it, and holds the result to what the table says.
func TestAPipelineStepCarriesEverythingThePipelineGoverns(t *testing.T) {
	t.Parallel()
	parent := &run.Run{}
	fillEveryField(t, reflect.ValueOf(parent).Elem())
	parent.ID = "run_parent"

	step := run.PipelineStep{
		Name: "step-two", Playbook: "step.yml", Inventory: "step-inventory",
		Tool: run.ToolAnsible, Command: "step-command",
	}
	vars := map[string]any{"from": "the step's own vars"}
	child := stepRun(parent, step, 1, 2, vars)

	pv := reflect.ValueOf(parent).Elem()
	cv := reflect.ValueOf(child).Elem()
	rt := pv.Type()
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		got, want := cv.Field(i).Interface(), pv.Field(i).Interface()
		switch stepFields[name] {
		case carries:
			if !reflect.DeepEqual(got, want) {
				t.Errorf("a pipeline step does not carry %s: the pipeline has %v and the step has %v. "+
					"This field decides how the run executes, so a step without it runs under different "+
					"terms than the pipeline was submitted and approved under.", name, want, got)
			}
		case theStepsOwn, fresh:
			if reflect.DeepEqual(got, want) {
				t.Errorf("a pipeline step took the pipeline's %s (%v). The step names or builds its "+
					"own, so the parent's value showing through means the builder is copying past the "+
					"line it is supposed to hold.", name, want)
			}
		case notOnAStep:
			zero := reflect.Zero(rt.Field(i).Type).Interface()
			if !reflect.DeepEqual(got, zero) {
				t.Errorf("a newly built pipeline step carries %s = %v. This belongs to the pipeline "+
					"run: on a step it is either an outcome the step has not had, a lease nobody took, "+
					"or a decision that covers the pipeline's spec and not this one.", name, got)
			}
		}
	}
}

// fillEveryField puts a non-zero value in every field of a struct, recursively, so a copy that misses
// one is visible. The values are deliberately unlike anything the builder produces.
func fillEveryField(t *testing.T, v reflect.Value) {
	t.Helper()
	rt := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if !v.Field(i).CanSet() {
			continue
		}
		fillValue(t, v.Field(i), "parent-"+rt.Field(i).Name)
	}
}

// fillValue puts a non-zero value of the right shape into one field.
func fillValue(t *testing.T, v reflect.Value, sentinel string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(sentinel)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillValue(t, elem, sentinel)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		key := reflect.New(v.Type().Key()).Elem()
		fillValue(t, key, sentinel+"-key")
		val := reflect.New(v.Type().Elem()).Elem()
		fillValue(t, val, sentinel+"-value")
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillValue(t, p.Elem(), sentinel)
		v.Set(p)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)))
			return
		}
		fillEveryField(t, v)
	case reflect.Interface:
		v.Set(reflect.ValueOf(sentinel))
	default:
		t.Fatalf("fillValue has no case for %s, so a field of run.Run would be left zero and this "+
			"test would pass over it", v.Kind())
	}
}
