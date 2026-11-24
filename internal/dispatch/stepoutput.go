package dispatch

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// layerOutputs copies a step's published outputs over vars and records in from that step as the
// origin of each key, so a later step can tell a value the run was approved with from one a step
// published while it ran.
func layerOutputs(vars map[string]any, from map[string]string, step string, outputs map[string]any) {
	maps.Copy(vars, outputs)
	for k := range outputs {
		from[k] = step
	}
}

// stepRefusal is outputRefusal with the names the workflow's credentials inject read as step is
// about to start, and only when it takes a published value, so a workflow that publishes nothing
// reads no credential, and a credential or type edited after the workflow began is judged as the
// step will run with it.
func (d *Dispatcher) stepRefusal(ctx context.Context, step run.PipelineStep, parent *run.Run,
	vars map[string]any, from map[string]string) string {
	var creds credentialVars
	if len(from) > 0 && !step.IsApproval() {
		creds = d.injectedVarNames(ctx, parent)
	}
	return outputRefusal(step, parent, creds, vars, from)
}

// outputRefusal reports why step may not start with the published values in vars, or an empty
// string when it may. parent is the workflow run, whose extra vars and secret survey answers are
// what a person approved. from names the step that published each key still in force.
//
// A published key that names a var the workflow was approved with is refused for every step that
// is not an approval, whatever its tool. A step's outputs are whatever its play printed with
// set_stats, which can echo a host's facts or a file it read, so a host could swap an approved
// target_env or confirm flag for a plain value of its own, and the step would run with a value no
// person saw. A value equal to the approved one is refused too, so the rule has one meaning. An
// output replacing an earlier step's output of the same name stays legal.
//
// creds holds the names the workflow's credentials inject. The tool reads a step's vars after a
// credential's, so a published key that names a var a credential injects into that step's tool is
// refused: a host could otherwise publish a custom credential's api_host and send its token to a
// server of its own on the next step. A credential's names that could not be read refuse any key on
// a tool a credential injects into, since nothing then shows the key is not one of them.
//
// A published value reaches an Ansible step as an extra var, and Ansible renders an extra var on
// the controller, so a value holding template code ran a lookup on the controller in the next
// step, and a key named ansible_ chose the next step's interpreter or connection. A step that is
// not Ansible reads its vars as plain values, so those two checks apply to Ansible steps only.
// Keys are checked in order, so the reason is stable.
func outputRefusal(step run.PipelineStep, parent *run.Run, creds credentialVars, vars map[string]any,
	from map[string]string) string {
	if step.IsApproval() {
		return ""
	}
	keys := slices.Sorted(maps.Keys(from))
	for _, k := range keys {
		if approvedVar(parent, k) {
			return fmt.Sprintf("step %q cannot take %q from step %q: the workflow was approved with "+
				"its own value for that name, and a published value cannot replace it, so the "+
				"workflow stopped. Publish it under another name, or remove %q from the "+
				"workflow's vars", step.Name, k, from[k], k)
		}
	}
	if reason := creds.refusal(step, keys, from); reason != "" {
		return reason
	}
	if run.NormalizeTool(step.Tool) != run.ToolAnsible {
		return ""
	}
	for _, k := range keys {
		switch {
		case strings.HasPrefix(strings.ToLower(k), "ansible_"):
			return fmt.Sprintf("step %q cannot take %q from step %q: a published name that begins "+
				"with ansible_ would set how Ansible connects or runs, so the workflow stopped",
				step.Name, k, from[k])
		case run.HoldsTemplate(map[string]any{k: vars[k]}):
			return fmt.Sprintf("step %q cannot take %q from step %q: the value holds template "+
				"code, which Ansible would render on the controller, so the workflow stopped",
				step.Name, k, from[k])
		}
	}
	return ""
}

// approvedVar reports whether name is one of the workflow's own extra vars or secret survey
// answers, the values a person approved it with.
func approvedVar(parent *run.Run, name string) bool {
	if parent == nil {
		return false
	}
	if _, ok := parent.ExtraVars[name]; ok {
		return true
	}
	return slices.Contains(parent.SealedNames, name)
}

