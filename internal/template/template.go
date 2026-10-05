// Package template holds job templates: saved launch presets that bundle a project, playbook,
// inventory, shards, credentials, and extra vars so a run launches in one action instead of a
// hand-built request.
package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
)

var (
	// ErrNotFound is returned when a template does not exist in the store.
	ErrNotFound = errors.New("template not found")
	// ErrSurvey is returned when launch answers do not satisfy a template's survey.
	ErrSurvey = errors.New("survey answer invalid")
	// ErrSurveyField is returned when a survey field's own definition is malformed, which is a
	// problem with the template rather than with an answer.
	ErrSurveyField = errors.New("survey field invalid")
	// ErrUnanswered is returned when a launch with nobody present to answer the survey reaches a
	// required question it has no usable answer for. An *UnansweredError carrying it names them.
	ErrUnanswered = errors.New("survey question unanswered")
)

// FieldType names the kind of a survey field.
type FieldType string

// fieldTypeSpellings are the names other tools and the JSON Schema vocabulary give a field type,
// read as the type a field stores. A template posted with "integer" or "boolean" was refused as an
// unknown type, though the words mean int and bool to anyone who writes them.
var fieldTypeSpellings = map[string]FieldType{
	"integer": FieldInt, "boolean": FieldBool, "string": FieldText, "textarea": FieldMultiline,
	"password": FieldSecret,
}

// UnmarshalJSON reads a field type, taking the common spellings in fieldTypeSpellings as the type
// they name.
func (t *FieldType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if typ, ok := fieldTypeSpellings[strings.ToLower(s)]; ok {
		*t = typ
		return nil
	}
	*t = FieldType(s)
	return nil
}

const (
	// FieldText is a free text string.
	FieldText FieldType = "text"
	// FieldInt is an integer.
	FieldInt FieldType = "int"
	// FieldBool is a true or false toggle.
	FieldBool FieldType = "bool"
	// FieldChoice is one of a fixed set of strings.
	FieldChoice FieldType = "choice"
	// FieldMultiline is free text spanning several lines, such as a block of variables or a note.
	FieldMultiline FieldType = "multiline"
	// FieldSecret is a text answer that is a secret, such as a password or a token. Its answer is
	// sealed with the credential key the moment a launch is accepted, is never stored on the run in
	// plain text, and is opened only inside the execution that uses it. AWX calls this type password.
	FieldSecret FieldType = "secret"
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
	// Help is optional guidance shown beneath the prompt.
	Help string `json:"help,omitempty"`
	// Min and Max bound an int field's answer, inclusive. Nil leaves that side unbounded.
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
	// MinLength and MaxLength bound a text or multiline answer's length. Zero leaves that side
	// unbounded, except a MinLength of zero on a required field still rejects an empty answer.
	MinLength int `json:"min_length,omitempty"`
	MaxLength int `json:"max_length,omitempty"`
	// Pattern is a regular expression a text answer must match in full. Empty imposes no pattern.
	Pattern string `json:"pattern,omitempty"`
	// SealedDefault is a secret field's default, sealed with the credential key. A secret field never
	// keeps a plain Default: the server seals one when the template is saved, and every read shows
	// the default as set or not set rather than returning it. It is stored and backed up with the
	// template, never accepted from a caller, and never written into an API response.
	SealedDefault string `json:"sealed_default,omitempty"`
}

