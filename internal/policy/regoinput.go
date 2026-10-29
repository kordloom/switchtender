package policy

import (
	"sort"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// RegoInputVersion is the version of the input document a Rego policy is evaluated against. It
// changes only when a field is removed or changes meaning, so a module written against version 1
// keeps reading the same document across releases.
const RegoInputVersion = 1

// RegoInput builds the input document a Rego policy is evaluated against for r. Every field is
// always present, with an empty value when the run has none, so a module never meets a missing
// field. The shape is documented in docs/policy.md and versioned by RegoInputVersion.
//
// Extra variables are given by name only. Their values are where secrets ride, and a message a
// module builds from one would be written into the run record in plain text.
func RegoInput(r *run.Run) map[string]any {
	risk := run.AssessRisk(r)
	undo := reversibilityGrade(r)
	submitted := r.CreatedAt
	if submitted.IsZero() {
		submitted = time.Now()
	}
	var destroys any
	if r.PlanDestroys != nil {
		destroys = *r.PlanDestroys
	}
	return map[string]any{
		"version": RegoInputVersion,
		"run": map[string]any{
			"id":         r.ID,
			"tool":       run.NormalizeTool(r.Tool),
			"command":    r.Command,
			"playbook":   r.Playbook,
			"inventory":  map[string]any{"id": r.InventoryID, "path": r.Inventory},
			"limit":      r.Limit,
			"queue":      r.Queue,
			"project_id": r.ProjectID,
			"dry_run":    r.DryRun,
			// The tool flag above says what the run asked for. These two say what it is: a dry
			// run the gate's scan found running work for real, or could not read in full, is not
			// change free, whether its playbook forces tasks under check mode or its
			// configuration runs a program while it plans.
			"dry_run_findings": stringList(r.DryRunFindings()),
			"change_free":      r.ChangeFree(),
			"kind":             r.Kind,
			"step_name":        r.StepName,
			"source":           r.Source,
			"source_id":        r.SourceID,
			"template_id":      r.TemplateID,
			"proposed_from":    r.ProposedFrom,
			"intent":           r.Intent,
			"image":            r.Image,
			"tags":             stringList(r.Tags),
			"skip_tags":        stringList(r.SkipTags),
			"labels":           stringMap(r.Labels),
			"extra_var_names":  stringList(sortedKeys(r.ExtraVars)),
			"credential_ids":   stringList(r.CredentialIDs),
		},
		"actor": map[string]any{
			"name":       r.Actor,
			"type":       r.ActorType,
			"kind":       actorKindOf(r.ActorType),
			"account_id": r.ActorUserID,
		},
		"plan": map[string]any{
			"planned":  r.PlanDestroys != nil,
			"destroys": destroys,
		},
		"risk": map[string]any{
			"level":   risk.Level,
			"reasons": stringList(risk.Reasons),
		},
		"reversibility": map[string]any{
			"class":   undo.Class,
			"reasons": stringList(undo.Reasons),
		},
		"time": map[string]any{
			"submitted_at":    submitted.UTC().Format(time.RFC3339Nano),
			"submitted_at_ns": submitted.UnixNano(),
		},
	}
}

// actorKindOf names who fired a run in the actor_kind vocabulary: agent, human, or other for a
// webhook, a schedule, or a source the server does not know.
func actorKindOf(actorType string) string {
	switch {
	case actorType == ActorKindAgent:
		return ActorKindAgent
	case humanActorTypes[actorType]:
		return ActorKindHuman
	default:
		return "other"
	}
}

// reversibilityGrade reads the grade already attached to a run, falling back to grading it here, the
// same preference reversibilityOf makes for the YAML floor.
func reversibilityGrade(r *run.Run) run.Reversibility {
	if r.Reversibility != nil {
		return *r.Reversibility
	}
	return run.AssessReversibility(r)
}

// stringList converts a string slice to the list form the input document uses, never nil.
func stringList(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// stringMap converts a string map to the object form the input document uses, never nil.
func stringMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RegoInputSchema is the shape of the input document, which the paid compiler holds every module's
// references against. A nested map is an object with fixed keys, RegoAnyKeys is an object whose keys
// are the operator's own, and nil is a value with nothing named below it. A test holds this to the
// document RegoInput builds, so the two cannot drift.
var RegoInputSchema = map[string]any{
	"version": nil,
	"run": map[string]any{
		"id": nil, "tool": nil, "command": nil, "playbook": nil,
		"inventory": map[string]any{"id": nil, "path": nil},
		"limit":     nil, "queue": nil, "project_id": nil, "dry_run": nil, "kind": nil,
		"dry_run_findings": nil, "change_free": nil,
		"step_name": nil, "source": nil, "source_id": nil, "template_id": nil,
		"proposed_from": nil, "intent": nil, "image": nil, "tags": nil, "skip_tags": nil,
		"labels":          RegoAnyKeys,
		"extra_var_names": nil, "credential_ids": nil,
	},
	"actor":         map[string]any{"name": nil, "type": nil, "kind": nil, "account_id": nil},
	"plan":          map[string]any{"planned": nil, "destroys": nil},
	"risk":          map[string]any{"level": nil, "reasons": nil},
	"reversibility": map[string]any{"class": nil, "reasons": nil},
	"time":          map[string]any{"submitted_at": nil, "submitted_at_ns": nil},
}

// RegoAnyKeysMarker is the type of RegoAnyKeys.
type RegoAnyKeysMarker struct{}

// RegoAnyKeys marks an object in the schema whose keys are not fixed, such as the run's labels.
var RegoAnyKeys = RegoAnyKeysMarker{}
