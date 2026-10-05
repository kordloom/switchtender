package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// nulProbe is a body shape with a field at each depth a NUL can hide in.
type nulProbe struct {
	// Name is a top-level string.
	Name string `json:"name"`
	// Steps holds objects inside an array.
	Steps []nulProbeStep `json:"steps"`
	// Labels is a map whose keys come from the caller.
	Labels map[string]string `json:"labels"`
	// Extra holds values of any shape.
	Extra map[string]any `json:"extra"`
}

// nulProbeStep is one element of nulProbe.Steps.
type nulProbeStep struct {
	// Name is the step's name.
	Name string `json:"name"`
}

// TestStrictDecodeRefusesANulAndNamesWhereItSits pins the strict decoder's refusal of a NUL
// character, written in JSON as the escape \u0000, at every depth a body can carry one, and its
// silence about text that only looks like one.
//
// The decoder used to accept the escape and hand the byte to the handler, which stored it: SQLite
// kept it and PostgreSQL refused the write with a 500. A caller needs to know which field to fix,
// so the refusal names it, and a field name holding the NUL is reported by the object it sits in.
func TestStrictDecodeRefusesANulAndNamesWhereItSits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body        string
		WantMessage string
	}{{ // Test 0: A top-level field.
		Body:        `{"name":"a\u0000b"}`,
		WantMessage: `the field "name" in the request body holds a NUL character`,
	}, { // Test 1: A field of an object inside an array.
		Body:        `{"steps":[{"name":"one"},{"name":"tw\u0000o"}]}`,
		WantMessage: `the field "steps[1].name" in the request body holds a NUL character`,
	}, { // Test 2: A map key the caller chose.
		Body:        `{"labels":{"te\u0000am":"ops"}}`,
		WantMessage: `a field name in "labels" in the request body holds a NUL character`,
	}, { // Test 3: A value nested in arrays inside a free-form map.
		Body:        `{"extra":{"hosts":[["a","b"],["c","d\u0000"]]}}`,
		WantMessage: `the field "extra.hosts[1][1]" in the request body holds a NUL character`,
	}, { // Test 4: A top-level field name.
		Body:        `{"na\u0000me":"x"}`,
		WantMessage: "a field name in the request body holds a NUL character",
	}, { // Test 5: A body that is only a string.
		Body:        `"\u0000"`,
		WantMessage: "the request body holds a NUL character",
	}, { // Test 6: An escaped backslash before u0000 is six characters of text, not a NUL.
		Body: `{"name":"a\\u0000b"}`,
	}, { // Test 7: An unknown field is still named as unknown.
		Body:        `{"title":"x"}`,
		WantMessage: `unknown field "title" in the request body`,
	}, { // Test 8: A body holding no NUL decodes as before.
		Body: `{"name":"ok","steps":[{"name":"one"}],"labels":{"team":"ops"}}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var dst nulProbe
			err := strictDecode(strings.NewReader(test.Body), &dst)
			got := ""
			if err != nil {
				got = decodeErrorMessage(err)
			}
			if diff := cmp.Diff(test.WantMessage, got); diff != "" {
				t.Errorf("refusal mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