// Secret reports whether the field collects a secret answer.
func (f SurveyField) Secret() bool {
	return f.Type == FieldSecret
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
	// Limit narrows every launch to the hosts matching this pattern, the way an operator types
	// --limit by hand. A template that pins one is safe to fire unattended: a schedule and a webhook
	// carry it too, where before they reached the whole inventory because only an interactive launch
	// could supply one. Empty targets everything the inventory holds.
	Limit string `json:"limit,omitempty"`
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
	// outranks the project's image. Every tool the container runner knows executes inside it.
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
	// Steps, when set, make the template a saved workflow: a pipeline graph fired as one run instead
	// of a single tool launch. A stepped template carries no top-level tool, playbook, command, or
	// Ansible controls, since each step names its own; the survey answers, extra vars, credentials,
	// project, inventory, and image still apply to the whole workflow.
	Steps []run.PipelineStep `json:"steps,omitempty"`
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
	// UseFactCache keeps the facts each launch gathers for every host of the stored inventory and
	// serves them to the next launch through Ansible's jsonfile fact cache, the way AWX's
	// use_fact_cache does. It needs a stored inventory, since facts are kept per inventory host.
	UseFactCache bool `json:"use_fact_cache,omitempty"`
	// FactCacheTimeout is how many seconds cached facts stay fresh enough to serve to a launch. Zero
	// serves them however old they are, which is the AWX default.
	FactCacheTimeout int `json:"fact_cache_timeout,omitempty"`
	// AllowCallbacks lets a host in the template's stored inventory launch the template against
	// itself by posting the host config key to the template's callback URL, the way AWX's
	// provisioning callbacks do. A callback is refused until a key has been minted.
	AllowCallbacks bool `json:"allow_callbacks,omitempty"`
	// HostConfigKey is the provisioning callback key, sealed with the server key. It never
	// serializes: the plaintext is shown once when it is minted and is never returned again.
	HostConfigKey string `json:"-"`
	// HostConfigKeySet reports whether a callback key has been minted. It is filled in when a
	// template is served and is never stored, so a reader learns that a key exists and nothing else.
	HostConfigKeySet bool `json:"host_config_key_set,omitempty"`
	// CallbackLimit says what a provisioning callback does with Limit: CallbackLimitIntersect, the
	// default, launches only when the calling host also falls within it, and CallbackLimitReplace
	// launches against the calling host whatever it says, as AWX does. Empty means intersect.
	CallbackLimit string `json:"callback_limit,omitempty"`
	// AWXCallback answers provisioning callbacks on the AWX-compatible address as well, for a
	// template an import bound to its AWX job template id. See AWXBinding.
	AWXCallback bool `json:"awx_callback,omitempty"`
	// AWXJobTemplateID is the AWX job template id bound to the template. It is filled in when a
	// template is served or planned for import and is never stored on the template.
	AWXJobTemplateID int64 `json:"awx_job_template_id,omitempty"`
	// AWXCallbackCalledAt is when a host last called the template through the AWX-compatible
	// address. It is filled in when a template is served and is never stored on the template.
	AWXCallbackCalledAt *time.Time `json:"awx_callback_called_at,omitempty"`
	// CreatedAt is when the template was created.
	CreatedAt time.Time `json:"created_at"`
}

// ResolveSurvey validates launch answers against the survey and returns the values to inject as
// extra vars: each answered field coerced to its type, plus defaults for optional unanswered
// fields. It fails when a required field is missing or an answer does not match its type. A secret
// field is validated like any other but its answer is not among the values returned, since it must
// never become a plain extra var. ResolveSurveyAnswers returns it.
func ResolveSurvey(fields []SurveyField, answers map[string]any) (map[string]any, error) {
	vars, _, err := ResolveSurveyAnswers(fields, answers)
	return vars, err
}

// SecretAnswers holds the resolved answers to a survey's secret fields, kept apart from the plain
// extra vars so no caller can put one on a run in plain text by accident.
type SecretAnswers struct {
	// Plain maps a secret field's var to the answer the launcher supplied. The caller seals each one
	// before anything is stored and drops the map.
	Plain map[string]string `json:"-"`
	// Sealed maps a secret field's var to its template default, which was sealed when the template
	// was saved and is carried onto the run sealed, without being opened.
	Sealed map[string]string `json:"-"`
}

// Empty reports whether no secret field produced a value.
func (s SecretAnswers) Empty() bool {
	return len(s.Plain) == 0 && len(s.Sealed) == 0
}

