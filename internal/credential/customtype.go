package credential

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/util"
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
	// fileKeyPattern is the shape of a file injector key: template for the single file, or
	// template.<name> for one of several, the spelling AWX uses.
	fileKeyPattern = regexp.MustCompile(`^template(?:\.([A-Za-z_][A-Za-z0-9_]*))?$`)
	// tokenPattern matches a single {{ reference }}, with optional surrounding spaces. A reference is
	// a field name, or a dotted path such as tower.filename.cert that names a file's written path.
	tokenPattern = regexp.MustCompile(
		`\{\{\s*([a-z][a-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\}\}`)
)

// SingleFile is the key FileInjectors uses for the one unnamed file a type writes, and the name
// RenderFiles and Inject use for it.
const SingleFile = "template"

// OriginAWX is the Origin of a type an AWX import brought across. A type from AWX may arrive
// writing a file no injector references, and an imported type keeps one rather than being turned
// away at the door of a migration, while a run of it says which file nothing points at.
const OriginAWX = "awx"

// maxFileTemplateLen bounds one file injector's template, in bytes, so a type stays small
// configuration rather than a store for blobs.
const maxFileTemplateLen = 64 << 10

// Field is one input a custom credential type collects.
type Field struct {
	// Name is what an injector references. It is lowercase snake case.
	Name string `json:"name"`
	// Label is the human prompt shown when entering the credential.
	Label string `json:"label,omitempty"`
	// Secret marks a field whose value is masked out of run output. A field left non-secret is
	// treated as configuration, such as a host or a region, and is not masked.
	Secret bool `json:"secret,omitempty"`
	// Multiline marks a field whose value spans lines, such as a kubeconfig or a certificate, so an
	// editor offers a text area for it. Only a field no env or extra-var injector references may
	// hold a line break at run time.
	Multiline bool `json:"multiline,omitempty"`
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
	// in. A value with no reference is a constant. A template may also name a written file's path
	// with {{tower.filename}} or {{tower.filename.<name>}}, the form AWX uses, or with awx.filename
	// in place of tower.filename, an alias this package accepts.
	EnvInjectors map[string]string `json:"env,omitempty"`
	// ExtraVarInjectors maps an Ansible extra-var name to a template, the same way.
	ExtraVarInjectors map[string]string `json:"extra_vars,omitempty"`
	// FileInjectors maps a file key to the template of a file written for the run. The key is
	// template for a type that writes one file, or template.<name> for each of several, and a file's
	// written path reaches the tool through an env or extra-var injector that references it. This is
	// how AWX delivers a kubeconfig or a cloud config file.
	FileInjectors map[string]string `json:"file,omitempty"`
	// Origin is where the type was defined: empty for a type created in SwitchTender, OriginAWX for
	// one an AWX import brought across. The server and the importer set it and a request never does.
	// It decides one rule: an imported type may write a file no injector references, and a type
	// created here may not.
	Origin string `json:"origin,omitempty"`
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

// fileNames returns the names of the files the type writes, SingleFile for the unnamed one, as a
// set for reference checking.
func (t *CredentialType) fileNames() map[string]bool {
	set := make(map[string]bool, len(t.FileInjectors))
	for key := range t.FileInjectors {
		if name, ok := fileName(key); ok {
			set[name] = true
		}
	}
	return set
}

// fileName returns the file name a file injector key declares: SingleFile for the bare template key,
// or the part after the dot for template.<name>.
func fileName(key string) (string, bool) {
	m := fileKeyPattern.FindStringSubmatch(key)
	if m == nil {
		return "", false
	}
	if m[1] == "" {
		return SingleFile, true
	}
	return m[1], true
}

// templateRef is one {{ reference }} in a template, resolved to a field or to a file's path.
type templateRef struct {
	// field is the referenced field's name, empty for a file reference.
	field string
	// file is the referenced file's name, SingleFile for the unnamed one, empty for a field.
	file string
}

// parseRef resolves the text inside a {{ }} pair. A bare name is a field. tower.filename, the form
// AWX uses, names the single file, and followed by .<name> names one of several. awx.filename is
// accepted in its place as an alias. Any other dotted path is refused, since nothing else is in
// scope and expanding it to nothing at run time is how a credential authenticates with half of
// itself.
func parseRef(ref string) (templateRef, error) {
	parts := strings.Split(ref, ".")
	if len(parts) == 1 {
		return templateRef{field: ref}, nil
	}
	if (parts[0] != "tower" && parts[0] != "awx") || parts[1] != "filename" || len(parts) > 3 {
		return templateRef{}, fmt.Errorf("references %q, which is neither a field nor a file path "+
			"such as tower.filename", ref)
	}
	if len(parts) == 2 {
		return templateRef{file: SingleFile}, nil
	}
	return templateRef{file: parts[2]}, nil
}

// Validate reports whether the type is well formed: every field is named legally and once, every
// injector name is legal, every {{field}} reference resolves to a declared field, and every file
// the type writes has its path handed to the run by an env or extra-var injector.
//
// A reference that does not resolve is refused here, at definition time, rather than expanding to an
// empty string at run time. An injector that silently drops a field is how a credential ends up
// authenticating with half of itself.
//
// A file nothing references is refused for a type created here and accepted for one imported from
// AWX. Refusing it there would stop a migration over a type that was in use before the move, so the
// import carries it and says so instead.
func (t *CredentialType) Validate() error {
	return t.validate(func(string) bool { return t.Origin == OriginAWX })
}

// ValidateEdit validates t as an edit of prev made here, with t carrying prev's Origin. A type
// imported from AWX keeps the files nothing references that it arrived with, but an edit cannot add
// one, because the edit is made here, where a file nothing references is refused.
func (t *CredentialType) ValidateEdit(prev *CredentialType) error {
	kept := map[string]bool{}
	if prev != nil && prev.Origin == OriginAWX {
		for _, name := range prev.UnreferencedFiles() {
			kept[name] = true
		}
	}
	return t.validate(func(name string) bool { return kept[name] })
}

// validate checks the type, accepting a file nothing references only when keep says to.
func (t *CredentialType) validate(keep func(file string) bool) error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("%w: a type needs a name", ErrBadType)
	}
	if len(t.Fields) == 0 {
		return fmt.Errorf("%w: a type needs at least one field", ErrBadType)
	}
	if len(t.EnvInjectors) == 0 && len(t.ExtraVarInjectors) == 0 && len(t.FileInjectors) == 0 {
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
	if err := t.validateFiles(); err != nil {
		return err
	}
	fields, files := t.fieldSet(), t.fileNames()
	used := make(map[string]bool, len(files))
	for name, tmpl := range t.EnvInjectors {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("%w: environment variable name %q is not a valid name", ErrBadType, name)
		}
		if err := checkTemplateRefs(tmpl, fields, files, used); err != nil {
			return fmt.Errorf("%w: env %s: %w", ErrBadType, name, err)
		}
	}
	for name, tmpl := range t.ExtraVarInjectors {
		if !extraVarNamePattern.MatchString(name) {
			return fmt.Errorf("%w: extra-var name %q is not a valid name", ErrBadType, name)
		}
		if err := checkTemplateRefs(tmpl, fields, files, used); err != nil {
			return fmt.Errorf("%w: extra var %s: %w", ErrBadType, name, err)
		}
	}
	// A file whose path no injector hands over is written and never read, so the tool runs without
	// the credential and nothing says why.
	var lost []string
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !used[name] && !keep(name) {
			lost = append(lost, name)
		}
	}
	if len(lost) > 0 {
		return unreferencedError(lost)
	}
	return nil
}

