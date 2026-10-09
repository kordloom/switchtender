package importer

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
)

// awxCredentialType is an AWX custom credential type: the inputs a credential of it collects and the
// injectors that turn those inputs into files, environment variables, and extra vars.
type awxCredentialType struct {
	// Name is the type's name, which a credential's credential_type reference names.
	Name string `json:"name"`
	// Description is the type's free text description. It has no effect on a run.
	Description string `json:"description"`
	// Kind is AWX's category for the type, cloud or net for a custom one. It has no effect on a run.
	Kind string `json:"kind"`
	// Managed marks one of AWX's own built-in types. Credentials of those map onto the built-in kinds
	// here, so a managed type is not imported as a custom one.
	Managed bool `json:"managed"`
	// Inputs are the fields the type collects.
	Inputs awxCredTypeInputs `json:"inputs"`
	// Injectors are how the fields reach a run.
	Injectors awxCredTypeInjectors `json:"injectors"`
}

// awxCredTypeInputs is the inputs block of an AWX credential type.
type awxCredTypeInputs struct {
	// Fields are the inputs a credential of the type collects.
	Fields []awxCredTypeField `json:"fields"`
	// Required names the fields AWX makes a person fill. Every field here is entered when the
	// credential's values are set, so the list changes nothing about how the type injects.
	Required []string `json:"required"`
}

// awxCredTypeField is one input of an AWX credential type.
type awxCredTypeField struct {
	// ID is the name an injector references.
	ID string `json:"id"`
	// Label is the prompt shown when entering the value.
	Label string `json:"label"`
	// Type is string or boolean. Empty means string.
	Type string `json:"type"`
	// Secret marks a value AWX encrypts and masks.
	Secret bool `json:"secret"`
	// Multiline marks a value that spans lines.
	Multiline bool `json:"multiline"`
	// HelpText is the hint shown beside the input. It has no effect on a run.
	HelpText string `json:"help_text"`
	// Format is a validation hint such as ssh_private_key. It has no effect on a run.
	Format string `json:"format"`
	// Choices restricts the value AWX's form accepts. It has no effect on a run.
	Choices []any `json:"choices"`
	// Default is the value AWX's form starts from.
	Default any `json:"default"`
	// AskAtRuntime makes AWX ask for the value at launch rather than store it.
	AskAtRuntime bool `json:"ask_at_runtime"`
}

// awxCredTypeInjectors is the injectors block of an AWX credential type. The values are decoded
// loosely so a template that is not a string is refused with a sentence rather than failing the
// whole document.
type awxCredTypeInjectors struct {
	// File maps template, or template.<name> for several files, to a file's template.
	File map[string]any `json:"file"`
	// Env maps an environment variable to a template.
	Env map[string]any `json:"env"`
	// ExtraVars maps an extra var to a template.
	ExtraVars map[string]any `json:"extra_vars"`
}

// addCredentialTypes imports an export's custom credential types and returns them by AWX name, so
// the credentials that name one become credentials of the imported type. A type that cannot come
// across is reported and left out, and credentials of it fall back to the kind mapping. Those types
// are returned too, by name, each with a summary of the injectors its credentials lose, so the
// fallback can be said beside each credential rather than only beside the type.
func (p *Plan) addCredentialTypes(types []awxCredentialType,
	now time.Time) (byName map[string]*credential.CredentialType, refused map[string]string) {
	byName = map[string]*credential.CredentialType{}
	refused = map[string]string{}
	for _, ct := range types {
		if ct.Managed {
			continue
		}
		typ, notes, err := convertAWXCredType(ct, now)
		if err != nil {
			p.warn("credential type %q was not imported: %v. Credentials of this type are mapped "+
				"to a built-in kind by the type's name instead", ct.Name, err)
			p.refused++
			refused[ct.Name] = injectorSummary(ct.Injectors)
			continue
		}
		if _, dup := byName[ct.Name]; dup {
			p.warn("credential type %q appears more than once; the later one is what its "+
				"credentials will use", ct.Name)
			p.CredentialTypes = slices.DeleteFunc(p.CredentialTypes,
				func(t *credential.CredentialType) bool { return t.Name == ct.Name })
		}
		byName[ct.Name] = typ
		p.CredentialTypes = append(p.CredentialTypes, typ)
		for _, n := range notes {
			p.warn("credential type %q: %s", ct.Name, n)
		}
	}
	return byName, refused
}