// ResolveSurveyAnswers validates launch answers against the survey and returns the plain values to
// inject as extra vars and, separately, the answers to its secret fields. A secret field left
// unanswered falls back to its sealed default when it has one.
func ResolveSurveyAnswers(fields []SurveyField, answers map[string]any) (map[string]any, SecretAnswers, error) {
	out := map[string]any{}
	var secrets SecretAnswers
	for _, f := range fields {
		raw, given := answers[f.Var]
		if !given || raw == nil {
			if f.Required {
				return nil, SecretAnswers{}, fmt.Errorf("%w: %q is required", ErrSurvey, f.Var)
			}
			switch {
			case f.Secret() && f.SealedDefault != "":
				if secrets.Sealed == nil {
					secrets.Sealed = map[string]string{}
				}
				secrets.Sealed[f.Var] = f.SealedDefault
			case !f.Secret() && f.Default != nil:
				out[f.Var] = f.Default
			}
			continue
		}
		val, err := coerce(f, raw)
		if err != nil {
			return nil, SecretAnswers{}, err
		}
		if f.Secret() {
			if secrets.Plain == nil {
				secrets.Plain = map[string]string{}
			}
			secrets.Plain[f.Var], _ = val.(string)
			continue
		}
		out[f.Var] = val
	}
	return out, secrets, nil
}

// UnansweredError names the required survey questions a launch with nobody present to answer them
// has no usable answer for: a question with no default, or with a default its own rules refuse. It
// matches ErrUnanswered.
type UnansweredError struct {
	// Vars are the variables of those questions, in survey order.
	Vars []string
	// Reasons say why each one has no answer, in the same order.
	Reasons []string
}

// Error names every unanswered question and why.
func (e *UnansweredError) Error() string {
	return ErrUnanswered.Error() + ": " + strings.Join(e.Reasons, ", ")
}

// Is reports whether target is ErrUnanswered, so a caller tells this refusal apart with errors.Is.
func (e *UnansweredError) Is(target error) bool {
	return target == ErrUnanswered
}

// add records one unanswered question.
func (e *UnansweredError) add(v, reason string) {
	e.Vars = append(e.Vars, v)
	e.Reasons = append(e.Reasons, reason)
}

// UnansweredVars returns the questions an *UnansweredError in err's chain names, nil when it holds
// none.
func UnansweredVars(err error) []string {
	var u *UnansweredError
	if errors.As(err, &u) {
		return u.Vars
	}
	return nil
}

// RefuseUnattended returns the refusal a launch with nobody present to answer t's survey records when
// cause, from UnattendedOptions, says a required question has no usable answer. launch names that
// launch in the reason, such as "scheduled fire". The refusal wraps cause, so it still matches
// ErrUnanswered, and it says the two ways out, since whoever reads it is the one who has to act.
func (t *Template) RefuseUnattended(launch string, cause error) error {
	fix := "give the question a default"
	if len(UnansweredVars(cause)) > 1 {
		fix = "give each question a default"
	}
	return fmt.Errorf("refused: %w. A %s of template %q has nobody to answer it, so %s or launch "+
		"the template by hand", cause, launch, t.Name, fix)
}