// unreferencedError explains a type that writes files nothing references: which files, why that is
// refused, and the reference that hands each path to the tool, with an injector to copy.
func unreferencedError(names []string) error {
	what := "file " + names[0] + " is written but no env or extra-var injector references its path"
	if len(names) > 1 {
		what = "files " + util.JoinWords(names, "and") + " are written but no env or extra-var " +
			"injector references their paths"
	}
	refs := make([]string, 0, len(names))
	for _, name := range names {
		refs = append(refs, FileRef(name))
	}
	return fmt.Errorf("%w: %s, so the tool would never be told where to find it. Hand the path "+
		"over with %s in an env or extra-var injector, for example \"env\": {%q: %q}, or delete "+
		"the file injector", ErrBadType, what, strings.Join(refs, ", "),
		fileEnvExample(names[0]), FileRef(names[0]))
}

// fileEnvExample suggests an environment variable name for a file's path in an error's example.
func fileEnvExample(name string) string {
	if name == SingleFile {
		return "CREDENTIAL_FILE"
	}
	return strings.ToUpper(name) + "_FILE"
}

// UnreferencedFiles returns, sorted, the names of the files the type writes whose path no env or
// extra-var injector references, SingleFile for the unnamed one. A type created here never has one,
// since Validate refuses it. A type imported from AWX may: such a file is still written for each
// run, and nothing tells the tool where it is.
func (t *CredentialType) UnreferencedFiles() []string {
	used := map[string]bool{}
	for _, set := range []map[string]string{t.EnvInjectors, t.ExtraVarInjectors} {
		for _, tmpl := range set {
			for _, m := range tokenPattern.FindAllStringSubmatch(tmpl, -1) {
				if ref, err := parseRef(m[1]); err == nil && ref.file != "" {
					used[ref.file] = true
				}
			}
		}
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(t.fileNames())) {
		if !used[name] {
			out = append(out, name)
		}
	}
	return out
}

