package roundhouse

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// planJSONCap bounds how much of a plan's JSON rendering is held in memory. A rendering past it is
// not returned at all, so the plan is treated as one nobody could measure rather than measured from a
// truncated copy.
const planJSONCap = 64 << 20

// errPlanTooLarge is returned when a plan's JSON rendering is larger than planJSONCap.
var errPlanTooLarge = errors.New("the plan's JSON rendering is larger than the runner holds")

// terraformShowArgs is the argument list that renders a saved plan file as JSON, shared by the host
// runner and the container plan.
func terraformShowArgs(planFile string) []string {
	return []string{"show", "-json", "-no-color", planFile}
}

// cappedCapture accumulates up to planJSONCap bytes and remembers that it was asked to hold more.
type cappedCapture struct {
	// buf holds what fit.
	buf bytes.Buffer
	// over reports that output was discarded.
	over bool
}

// Write stores what fits and records an overflow, always reporting the full length so the process
// writing to it is never told its output was short.
func (c *cappedCapture) Write(p []byte) (int, error) {
	if room := planJSONCap - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
			return len(p), nil
		}
		c.buf.Write(p[:room])
	}
	c.over = true
	return len(p), nil
}

// bytes returns what was captured, or errPlanTooLarge when output was discarded.
func (c *cappedCapture) bytes() ([]byte, error) {
	if c.over {
		return nil, errPlanTooLarge
	}
	return c.buf.Bytes(), nil
}

// PlanSensitiveValues returns every string value a plan's JSON rendering marks sensitive: in the
// before and after of each resource and output change, and the values of the variables the
// configuration declares sensitive. They are the values a saved plan carries in the clear, so the
// run's masker holds them before anything the tool prints could quote one.
func PlanSensitiveValues(planJSON []byte) []string {
	var plan struct {
		// ResourceChanges are the planned resource changes.
		ResourceChanges []struct {
			// Change is the change with its sensitivity masks.
			Change planChange `json:"change"`
		} `json:"resource_changes"`
		// OutputChanges are the planned output changes by name.
		OutputChanges map[string]planChange `json:"output_changes"`
		// Variables are the variable values the plan was made with.
		Variables map[string]struct {
			// Value is the variable's value.
			Value any `json:"value"`
		} `json:"variables"`
		// Configuration declares which variables are sensitive.
		Configuration struct {
			// RootModule is the root module's configuration.
			RootModule struct {
				// Variables are the root module's declared variables.
				Variables map[string]struct {
					// Sensitive reports the variable is declared sensitive.
					Sensitive bool `json:"sensitive"`
				} `json:"variables"`
			} `json:"root_module"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil
	}
	var out []string
	add := func(value, mask any) { out = append(out, sensitiveLeaves(value, mask, false)...) }
	for _, rc := range plan.ResourceChanges {
		add(rc.Change.Before, rc.Change.BeforeSensitive)
		add(rc.Change.After, rc.Change.AfterSensitive)
	}
	for _, oc := range plan.OutputChanges {
		add(oc.Before, oc.BeforeSensitive)
		add(oc.After, oc.AfterSensitive)
	}
	for name, decl := range plan.Configuration.RootModule.Variables {
		if v, ok := plan.Variables[name]; ok && decl.Sensitive {
			out = append(out, sensitiveLeaves(v.Value, true, false)...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// planChange is one change in a plan's JSON rendering: the values before and after, and the masks
// that say which parts of each are sensitive.
type planChange struct {
	// Before is the value before the change.
	Before any `json:"before"`
	// After is the value after the change.
	After any `json:"after"`
	// BeforeSensitive marks which parts of Before are sensitive: true for all of it, or a structure
	// shaped like the value with true where it is.
	BeforeSensitive any `json:"before_sensitive"`
	// AfterSensitive marks which parts of After are sensitive the same way.
	AfterSensitive any `json:"after_sensitive"`
}

// sensitiveLeaves returns the non-empty string leaves of value that mask marks sensitive. A mask of
// true marks everything beneath it, and an object or list mask marks its matching members.
func sensitiveLeaves(value, mask any, inherited bool) []string {
	if b, ok := mask.(bool); ok && b {
		inherited = true
	}
	switch v := value.(type) {
	case string:
		if inherited && v != "" {
			return []string{v}
		}
	case map[string]any:
		var out []string
		m, _ := mask.(map[string]any)
		for k, child := range v {
			out = append(out, sensitiveLeaves(child, m[k], inherited)...)
		}
		return out
	case []any:
		var out []string
		l, _ := mask.([]any)
		for i, child := range v {
			var childMask any
			if i < len(l) {
				childMask = l[i]
			}
			out = append(out, sensitiveLeaves(child, childMask, inherited)...)
		}
		return out
	}
	return nil
}