// ResolveSurveyDefaults resolves a survey for a launch with nobody present to answer it: a
// schedule's fire, a webhook, a pull request plan, or a provisioning callback. It returns what such
// a launch carries in the shape ResolveSurveyAnswers does, the plain values to inject as extra vars
// and, apart from them, the sealed defaults of the secret questions, which ride onto the run
// without being opened.
//
// Every question takes its default. An optional one takes it exactly as an interactive launch that
// leaves it blank does, so the two never fire one template with different values. A required one
// takes it too, since nobody is there to type the answer, and so its default is held to the rules
// an answer is held to. A required question with no default, an empty one, or one those rules
// refuse has no answer at all, and the launch is refused with an *UnansweredError naming every such
// question: the only other outcome is a run missing an answer its template says it needs.
func ResolveSurveyDefaults(fields []SurveyField) (map[string]any, SecretAnswers, error) {
	out := map[string]any{}
	var secrets SecretAnswers
	var missing UnansweredError
	for _, f := range fields {
		if f.Secret() {
			switch {
			case f.SealedDefault != "":
				if secrets.Sealed == nil {
					secrets.Sealed = map[string]string{}
				}
				secrets.Sealed[f.Var] = f.SealedDefault
			case f.Required:
				missing.add(f.Var, fmt.Sprintf("%q is required and has no default", f.Var))
			}
			continue
		}
		if !f.Required {
			if f.Default != nil {
				out[f.Var] = f.Default
			}
			continue
		}
		if s, ok := f.Default.(string); f.Default == nil || (ok && s == "") {
			missing.add(f.Var, fmt.Sprintf("%q is required and has no default", f.Var))
			continue
		}
		val, err := coerce(f, f.Default)
		if err != nil {
			missing.add(f.Var, fmt.Sprintf("%q is required and its default is not a valid answer: %s",
				f.Var, strings.TrimPrefix(err.Error(), ErrSurvey.Error()+": ")))
			continue
		}
		out[f.Var] = val
	}
	if len(missing.Vars) > 0 {
		return nil, SecretAnswers{}, &missing
	}
	return out, secrets, nil
}

// UnattendedVars returns the variables a launch with nobody present to answer the template's survey
// carries: the template's own extra vars with the survey's defaults over them, and, apart from
// them, the sealed defaults of its secret questions. A template extra var named like a secret
// question that has a sealed default is dropped, so the sealed default is the only value that
// variable has, the rule an interactive launch follows. It fails with an *UnansweredError when a
// required question has no usable default.
func (t *Template) UnattendedVars() (map[string]any, map[string]string, error) {
	vars := maps.Clone(t.ExtraVars)
	if len(t.Survey) == 0 {
		return vars, nil, nil
	}
	resolved, secrets, err := ResolveSurveyDefaults(t.Survey)
	if err != nil {
		return nil, nil, err
	}
	if vars == nil {
		vars = map[string]any{}
	}
	maps.Copy(vars, resolved)
	for name := range secrets.Sealed {
		delete(vars, name)
	}
	return vars, secrets.Sealed, nil
}

// UnattendedOptions returns the submit options that apply the template's survey to a launch with
// nobody present to answer it, to append after LaunchOptions: the variables UnattendedVars resolves,
// in place of the template's own, and the sealed defaults beside them.
//
// Schedules, webhooks, pull request plans, and provisioning callbacks all fire a template this way.
// A schedule and a webhook used to fire with LaunchOptions alone, which left the survey out
// entirely: an optional question's default never reached the play, a secret question's sealed
// default never reached it either, and a template with a required question nobody answered fired
// anyway, so the run went ahead without an answer its template says it cannot do without.
func (t *Template) UnattendedOptions() ([]run.SubmitOption, error) {
	vars, sealed, err := t.UnattendedVars()
	if err != nil {
		return nil, err
	}
	return []run.SubmitOption{run.WithExactExtraVars(vars), run.WithSealedVars(sealed)}, nil
}