// validateFiles checks the file injectors: every key is template or template.<name>, the two
// spellings are not mixed, and every template references only declared fields.
//
// Mixing is refused the way AWX refuses it. With a bare template beside named ones,
// {{tower.filename}} would name one file while the others hang off the same name, and the reader of
// the type could not tell which path a variable receives.
func (t *CredentialType) validateFiles() error {
	if len(t.FileInjectors) == 0 {
		return nil
	}
	_, single := t.FileInjectors[SingleFile]
	if single && len(t.FileInjectors) > 1 {
		return fmt.Errorf("%w: a type writes one file under template or several under "+
			"template.<name>, not both", ErrBadType)
	}
	fields := t.fieldSet()
	for _, key := range sortedKeys(t.FileInjectors) {
		if _, ok := fileName(key); !ok {
			return fmt.Errorf("%w: file injector %q must be template or template.<name>, the name "+
				"letters, digits, and underscores", ErrBadType, key)
		}
		tmpl := t.FileInjectors[key]
		if len(tmpl) > maxFileTemplateLen {
			return fmt.Errorf("%w: file %s template exceeds %d bytes", ErrBadType, key,
				maxFileTemplateLen)
		}
		for _, m := range tokenPattern.FindAllStringSubmatch(tmpl, -1) {
			ref, err := parseRef(m[1])
			if err != nil {
				return fmt.Errorf("%w: file %s: %w", ErrBadType, key, err)
			}
			if ref.file != "" {
				return fmt.Errorf("%w: file %s references a file path, and a file's contents can "+
					"only reference fields", ErrBadType, key)
			}
			if !fields[ref.field] {
				return fmt.Errorf("%w: file %s references field %q, which the type does not declare",
					ErrBadType, key, ref.field)
			}
		}
	}
	return nil
}

// checkTemplateRefs confirms every {{reference}} in tmpl names a declared field or file, records the
// files it references in used, and checks that the template itself spans one line.
//
// A newline in the template body is refused as well as one in a field value. An environment file
// writes one entry per line, so a template carrying a line break would emit a second variable even
// before any field is spliced in.
func checkTemplateRefs(tmpl string, fields, files, used map[string]bool) error {
	if strings.ContainsAny(tmpl, "\n\r") {
		return fmt.Errorf("the template spans more than one line")
	}
	for _, m := range tokenPattern.FindAllStringSubmatch(tmpl, -1) {
		ref, err := parseRef(m[1])
		if err != nil {
			return err
		}
		if ref.file != "" {
			if !files[ref.file] {
				return fmt.Errorf("references the path of file %s, which the type does not write",
					ref.file)
			}
			used[ref.file] = true
			continue
		}
		if !fields[ref.field] {
			return fmt.Errorf("references field %q, which the type does not declare", ref.field)
		}
	}
	return nil
}

// FileRef returns the reference an env or extra-var injector uses to hand over the path of the named
// file, in the form AWX uses: {{ tower.filename }} for the single file, SingleFile, and
// {{ tower.filename.<name> }} for one of several. Messages that tell a person how to fix a type quote
// it, so they all spell it the same way.
func FileRef(name string) string {
	if name == SingleFile {
		return "{{ tower.filename }}"
	}
	return "{{ tower.filename." + name + " }}"
}

// ValidFieldName reports whether name can name a custom type's field: lowercase letters, digits, and
// underscores, starting with a letter.
func ValidFieldName(name string) bool { return fieldNamePattern.MatchString(name) }