// injectorSummary names what a type's injectors wrote in AWX, block by block, so a credential of a
// type that did not come across can say what its runs do not receive here.
func injectorSummary(in awxCredTypeInjectors) string {
	var parts []string
	for _, block := range []struct {
		// name is the injector block, as AWX names it.
		name string
		// templates are the block's injectors, keyed by what each writes.
		templates map[string]any
	}{{"env", in.Env}, {"file", in.File}, {"extra_vars", in.ExtraVars}} {
		if len(block.templates) == 0 {
			continue
		}
		names := slices.Sorted(maps.Keys(block.templates))
		parts = append(parts, block.name+" "+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " and ")
}

// convertAWXCredType maps one AWX custom credential type onto a SwitchTender one, returning notes a
// person should read and an error naming why a type cannot come across.
//
// A type moves only when it would inject exactly what it injects in AWX. The injectors are Jinja
// there and plain substitution here, so a template with a filter, a conditional, or any expression
// beyond a field or a file path is refused rather than carried as literal text a run would receive.
//
// The type is marked as imported from AWX, which keeps a file no injector references, since AWX
// accepts one. credTypeNotes says what a person should know about such a file, and about a type
// shaped like a kubeconfig. Neither is changed on the way in.
func convertAWXCredType(ct awxCredentialType, now time.Time) (*credential.CredentialType, []string, error) {
	typ := &credential.CredentialType{
		ID: credential.NewTypeID(), Name: ct.Name, Origin: credential.OriginAWX, CreatedAt: now,
	}
	var notes []string
	for _, f := range ct.Inputs.Fields {
		if !credential.ValidFieldName(f.ID) {
			return nil, nil, fmt.Errorf("field %q is not lowercase letters, digits, and underscores "+
				"starting with a letter, which is what an injector names here", f.ID)
		}
		switch f.Type {
		case "", "string":
		case "boolean":
			notes = append(notes, fmt.Sprintf("field %q is a boolean in AWX, which renders as True "+
				"or False, so enter it here as that text", f.ID))
		default:
			return nil, nil, fmt.Errorf("field %q has type %q, and only string and boolean fields "+
				"have an equivalent", f.ID, f.Type)
		}
		label := f.Label
		if label == "" {
			label = f.ID
		}
		typ.Fields = append(typ.Fields, credential.Field{
			Name: f.ID, Label: label, Secret: f.Secret, Multiline: f.Multiline,
		})
		if f.AskAtRuntime {
			notes = append(notes, fmt.Sprintf("field %q is asked for at launch in AWX. Here a value "+
				"is stored on the credential, so set it there", f.ID))
		}
		if def := jsonScalarString(f.Default); def != "" && !f.Secret {
			notes = append(notes, fmt.Sprintf("field %q defaults to %q in AWX. Enter that value "+
				"when setting the credential's fields, since an empty field injects nothing", f.ID, def))
		}
	}
	var err error
	if typ.FileInjectors, err = plainTemplates("file", ct.Injectors.File); err != nil {
		return nil, nil, err
	}
	if typ.EnvInjectors, err = plainTemplates("env", ct.Injectors.Env); err != nil {
		return nil, nil, err
	}
	if typ.ExtraVarInjectors, err = plainTemplates("extra_vars", ct.Injectors.ExtraVars); err != nil {
		return nil, nil, err
	}
	if err := typ.Validate(); err != nil {
		return nil, nil, err
	}
	return typ, append(notes, credTypeNotes(typ)...), nil
}

// plainTemplates converts one injector block, refusing a value that is not a string or a template
// that is not plain substitution. An empty block converts to nil.
func plainTemplates(block string, in map[string]any) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tmpl, ok := in[k].(string)
		if !ok {
			return nil, fmt.Errorf("%s injector %q is not a string template", block, k)
		}
		if !credential.PlainTemplate(tmpl) {
			return nil, fmt.Errorf("%s injector %q uses Jinja beyond a field or a file path, "+
				"such as a filter or a condition, which has no equivalent", block, k)
		}
		out[k] = tmpl
	}
	return out, nil
}

// typedCredentialWarning tells a person what a credential of an imported custom type needs before a
// run can use it: every field's value, set in place.
func typedCredentialWarning(name string, typ *credential.CredentialType) string {
	names := make([]string, 0, len(typ.Fields))
	for _, f := range typ.Fields {
		names = append(names, f.Name)
	}
	return fmt.Sprintf("credential %q is of the custom type %q and needs its field values entered "+
		"(%s): an export never carries secret values, and a typed credential keeps all of its "+
		"fields sealed together. Set them with PUT /v1/credentials/{id} and a fields object",
		name, typ.Name, strings.Join(names, ", "))
}
