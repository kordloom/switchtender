package roundhouse

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestPlanSensitiveValuesFindsWhatThePlanMarks covers reading the values a saved plan carries in the
// clear and marks sensitive: attributes a change's masks mark, whole values a mask marks true, and
// variables the configuration declares sensitive. Values nothing marks stay out, so masking does not
// swallow ordinary output.
func TestPlanSensitiveValuesFindsWhatThePlanMarks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// In is the plan's JSON rendering.
		In string
		// WantValues are the values found, sorted.
		WantValues []string
	}{{ // Test 0: One attribute of a resource is marked, its neighbor is not.
		Name: "attribute",
		In: `{"resource_changes":[{"change":{"after":{"password":"hunter2-x","name":"web"},` +
			`"after_sensitive":{"password":true}}}]}`,
		WantValues: []string{"hunter2-x"},
	}, { // Test 1: A whole value marked true marks every string beneath it.
		Name: "whole value",
		In: `{"resource_changes":[{"change":{"before":{"keys":["k1-secret","k2-secret"]},` +
			`"before_sensitive":true}}]}`,
		WantValues: []string{"k1-secret", "k2-secret"},
	}, { // Test 2: A sensitive variable's value, and not an ordinary one's.
		Name: "variables",
		In: `{"variables":{"token":{"value":"tok-123456"},"region":{"value":"us-east-1"}},` +
			`"configuration":{"root_module":{"variables":{"token":{"sensitive":true},"region":{}}}}}`,
		WantValues: []string{"tok-123456"},
	}, { // Test 3: A sensitive output.
		Name:       "output",
		In:         `{"output_changes":{"dsn":{"after":"postgres://u:p@h/db","after_sensitive":true}}}`,
		WantValues: []string{"postgres://u:p@h/db"},
	}, { // Test 4: Nothing marked, nothing found.
		Name:       "nothing marked",
		In:         `{"resource_changes":[{"change":{"after":{"name":"web"},"after_sensitive":{}}}]}`,
		WantValues: nil,
	}, { // Test 5: A rendering that does not decode yields nothing rather than failing.
		Name: "not json", In: `not json`, WantValues: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := PlanSensitiveValues([]byte(test.In))
			if diff := cmp.Diff(test.WantValues, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("values mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTerraformArgsApplyTheSavedPlan pins the argument lists the plan file rests on: a plan given
// PlanOut saves its plan file, and an apply given PlanFile applies that file and nothing else, so the
// tool refuses a plan made stale since. An apply given none applies the configuration directly.
func TestTerraformArgsApplyTheSavedPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Spec is the run.
		Spec Spec
		// WantArgs is the argument list.
		WantArgs []string
	}{{ // Test 0: A plan that saves its plan file.
		Spec:     Spec{DryRun: true, PlanOut: "/rf/run/plan/plan.tfplan"},
		WantArgs: []string{"plan", "-input=false", "-no-color", "-detailed-exitcode", "-out=/rf/run/plan/plan.tfplan"},
	}, { // Test 1: An apply that carries out a saved plan.
		Spec:     Spec{PlanFile: "/rf/run/plan/plan.tfplan"},
		WantArgs: []string{"apply", "-input=false", "-no-color", "/rf/run/plan/plan.tfplan"},
	}, { // Test 2: A direct apply, which no plan gate scoped.
		Spec:     Spec{},
		WantArgs: []string{"apply", "-auto-approve", "-input=false", "-no-color"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantArgs, terraformActionArgs(test.Spec)); diff != "" {
				t.Errorf("args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
