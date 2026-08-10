// Package template holds job templates: saved launch presets that bundle a project, playbook,
// inventory, shards, credentials, and extra vars so a run launches in one action instead of a
// hand-built request.
package template

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
)

var (
	// ErrNotFound is returned when a template does not exist in the store.
	ErrNotFound = errors.New("template not found")
	// ErrSurvey is returned when launch answers do not satisfy a template's survey.
	ErrSurvey = errors.New("survey answer invalid")
)

// FieldType names the kind of a survey field.
type FieldType string

const (
	// FieldText is a free text string.
	FieldText FieldType = "text"
	// FieldInt is an integer.
	FieldInt FieldType = "int"
	// FieldBool is a true or false toggle.
	FieldBool FieldType = "bool"
	// FieldChoice is one of a fixed set of strings.
	FieldChoice FieldType = "choice"
)

// SurveyField is one prompt shown at launch, whose answer becomes an extra var.
type SurveyField struct {
	// Var is the extra var name the answer is stored under.
	Var string `json:"var"`
	// Label is the human prompt.
	Label string `json:"label"`
	// Type is the field kind.
	Type FieldType `json:"type"`
	// Required rejects a launch that omits the field.
	Required bool `json:"required,omitempty"`
	// Default is used when the field is optional and unanswered.
	Default any `json:"default,omitempty"`
	// Choices lists the allowed values for a choice field.
	Choices []string `json:"choices,omitempty"`
}

// Template is one saved launch preset.
type Template struct {
	// ID is the unique template identifier.
	ID string `json:"id"`
	// Name labels the template for humans, for example deploy production.
	Name string `json:"name"`
	// ProjectID sources the playbook from a git project. Empty for local paths.
	ProjectID string `json:"project_id,omitempty"`
	// Playbook is the playbook path, relative to the project when one is set. Used by the Ansible tool.
	Playbook string `json:"playbook"`
	// Inventory is the inventory path, relative to the project when one is set.
	Inventory string `json:"inventory,omitempty"`
	// InventoryID names a stored inventory to materialize for the run, taking precedence over the
	// Inventory path when set.
	InventoryID string `json:"inventory_id,omitempty"`
	// Tool selects the execution engine: ansible, bash, terraform, opentofu, python, powershell, or go. Empty means ansible.
	Tool string `json:"tool,omitempty"`
	// Command carries the tool's primary input for non-Ansible tools: the script for bash and python,
	// the working directory for terraform.
	Command string `json:"command,omitempty"`
	// DryRun runs the tool in its no-change mode when the template launches.
	DryRun bool `json:"dry_run,omitempty"`
	// Tags runs only the Ansible plays and tasks carrying one of these tags on every launch.
	Tags []string `json:"tags,omitempty"`
	// SkipTags skips the Ansible plays and tasks carrying one of these tags on every launch.
	SkipTags []string `json:"skip_tags,omitempty"`
	// Verbosity raises Ansible logging from 0 to 4 on every launch.
	Verbosity int `json:"verbosity,omitempty"`
	// Forks sets how many hosts Ansible addresses in parallel on every launch. Zero leaves the default.
	Forks int `json:"forks,omitempty"`
	// DiffMode shows the before-and-after of every Ansible file and template change on every launch.
	DiffMode bool `json:"diff_mode,omitempty"`
	// Shards, when two or more, splits the run across that many inventory slices.
	Shards int `json:"shards,omitempty"`
	// Queue restricts launches to workers serving this queue.
	Queue string `json:"queue,omitempty"`
	// Timeout caps how many seconds a launch may execute before it is canceled and failed. Zero
	// leaves the launch on the server default, so a template that sets nothing behaves as before.
	Timeout int `json:"timeout,omitempty"`
	// Image names a container image every launch executes inside, its execution environment. It
	// outranks the project's image. Only the Ansible tool runs in a container.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image. Empty for public.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// CredentialIDs names stored credentials materialized for every launch of the template.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// SelectableCredentialIDs names credentials a launch may choose from, prompt on launch. A launch
	// applies its chosen subset on top of CredentialIDs, and a choice outside this set is rejected, so
	// a template offers a constrained menu rather than any credential.
	SelectableCredentialIDs []string `json:"selectable_credential_ids,omitempty"`
	// ExtraVars are injected into the run as extra vars, under any survey answers.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Survey prompts the launcher for typed values that become extra vars.
	Survey []SurveyField `json:"survey,omitempty"`
	// ConfirmOnLaunch routes the plain Launch action through the overrides dialog, so a risky
	// template is reviewed each time instead of firing on one click.
	ConfirmOnLaunch bool `json:"confirm_on_launch,omitempty"`
	// Notifications route every launch's terminal state to specific channels, beyond the server-wide
	// ones, so a template pages its own team.
	Notifications []run.NotifyTarget `json:"notifications,omitempty"`
	// OrgID is the owning organization. Empty means unowned, a global object that follows the role.
	// When set, members of the organization gain access to the template and, under strict grants, it
	// is hidden from non-members who lack an explicit grant.
	OrgID string `json:"org_id,omitempty"`
	// CreatedAt is when the template was created.
	CreatedAt time.Time `json:"created_at"`
}

