package credential

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrBadType is returned when a custom credential type is malformed.
var ErrBadType = errors.New("invalid credential type")

// fieldNamePattern and envNamePattern bound what a type may name. A field name is what an operator
// references in an injector, and an environment variable name is what reaches the process, so both
// are held to a strict charset rather than trusted: a loose name is how a value becomes a second
// variable or an injector reaches something it should not.
var (
	fieldNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envNamePattern      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	extraVarNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	// tokenPattern matches a single {{ field }} reference, with optional surrounding spaces.
	tokenPattern = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)
)

// Field is one input a custom credential type collects.
type Field struct {
	// Name is what an injector references. It is lowercase snake case.
	Name string `json:"name"`
	// Label is the human prompt shown when entering the credential.
	Label string `json:"label,omitempty"`
	// Secret marks a field whose value is masked out of run output. A field left non-secret is
	// treated as configuration, such as a host or a region, and is not masked.
	Secret bool `json:"secret,omitempty"`
}

// CredentialType is an operator-defined credential shape: the fields it collects and how those
// fields are injected into a run.
//
// It is what AWX calls a custom credential type. The built-in kinds each have injection logic
// compiled in; this lets an operator describe a new one over the API without a code change, for a
// provider the built-ins do not cover. The description is data, not code: an injector substitutes a
// field's value into a value literally, and nothing in it is executed, so a type cannot be a way to
// run something on the executor.
type CredentialType struct {
	// ID is the unique type identifier.
	ID string `json:"id"`
	// Name labels the type, for example "Datadog API".
	Name string `json:"name"`
	// Fields are the inputs a credential of this type collects.
	Fields []Field `json:"fields"`
	// EnvInjectors maps an environment variable name to a template. A template is literal text with
	// {{field}} references, so "Bearer {{token}}" becomes the header value with the token spliced
	// in. A value with no reference is a constant.
	EnvInjectors map[string]string `json:"env,omitempty"`
	// ExtraVarInjectors maps an Ansible extra-var name to a template, the same way.
	ExtraVarInjectors map[string]string `json:"extra_vars,omitempty"`
	// CreatedAt is when the type was defined, so a list can be ordered oldest first the same way on
	// every backend.
	CreatedAt time.Time `json:"created_at"`
}

// fieldSet returns the declared field names as a set, for reference checking.
func (t *CredentialType) fieldSet() map[string]bool {
	set := make(map[string]bool, len(t.Fields))
	for _, f := range t.Fields {
		set[f.Name] = true
	}
	return set
}

// Validate reports whether the type is well formed: every field is named legally and once, every
// injector name is legal, and every {{field}} reference resolves to a declared field.
//
// A reference that does not resolve is refused here, at definition time, rather than expanding to an
// empty string at run time. An injector that silently drops a field is how a credential ends up
// authenticating with half of itself.
func (t *CredentialType) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("%w: a type needs a name", ErrBadType)
	}
	if len(t.Fields) == 0 {
		return fmt.Errorf("%w: a type needs at least one field", ErrBadType)
	}
	if len(t.EnvInjectors) == 0 && len(t.ExtraVarInjectors) == 0 {
		return fmt.Errorf("%w: a type that injects nothing does nothing", ErrBadType)
	}
	seen := make(map[string]bool, len(t.Fields))
	for _, f := range t.Fields {
		if !fieldNamePattern.MatchString(f.Name) {
			return fmt.Errorf("%w: field name %q must be lowercase letters, digits, and "+
				"underscores, starting with a letter", ErrBadType, f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("%w: field %q is declared twice", ErrBadType, f.Name)
		}
		seen[f.Name] = true
	}
	fields := t.fieldSet()
	for name, tmpl := range t.EnvInjectors {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("%w: environment variable name %q is not a valid name", ErrBadType, name)
		}
		if err := checkTemplateRefs(tmpl, fields); err != nil {
			return fmt.Errorf("%w: env %s: %w", ErrBadType, name, err)
		}
	}
	for name, tmpl := range t.ExtraVarInjectors {
		if !extraVarNamePattern.MatchString(name) {
			return fmt.Errorf("%w: extra-var name %q is not a valid name", ErrBadType, name)
		}
		if err := checkTemplateRefs(tmpl, fields); err != nil {
			return fmt.Errorf("%w: extra var %s: %w", ErrBadType, name, err)
		}
	}
	return nil
}

// checkTemplateRefs confirms every {{field}} in tmpl names a declared field, and that the template
// itself spans one line.
//
// A newline in the template body is refused as well as one in a field value. An environment file
// writes one entry per line, so a template carrying a line break would emit a second variable even
// before any field is spliced in.
func checkTemplateRefs(tmpl string, fields map[string]bool) error {
	if strings.ContainsAny(tmpl, "\n\r") {
		return fmt.Errorf("the template spans more than one line")
	}
	for _, m := range tokenPattern.FindAllStringSubmatch(tmpl, -1) {
		if !fields[m[1]] {
			return fmt.Errorf("references field %q, which the type does not declare", m[1])
		}
	}
	return nil
}

// SecretFields returns the names of fields marked secret, for masking.
func (t *CredentialType) SecretFields() []string {
	var out []string
	for _, f := range t.Fields {
		if f.Secret {
			out = append(out, f.Name)
		}
	}
	return out
}

// Inject applies the type's injectors to the given field values and returns what to add to a run.
//
// Substitution is a single literal pass: each {{field}} is replaced by that field's value exactly,
// and a field value is never itself re-scanned for references. That is what keeps a value like
// "{{other}}" stored in a field from expanding, and it is why an injector cannot be a route to
// anything beyond string assembly. Every secret field's raw value is returned for masking regardless
// of how it was wrapped, because the masker redacts the substring wherever it lands, so "Bearer
// abc123" is masked to "Bearer ***" from the token alone.
//
// A newline in a value is refused. An environment file writes one entry per line, so a value with a
// line break would become a second variable, which is the injection this validation exists to stop.
func (t *CredentialType) Inject(values map[string]string) (Injection, error) {
	var inj Injection
	for _, f := range t.Fields {
		if strings.ContainsAny(values[f.Name], "\n\r") {
			return Injection{}, fmt.Errorf("%w: field %q spans more than one line", ErrBadType, f.Name)
		}
	}
	subst := func(tmpl string) string {
		return tokenPattern.ReplaceAllStringFunc(tmpl, func(token string) string {
			m := tokenPattern.FindStringSubmatch(token)
			return values[m[1]]
		})
	}
	// Environment lines are emitted in a stable order so a run's environment does not depend on map
	// iteration order.
	for _, name := range sortedKeys(t.EnvInjectors) {
		inj.Env = append(inj.Env, name+"="+subst(t.EnvInjectors[name]))
	}
	if len(t.ExtraVarInjectors) > 0 {
		inj.ExtraVars = make(map[string]string, len(t.ExtraVarInjectors))
		for _, name := range sortedKeys(t.ExtraVarInjectors) {
			inj.ExtraVars[name] = subst(t.ExtraVarInjectors[name])
		}
	}
	for _, f := range t.Fields {
		if f.Secret && values[f.Name] != "" {
			inj.Secrets = append(inj.Secrets, values[f.Name])
		}
	}
	return inj, nil
}

// sortedKeys returns the keys of m in sorted order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