// PlainTemplate reports whether every {{ }} pair in tmpl is a plain reference this package
// substitutes, a field name or a file path, and the text carries no other template syntax. AWX
// injectors are Jinja, and a filter, a conditional, or a comment would reach a run here as literal
// text, so an importer refuses a template that is not plain rather than carrying it across wrong.
func PlainTemplate(tmpl string) bool {
	if strings.Contains(tmpl, "{%") || strings.Contains(tmpl, "{#") {
		return false
	}
	return strings.Count(tmpl, "{{") == len(tokenPattern.FindAllStringIndex(tmpl, -1))
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

// lineFields returns the fields an env or extra-var injector references. Their values are spliced
// into one-line entries, so a line break in one of them is refused at injection.
func (t *CredentialType) lineFields() map[string]bool {
	out := map[string]bool{}
	for _, set := range []map[string]string{t.EnvInjectors, t.ExtraVarInjectors} {
		for _, tmpl := range set {
			for _, m := range tokenPattern.FindAllStringSubmatch(tmpl, -1) {
				if !strings.Contains(m[1], ".") {
					out[m[1]] = true
				}
			}
		}
	}
	return out
}

// substitute replaces every {{field}} in tmpl with that field's value, and every file reference with
// that file's path, in a single literal pass. A value is never itself re-scanned for references.
func substitute(tmpl string, values, paths map[string]string) string {
	return tokenPattern.ReplaceAllStringFunc(tmpl, func(token string) string {
		m := tokenPattern.FindStringSubmatch(token)
		ref, err := parseRef(m[1])
		if err != nil {
			return token
		}
		if ref.file != "" {
			return paths[ref.file]
		}
		return values[ref.field]
	})
}

// RenderFiles returns the contents of every file the type writes, keyed by file name, SingleFile for
// the unnamed one. It performs no I/O: the caller writes each file to a private location and hands
// the paths to Inject.
//
// The substitution is the same single literal pass Inject uses, so a field value that looks like a
// reference stays text. A file may carry a field whose value spans lines, which is the point of a
// file: a kubeconfig or a certificate is several lines long.
func (t *CredentialType) RenderFiles(values map[string]string) (map[string]string, error) {
	if len(t.FileInjectors) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(t.FileInjectors))
	for key, tmpl := range t.FileInjectors {
		name, ok := fileName(key)
		if !ok {
			return nil, fmt.Errorf("%w: file injector %q must be template or template.<name>",
				ErrBadType, key)
		}
		out[name] = substitute(tmpl, values, nil)
	}
	return out, nil
}

// Inject applies the type's injectors to the given field values and returns what to add to a run.
// paths maps each file the type writes, by the names RenderFiles returned, to where it was written,
// and a template that references a file receives that path.
//
// Substitution is a single literal pass: each {{field}} is replaced by that field's value exactly,
// and a field value is never itself re-scanned for references. That is what keeps a value like
// "{{other}}" stored in a field from expanding, and it is why an injector cannot be a route to
// anything beyond string assembly. Every secret field's raw value is returned for masking regardless
// of how it was wrapped, because the masker redacts the substring wherever it lands, so "Bearer
// abc123" is masked to "Bearer ***" from the token alone.
//
// A newline in a value an env or extra-var injector references is refused. An environment file
// writes one entry per line, so a value with a line break would become a second variable, which is
// the injection this validation exists to stop. A field only a file references may span lines.
func (t *CredentialType) Inject(values, paths map[string]string) (Injection, error) {
	var inj Injection
	lines := t.lineFields()
	for _, f := range t.Fields {
		if lines[f.Name] && strings.ContainsAny(values[f.Name], "\n\r") {
			return Injection{}, fmt.Errorf("%w: field %q spans more than one line", ErrBadType, f.Name)
		}
	}
	for name := range t.fileNames() {
		p, ok := paths[name]
		if !ok || p == "" {
			return Injection{}, fmt.Errorf("%w: file %s was not written before injection",
				ErrBadType, name)
		}
		if strings.ContainsAny(p, "\n\r") {
			return Injection{}, fmt.Errorf("%w: the path of file %s spans more than one line",
				ErrBadType, name)
		}
	}
	// Environment lines are emitted in a stable order so a run's environment does not depend on map
	// iteration order.
	for _, name := range sortedKeys(t.EnvInjectors) {
		inj.Env = append(inj.Env, name+"="+substitute(t.EnvInjectors[name], values, paths))
	}
	if len(t.ExtraVarInjectors) > 0 {
		inj.ExtraVars = make(map[string]string, len(t.ExtraVarInjectors))
		for _, name := range sortedKeys(t.ExtraVarInjectors) {
			inj.ExtraVars[name] = substitute(t.ExtraVarInjectors[name], values, paths)
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