// ResolveSurvey validates launch answers against the survey and returns the values to inject as
// extra vars: each answered field coerced to its type, plus defaults for optional unanswered
// fields. It fails when a required field is missing or an answer does not match its type.
func ResolveSurvey(fields []SurveyField, answers map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range fields {
		raw, given := answers[f.Var]
		if !given || raw == nil {
			if f.Required {
				return nil, fmt.Errorf("%w: %q is required", ErrSurvey, f.Var)
			}
			if f.Default != nil {
				out[f.Var] = f.Default
			}
			continue
		}
		val, err := coerce(f, raw)
		if err != nil {
			return nil, err
		}
		out[f.Var] = val
	}
	return out, nil
}

// coerce converts a raw answer to the field's type or reports why it cannot.
func coerce(f SurveyField, raw any) (any, error) {
	switch f.Type {
	case FieldText, "":
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %q must be text", ErrSurvey, f.Var)
		}
		return s, nil
	case FieldInt:
		switch n := raw.(type) {
		case float64:
			return int(n), nil
		case int:
			return n, nil
		default:
			return nil, fmt.Errorf("%w: %q must be an integer", ErrSurvey, f.Var)
		}
	case FieldBool:
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("%w: %q must be true or false", ErrSurvey, f.Var)
		}
		return b, nil
	case FieldChoice:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %q must be one of its choices", ErrSurvey, f.Var)
		}
		if slices.Contains(f.Choices, s) {
			return s, nil
		}
		return nil, fmt.Errorf("%w: %q is not an allowed choice for %q", ErrSurvey, s, f.Var)
	default:
		return nil, fmt.Errorf("%w: %q has an unknown field type %q", ErrSurvey, f.Var, f.Type)
	}
}

// Store persists templates. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the template identified by t.ID.
	Save(ctx context.Context, t *Template) error
	// Update changes an existing template's fields, preserving its creation time, or returns
	// ErrNotFound.
	Update(ctx context.Context, t *Template) error
	// Get returns the template with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Template, error)
	// List returns all templates ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Template, error)
	// Delete removes the template with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random template identifier prefixed with "tpl_".
func NewID() string {
	return idgen.New("tpl_", 6)
}

// LaunchOptions returns the submit options that carry the template's own saved settings onto a run.
//
// A template is a saved launch preset, so every path that fires one has to apply the same settings or
// the preset means something different depending on how it was triggered. They had drifted: the API
// applied everything, a schedule dropped the tool, command, dry-run flag, inventory, and execution
// image, and a webhook dropped those plus notifications. A scheduled Bash template therefore fired as
// an Ansible run with no playbook, and a template saved as dry-run-only made real changes on every
// scheduled fire.
//
// Callers append what only they know: the source and actor that fired it, and any per-launch
// overrides such as a host limit or a substituted inventory.
func (t *Template) LaunchOptions() []run.SubmitOption {
	opts := []run.SubmitOption{
		run.WithCredentialIDs(t.CredentialIDs),
		run.WithExtraVars(t.ExtraVars),
		run.WithTool(t.Tool),
		run.WithCommand(t.Command),
		run.WithDryRun(t.DryRun),
		run.WithTags(t.Tags...),
		run.WithSkipTags(t.SkipTags...),
		run.WithVerbosity(t.Verbosity),
		run.WithForks(t.Forks),
		run.WithDiffMode(t.DiffMode),
	}
	if t.ProjectID != "" {
		opts = append(opts, run.WithProject(t.ProjectID))
	}
	if t.InventoryID != "" {
		opts = append(opts, run.WithInventory(t.InventoryID))
	}
	if t.Queue != "" {
		opts = append(opts, run.WithQueue(t.Queue))
	}
	if t.Timeout > 0 {
		opts = append(opts, run.WithTimeout(t.Timeout))
	}
	if t.Image != "" {
		opts = append(opts, run.WithImage(t.Image, t.PullCredentialID))
	}
	if len(t.Notifications) > 0 {
		opts = append(opts, run.WithNotifications(t.Notifications))
	}
	return opts
}