// credentialVars holds the var names the workflow's credentials inject, each mapped to the name of
// the credential that injects it.
type credentialVars struct {
	// ansible maps an extra var a credential injects to the credential's name.
	ansible map[string]string
	// terraform maps a Terraform variable a credential sets through TF_VAR_<name> to the
	// credential's name.
	terraform map[string]string
	// err is why the credentials could not be read, nil when they were.
	err error
}

// refusal reports why step may not take the published keys, named in order by keys, because a
// credential injects a var of the same name into the step's tool, or an empty string when it may.
//
// Ansible reads the run's extra vars file after every credential's, and the Terraform and OpenTofu
// runners set TF_VAR_<name> for each run var after the credential environment, so in both a
// published value replaced the credential's own. A bash or Python step reads neither, so it is not
// refused.
func (c credentialVars) refusal(step run.PipelineStep, keys []string, from map[string]string) string {
	var injected map[string]string
	var how func(name string) string
	switch run.NormalizeTool(step.Tool) {
	case run.ToolAnsible:
		injected = c.ansible
		how = func(string) string { return "a var of that name" }
	case run.ToolTerraform, run.ToolOpenTofu:
		injected = c.terraform
		how = func(name string) string { return "TF_VAR_" + name }
	default:
		return ""
	}
	if len(keys) == 0 {
		return ""
	}
	if c.err != nil {
		return fmt.Sprintf("step %q cannot take %q from step %q: the workflow's credentials "+
			"could not be read to check that none of them injects that name (%v), so the "+
			"workflow stopped", step.Name, keys[0], from[keys[0]], c.err)
	}
	for _, k := range keys {
		if name, ok := injected[k]; ok {
			return fmt.Sprintf("step %q cannot take %q from step %q: credential %q injects "+
				"%s, and a published value cannot replace it, so the workflow stopped. Publish "+
				"it under another name", step.Name, k, from[k], name, how(k))
		}
	}
	return ""
}

// injectedVarNames reads the var names the workflow's credentials inject, from the credentials'
// records and their types alone, so no secret is opened before a step runs. Every step runs with
// the workflow's credentials and its inventory's, so one read covers the walk.
//
// Only a custom type is read. A built-in kind injects only ansible_ extra vars, which a published
// key may not name on an Ansible step anyway, and a registered injector returns no extra vars and
// sets no TF_VAR_ name, which TestRegisteredInjectorsSetNoVars pins. A credential or type that
// cannot be read is recorded, not skipped, so the check fails closed.
func (d *Dispatcher) injectedVarNames(ctx context.Context, parent *run.Run) credentialVars {
	var c credentialVars
	ids := d.effectiveCredentialIDs(ctx, parent)
	if len(ids) == 0 {
		return c
	}
	if d.credentials == nil {
		c.err = fmt.Errorf("no credential store: %w", credential.ErrNoKey)
		return c
	}
	for _, id := range ids {
		cred, err := d.credentials.Get(ctx, id)
		if err != nil {
			c.err = fmt.Errorf("credential %s: %w", id, err)
			return c
		}
		if cred.TypeID == "" {
			continue
		}
		if d.credentialTypes == nil {
			c.err = fmt.Errorf("credential %q names custom type %s, and none are configured",
				cred.Name, cred.TypeID)
			return c
		}
		typ, err := d.credentialTypes.Get(ctx, cred.TypeID)
		if err != nil {
			c.err = fmt.Errorf("credential %q: read type %s: %w", cred.Name, cred.TypeID, err)
			return c
		}
		for name := range typ.ExtraVarInjectors {
			c.ansible = addName(c.ansible, name, cred.Name)
		}
		for env := range typ.EnvInjectors {
			if name, ok := strings.CutPrefix(env, "TF_VAR_"); ok && name != "" {
				c.terraform = addName(c.terraform, name, cred.Name)
			}
		}
	}
	return c
}

// addName records in names that the credential called cred injects name, keeping the first
// credential that does, and returns the map, made when it was nil.
func addName(names map[string]string, name, cred string) map[string]string {
	if names == nil {
		names = map[string]string{}
	}
	if _, ok := names[name]; !ok {
		names[name] = cred
	}
	return names
}