// SealDefaults prepares a survey for storage: every secret field's default is sealed with seal and
// its plain Default cleared, so a secret default is never kept or returned as text. A SealedDefault
// sent by a caller is discarded rather than trusted. A default sent back as scrub.Marker, the form
// every read shows a set default in, keeps the sealed default of the same secret field in existing,
// so an edit that does not touch the default does not erase it. An empty default clears it.
func SealDefaults(fields, existing []SurveyField, seal func(string) (string, error)) ([]SurveyField, error) {
	if fields == nil {
		return nil, nil
	}
	out := make([]SurveyField, len(fields))
	for i, f := range fields {
		f.SealedDefault = ""
		if !f.Secret() || f.Default == nil {
			out[i] = f
			continue
		}
		plain, ok := f.Default.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %q is a secret field, so its default must be text",
				ErrSurveyField, f.Var)
		}
		f.Default = nil
		switch plain {
		case "":
		case scrub.Marker:
			kept := storedSealedDefault(existing, f.Var)
			if kept == "" {
				return nil, fmt.Errorf("%w: %q sends its default back masked, but no default is "+
					"stored for it. Send the default itself, or leave it empty for none",
					ErrSurveyField, f.Var)
			}
			f.SealedDefault = kept
		default:
			if seal == nil {
				return nil, fmt.Errorf("%w: %q has a secret default and no encryption key is "+
					"configured to seal it", ErrSurveyField, f.Var)
			}
			sealed, err := seal(plain)
			if err != nil {
				return nil, fmt.Errorf("%w: seal the default of %q: %w", ErrSurveyField, f.Var, err)
			}
			f.SealedDefault = sealed
		}
		out[i] = f
	}
	return out, nil
}

// storedSealedDefault returns the sealed default of the secret field named v in fields, or the
// empty string when there is none.
func storedSealedDefault(fields []SurveyField, v string) string {
	for _, f := range fields {
		if f.Var == v && f.Secret() {
			return f.SealedDefault
		}
	}
	return ""
}

// MaskSurvey returns a copy of fields fit for any reader: a secret field's sealed default is
// removed and its Default reads scrub.Marker when one is set, so a reader learns that a default
// exists and never its value or its ciphertext. Nothing else changes.
func MaskSurvey(fields []SurveyField) []SurveyField {
	if fields == nil {
		return nil
	}
	out := make([]SurveyField, len(fields))
	for i, f := range fields {
		if f.Secret() {
			f.Default = nil
			if f.SealedDefault != "" {
				f.Default = scrub.Marker
			}
		}
		f.SealedDefault = ""
		out[i] = f
	}
	return out
}

// ValidateSurvey checks a survey's field definitions on their own, with no launch answers, so a
// template with a malformed field is refused when it is saved rather than failing every launch. It
// compiles each text pattern, confirms a choice field offers choices, and confirms bounds are
// ordered.
func ValidateSurvey(fields []SurveyField) error {
	for _, f := range fields {
		switch f.Type {
		case FieldText, FieldMultiline, FieldSecret, "":
			if f.Secret() && f.Default != nil {
				return fmt.Errorf("%w: %q is a secret field, so its default is sealed when the "+
					"template is saved and never kept as text", ErrSurveyField, f.Var)
			}
			if f.Pattern != "" {
				if _, err := regexp.Compile(anchorPattern(f.Pattern)); err != nil {
					return fmt.Errorf("%w: %q has an invalid pattern: %v", ErrSurveyField, f.Var, err)
				}
			}
			if f.MinLength > 0 && f.MaxLength > 0 && f.MinLength > f.MaxLength {
				return fmt.Errorf("%w: %q sets min_length above max_length", ErrSurveyField, f.Var)
			}
		case FieldInt:
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				return fmt.Errorf("%w: %q sets min above max", ErrSurveyField, f.Var)
			}
		case FieldBool:
		case FieldChoice:
			if len(f.Choices) == 0 {
				return fmt.Errorf("%w: %q is a choice field with no choices", ErrSurveyField, f.Var)
			}
		default:
			return fmt.Errorf("%w: %q has the type %q, which is not one of text, multiline, secret, "+
				"int, bool, or choice", ErrSurveyField, f.Var, f.Type)
		}
	}
	return nil
}

// anchorPattern wraps a survey pattern so it must match the whole answer, not just a substring. A
// bare regexp matches anywhere, so "\\d{4}" would accept "abc1234xyz"; anchoring with \A and \z ties
// it to the full string the way a format constraint is meant to read.
func anchorPattern(p string) string {
	return `\A(?:` + p + `)\z`
}

