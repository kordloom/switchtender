// Package template holds job templates: saved launch presets that bundle a project, playbook,
// inventory, shards, credentials, and extra vars so a run launches in one action instead of a
// hand-built request.
package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
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
	// Playbook is the playbook path, relative to the project when one is set.
	Playbook string `json:"playbook"`
	// Inventory is the inventory path, relative to the project when one is set.
	Inventory string `json:"inventory,omitempty"`
	// InventoryID names a stored inventory to materialize for the run, taking precedence over the
	// Inventory path when set.
	InventoryID string `json:"inventory_id,omitempty"`
	// Shards, when two or more, splits the run across that many inventory slices.
	Shards int `json:"shards,omitempty"`
	// Queue restricts launches to workers serving this queue.
	Queue string `json:"queue,omitempty"`
	// CredentialIDs names stored credentials materialized for the run.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ExtraVars are injected into the run as extra vars, under any survey answers.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Survey prompts the launcher for typed values that become extra vars.
	Survey []SurveyField `json:"survey,omitempty"`
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
		for _, c := range f.Choices {
			if c == s {
				return s, nil
			}
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
	// Get returns the template with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Template, error)
	// List returns all templates ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Template, error)
	// Delete removes the template with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random template identifier prefixed with "tpl_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("template: read random: " + err.Error())
	}
	return "tpl_" + hex.EncodeToString(b[:])
}