// coerce converts a raw answer to the field's type or reports why it cannot.
func coerce(f SurveyField, raw any) (any, error) {
	switch f.Type {
	case FieldText, FieldMultiline, FieldSecret, "":
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %q must be text", ErrSurvey, f.Var)
		}
		// A required field present but empty is still unanswered. ResolveSurvey only rejects a
		// missing key, so without this an empty string satisfies a required text field.
		if f.Required && s == "" {
			return nil, fmt.Errorf("%w: %q is required", ErrSurvey, f.Var)
		}
		// Bounds count characters, not bytes, so a multibyte answer is measured the way a person
		// reads it rather than rejected for being long in UTF-8.
		n := utf8.RuneCountInString(s)
		if f.MinLength > 0 && n < f.MinLength {
			return nil, fmt.Errorf("%w: %q must be at least %d characters", ErrSurvey, f.Var, f.MinLength)
		}
		if f.MaxLength > 0 && n > f.MaxLength {
			return nil, fmt.Errorf("%w: %q must be at most %d characters", ErrSurvey, f.Var, f.MaxLength)
		}
		if f.Pattern != "" {
			re, err := regexp.Compile(anchorPattern(f.Pattern))
			if err != nil {
				return nil, fmt.Errorf("%w: %q has an invalid pattern: %v", ErrSurvey, f.Var, err)
			}
			if !re.MatchString(s) {
				return nil, fmt.Errorf("%w: %q does not match the required pattern", ErrSurvey, f.Var)
			}
		}
		return s, nil
	case FieldInt:
		var n int
		switch v := raw.(type) {
		case float64:
			n = int(v)
		case int:
			n = v
		default:
			return nil, fmt.Errorf("%w: %q must be an integer", ErrSurvey, f.Var)
		}
		if f.Min != nil && n < *f.Min {
			return nil, fmt.Errorf("%w: %q must be at least %d", ErrSurvey, f.Var, *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return nil, fmt.Errorf("%w: %q must be at most %d", ErrSurvey, f.Var, *f.Max)
		}
		return n, nil
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
	// SetHostConfigKey replaces the template's sealed provisioning callback key and nothing else, or
	// returns ErrNotFound. An empty key removes it. Update never writes the key, so an edit made
	// from a snapshot read before a rotation cannot put the old key back.
	SetHostConfigKey(ctx context.Context, id, sealed string) error
	// BindAWX records that an AWX job template id reaches the template b names. A binding the id
	// already has for the same AWX object, by organization and name, is pointed at b's template.
	// One for a different AWX object is refused with ErrAWXConflict, and nothing changes.
	BindAWX(ctx context.Context, b AWXBinding) error
	// UnbindAWX removes the binding of b's AWX job template id while it still names b's AWX object
	// and b's template, and changes nothing otherwise. It is how an import or a restore gives back an
	// id it claimed before it wrote anything, when it then stops, so a binding another import has
	// since pointed at its own template is never taken back.
	UnbindAWX(ctx context.Context, b AWXBinding) error
	// AWXBindingFor returns the binding of an AWX job template id, or ErrNotFound.
	AWXBindingFor(ctx context.Context, awxID int64) (*AWXBinding, error)
	// AWXBindings returns every binding, including those whose template was deleted, ordered by
	// AWX id.
	AWXBindings(ctx context.Context) ([]AWXBinding, error)
	// MarkAWXCalled records that a host called through the AWX-compatible address of awxID at at,
	// or returns ErrNotFound.
	MarkAWXCalled(ctx context.Context, awxID int64, at time.Time) error
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
		run.WithTemplate(t.ID),
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
		run.WithFactCache(t.UseFactCache, t.FactCacheTimeout),
	}
	if t.Limit != "" {
		opts = append(opts, run.WithLimit(t.Limit))
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
