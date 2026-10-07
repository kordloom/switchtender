package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/kordloom/switchtender/internal/run"
)

// RegoInputVersion is the version of the input document a Rego policy is evaluated against. It
// changes only when a field is removed or changes meaning, so a module written against version 1
// keeps reading the same document across releases.
const RegoInputVersion = 1

// Rego syntaxes a policy file may name for a module.
const (
	// RegoSyntaxV1 is the Rego syntax OPA 1.0 made the default: rule bodies after if, partial sets
	// declared with contains. It is what a module is parsed as when the file names no syntax.
	RegoSyntaxV1 = "v1"
	// RegoSyntaxV0 is the syntax OPA used before 1.0, which most Conftest policies in the wild are
	// written in. Accepting it is what lets an existing rule port without being rewritten first.
	RegoSyntaxV0 = "v0"
)

// Decision rule names a Rego policy's package may define.
const (
	// regoRuleDeny is a set of messages, or a boolean, that refuses the submission outright.
	regoRuleDeny = "deny"
	// regoRuleHold is a set of messages, or a boolean, that holds the run for approval.
	regoRuleHold = "hold"
	// regoRuleWarn is Conftest's advisory rule. By default it is read as hold: a warning asks a
	// person to look, and at this boundary the way a person looks is by approving. A policy that
	// sets warn to note records its warnings on the run instead.
	regoRuleWarn = "warn"
	// regoRuleAllow is the OPA allowlist convention: anything it does not allow is refused.
	regoRuleAllow = "allow"
	// regoRuleDistinct demands that whoever approves the run is not whoever asked for it.
	regoRuleDistinct = "require_distinct_approver"
	// regoRulePlanGate sends a Terraform or OpenTofu apply through a plan first, so the apply is
	// evaluated again with input.plan filled in.
	regoRulePlanGate = "plan_gate"
)

// What a Rego policy's warn rule does, as its warn setting names it.
const (
	// RegoWarnHold reads a warning as a hold, so the run waits for a person. It is the default.
	RegoWarnHold = "hold"
	// RegoWarnNote records a warning on the run, where the run page, the outcome record, and a
	// receipt show it, and lets the run go ahead unless another rule holds or refuses it.
	RegoWarnNote = "note"
)

// How long one evaluation of a Rego policy may run.
const (
	// DefaultRegoTimeout bounds one evaluation of a policy that sets no timeout. A policy decides in
	// well under a millisecond. One that runs for half a second is doing something no gate should,
	// and a submission waiting on it is a person waiting on it.
	DefaultRegoTimeout = 500 * time.Millisecond
	// MaxRegoTimeout is the longest timeout a policy may set. Each question the dispatcher asks a
	// policy is an evaluation of its own, so the limit can be paid several times over before a
	// submission answers.
	MaxRegoTimeout = 10 * time.Second
)

// maxRegoReason bounds how much of one message a decision carries, since the messages are written
// into the run record and a module can build a string of any size.
const maxRegoReason = 200

// maxNoteReasons bounds how many of one policy's messages a note carries. A note is written on
// every run the policy warns about, held or not, and committed with that run's outcome, so a module
// emitting a message per variable name would otherwise grow every record it touches.
const maxNoteReasons = 10

// regoDisabledBuiltins are removed from what a module may call, beyond every builtin OPA itself marks
// nondeterministic. Each reaches outside the process or reads something other than the input: the
// network, the environment, the clock, or a random source. A module that calls one fails to compile,
// so it is refused at load rather than evaluated with a different answer on every call.
var regoDisabledBuiltins = []string{
	"http.send",
	"net.lookup_ip_addr",
	"opa.runtime",
	"time.now_ns",
	"rand.intn",
	"uuid.rfc4122",
	"io.jwt.decode_verify",
	"crypto.x509.parse_and_verify_certificates",
	"crypto.x509.parse_and_verify_certificates_with_options",
	"providers.aws.sign_req",
	"trace",
}

// RegoModule is one Rego source file a Rego policy compiles.
type RegoModule struct {
	// File is the path the policy file names it by, relative to the policy file. It is part of what
	// the bundle digest covers, so moving a file is visible in the record.
	File string `json:"file"`
	// Source is the module text exactly as read.
	Source string `json:"source"`
}

// RegoProgram is a compiled Rego policy: one or more modules, the package whose rules decide, and
// the digest that names this exact bundle in the evidence. It is immutable once compiled, so copies
// of a Policy may share it.
type RegoProgram struct {
	// pkg is the decision package's path, for example data.switchtender.
	pkg string
	// syntax is the Rego syntax the modules were parsed as.
	syntax string
	// modules are the sources compiled, sorted by file.
	modules []RegoModule
	// digest is the hex SHA-256 over the canonical bundle.
	digest string
	// rules are the decision rules the package defines, by name.
	rules map[string]bool
	// reads are the top-level input fields the modules reference, for example plan.
	reads map[string]bool
	// query is the prepared evaluation of the decision package.
	query rego.PreparedEvalQuery
	// timeout bounds one evaluation.
	timeout time.Duration
	// warn is what the warn rule does: RegoWarnHold or RegoWarnNote.
	warn string
}

// regoSettings are what a Rego policy sets beside its modules in the policy file. They change what
// the bundle does to a run without changing a byte of it, so they are not part of its digest.
type regoSettings struct {
	// warn is what the warn rule does.
	warn string
	// timeout bounds one evaluation.
	timeout time.Duration
}

// RegoOption sets one of a Rego policy's settings as it is compiled.
type RegoOption func(*regoSettings)

// WithRegoWarn sets what the program's warn rule does: RegoWarnHold, the default, or RegoWarnNote.
// Empty means the default.
func WithRegoWarn(mode string) RegoOption {
	return func(s *regoSettings) { s.warn = mode }
}

// WithRegoTimeout bounds each evaluation of the program. It must be more than zero and at most
// MaxRegoTimeout.
func WithRegoTimeout(limit time.Duration) RegoOption {
	return func(s *regoSettings) { s.timeout = limit }
}

// check refuses a setting this build cannot honor, at load, where a mistake is cheap to name. An
// empty warn mode is the default.
func (s *regoSettings) check() error {
	switch s.warn {
	case "":
		s.warn = RegoWarnHold
	case RegoWarnHold, RegoWarnNote:
	default:
		return fmt.Errorf("%w: warn must be %q or %q, not %q", ErrRego, RegoWarnHold, RegoWarnNote,
			s.warn)
	}
	if s.timeout <= 0 || s.timeout > MaxRegoTimeout {
		return fmt.Errorf("%w: timeout must be more than zero and at most %s, not %s", ErrRego,
			MaxRegoTimeout, s.timeout)
	}
	return nil
}

// RegoDecision is what a Rego policy decided about one run, in the vocabulary the YAML rules use.
type RegoDecision struct {
	// Deny holds the reasons the submission is refused, empty when nothing refused it.
	Deny []string
	// Hold holds the reasons the run waits for approval, empty when nothing held it.
	Hold []string
	// Note holds the warnings a policy set to warn: note recorded instead of holding the run.
	Note []string
	// RequireDistinctApprover demands an approver other than the requester.
	RequireDistinctApprover bool
	// PlanGate sends a Terraform or OpenTofu apply through a plan before it applies.
	PlanGate bool
	// Err is why the policy could not decide. A decision with an error is a refusal, never a pass.
	Err error
}

// regoCapabilities returns the builtins a module may call for the given syntax: everything OPA
// ships except what reaches outside the process or answers differently on each call, with no
// network host allowed at all.
func regoCapabilities(syntax string) *ast.Capabilities {
	version := ast.RegoV1
	if syntax == RegoSyntaxV0 {
		version = ast.RegoV0
	}
	caps := ast.CapabilitiesForThisVersion(ast.CapabilitiesRegoVersion(version))
	kept := caps.Builtins[:0:0]
	for _, b := range caps.Builtins {
		if b.Nondeterministic || slices.Contains(regoDisabledBuiltins, b.Name) {
			continue
		}
		kept = append(kept, b)
	}
	caps.Builtins = kept
	caps.AllowNet = []string{}
	return caps
}

// CompileRego compiles modules into a program deciding from the named package. An empty pkg means
// the package the first module declares. syntax is v1 or v0, empty meaning v1. The options set what
// the warn rule does and how long one evaluation may run. Without them a warning holds the run and
// an evaluation may run for DefaultRegoTimeout.
//
// Everything that can be found wrong before a run arrives is found here, so a policy file with a
// mistake in it is refused at load the way a malformed YAML rule is, rather than loaded and quietly
// deciding nothing: a parse or compile error, a call to a builtin that reaches outside the process, a
// package that defines no decision rule, a reference to an input field the document does not have,
// a module that reads input.plan without ever asking for a plan, a setting outside what this build
// honors, and warn set to note on a package with no warn rule for it to change.
func CompileRego(pkg, syntax string, modules []RegoModule, opts ...RegoOption) (*RegoProgram, error) {
	settings := regoSettings{warn: RegoWarnHold, timeout: DefaultRegoTimeout}
	for _, opt := range opts {
		opt(&settings)
	}
	if err := settings.check(); err != nil {
		return nil, err
	}
	switch syntax {
	case "":
		syntax = RegoSyntaxV1
	case RegoSyntaxV1, RegoSyntaxV0:
	default:
		return nil, fmt.Errorf("%w: syntax must be %q or %q, not %q",
			ErrRego, RegoSyntaxV1, RegoSyntaxV0, syntax)
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("%w: no module to compile", ErrRego)
	}
	sorted := slices.Clone(modules)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].File < sorted[j].File })
	version := ast.RegoV1
	if syntax == RegoSyntaxV0 {
		version = ast.RegoV0
	}
	caps := regoCapabilities(syntax)

	parsed := make(map[string]*ast.Module, len(modules))
	for i, m := range modules {
		if _, dup := parsed[m.File]; dup {
			return nil, fmt.Errorf("%w: %s is named twice", ErrRego, m.File)
		}
		mod, err := ast.ParseModuleWithOpts(m.File, m.Source,
			ast.ParserOptions{RegoVersion: version, Capabilities: caps})
		if err != nil {
			return nil, fmt.Errorf("%w: parse %s: %w", ErrRego, m.File, err)
		}
		parsed[m.File] = mod
		if pkg == "" && i == 0 {
			pkg = mod.Package.Path.String()
		}
	}
	if !strings.HasPrefix(pkg, "data.") {
		pkg = "data." + pkg
	}

	compiler := ast.NewCompiler().WithCapabilities(caps).WithEnablePrintStatements(false)
	compiler.Compile(parsed)
	if compiler.Failed() {
		return nil, fmt.Errorf("%w: compile: %w", ErrRego, compiler.Errors)
	}

	rules := map[string]bool{}
	reads := map[string]bool{}
	for _, file := range sortedKeys(parsed) {
		mod := parsed[file]
		if err := checkInputRefs(file, mod); err != nil {
			return nil, err
		}
		for _, field := range inputFieldsRead(mod) {
			reads[field] = true
		}
		if mod.Package.Path.String() != pkg {
			continue
		}
		for _, rule := range mod.Rules {
			rules[rule.Head.Ref()[0].String()] = true
		}
	}
	if !rules[regoRuleDeny] && !rules[regoRuleHold] && !rules[regoRuleWarn] && !rules[regoRuleAllow] {
		return nil, fmt.Errorf("%w: package %s defines none of deny, hold, warn, or allow, so it "+
			"would decide nothing about any run", ErrRego, pkg)
	}
	if reads["plan"] && !rules[regoRulePlanGate] {
		return nil, fmt.Errorf("%w: package %s reads input.plan but defines no plan_gate rule, so "+
			"no apply would ever be planned for it to read and the rule could never fire",
			ErrRego, pkg)
	}
	// The setting softens warn alone. Set on a package that has no warn rule it would read as
	// softening the hold or deny rules that are there while changing nothing, the same shape as a
	// criterion beside a Rego policy, which is refused for the same reason.
	if settings.warn == RegoWarnNote && !rules[regoRuleWarn] {
		return nil, fmt.Errorf("%w: warn is %q but package %s defines no warn rule, so the setting "+
			"would change nothing: it records warnings, and a hold or deny rule still holds or "+
			"refuses", ErrRego, RegoWarnNote, pkg)
	}

	query, err := rego.New(
		rego.Compiler(compiler),
		rego.Query(pkg),
		rego.Capabilities(caps),
		rego.StrictBuiltinErrors(true),
		rego.EnablePrintStatements(false),
	).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("%w: prepare %s: %w", ErrRego, pkg, err)
	}
	return &RegoProgram{
		pkg: pkg, syntax: syntax, modules: sorted, digest: regoDigest(pkg, syntax, sorted),
		rules: rules, reads: reads, query: query, timeout: settings.timeout, warn: settings.warn,
	}, nil
}

// regoDigest returns the hex SHA-256 over the canonical bundle: the package, the syntax, and every
// module's path and text, sorted by path.
func regoDigest(pkg, syntax string, modules []RegoModule) string {
	raw, err := json.Marshal(struct {
		Package string       `json:"package"`
		Syntax  string       `json:"syntax"`
		Modules []RegoModule `json:"modules"`
	}{Package: pkg, Syntax: syntax, Modules: modules})
	if err != nil {
		// The struct holds only strings, which always encode. Hashing the error text still yields a
		// digest that differs from any real bundle rather than an empty one.
		raw = []byte(err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Digest returns the hex SHA-256 naming this exact bundle, the value the evidence records.
func (p *RegoProgram) Digest() string { return p.digest }

// Package returns the path of the package whose rules decide, for example data.switchtender.
func (p *RegoProgram) Package() string { return p.pkg }

// Reads reports whether the modules reference the named top-level input field, such as plan or
// reversibility.
func (p *RegoProgram) Reads(field string) bool { return p.reads[field] }

// Warn returns what the program's warn rule does: RegoWarnHold or RegoWarnNote.
func (p *RegoProgram) Warn() string { return p.warn }

// Timeout returns how long one evaluation of the program may run.
func (p *RegoProgram) Timeout() time.Duration { return p.timeout }

// regoProgramJSON is how a program travels: to a relay worker, which compiles it again, and to the
// API, which shows what is in force.
type regoProgramJSON struct {
	// Package is the decision package's path.
	Package string `json:"package"`
	// Syntax is the Rego syntax the modules are parsed as.
	Syntax string `json:"syntax"`
	// SHA256 is the bundle digest, checked against the modules on the way back in.
	SHA256 string `json:"sha256"`
	// Modules are the sources.
	Modules []RegoModule `json:"modules"`
	// Warn is what the warn rule does, hold or note. Empty, from a control node older than the
	// setting, is hold, which is all such a node does.
	Warn string `json:"warn"`
	// Timeout bounds one evaluation, written as a duration such as 500ms. Empty is the default.
	Timeout string `json:"timeout"`
}

// MarshalJSON encodes the program as its sources, its digest, and its settings.
func (p *RegoProgram) MarshalJSON() ([]byte, error) {
	return json.Marshal(regoProgramJSON{
		Package: p.pkg, Syntax: p.syntax, SHA256: p.digest, Modules: p.modules,
		Warn: p.warn, Timeout: p.timeout.String(),
	})
}

// UnmarshalJSON compiles the program the sources describe, with the settings they arrived under,
// and refuses one whose sources do not produce the digest it claims. A relay worker reads the control
// node's policies this way, and a program that cannot be rebuilt here is an error the worker fails
// closed on, never an empty rule.
func (p *RegoProgram) UnmarshalJSON(raw []byte) error {
	var in regoProgramJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("%w: decode: %w", ErrRego, err)
	}
	opts := []RegoOption{WithRegoWarn(in.Warn)}
	if in.Timeout != "" {
		limit, err := time.ParseDuration(in.Timeout)
		if err != nil {
			return fmt.Errorf("%w: timeout %q is not a duration: %w", ErrRego, in.Timeout, err)
		}
		opts = append(opts, WithRegoTimeout(limit))
	}
	prog, err := CompileRego(in.Package, in.Syntax, in.Modules, opts...)
	if err != nil {
		return err
	}
	if prog.digest != in.SHA256 {
		return fmt.Errorf("%w: the modules digest to %s, not the %s they arrived under",
			ErrRego, prog.digest, in.SHA256)
	}
	*p = *prog
	return nil
}

// Decide evaluates the program against r and reports what it decided.
//
// It never fails open. A run it cannot build an input for, an evaluation error, a timeout, a
// package that evaluates to nothing, and a decision rule of the wrong type all come back as a
// decision carrying Err, which every caller reads as a refusal.
//
// A dry run the gate's scan did not find change free, a playbook that forces real tasks under check
// mode or a configuration that runs a program while it plans or could not be read in full, is
// judged twice: as the input says it is, and as the real run it partly is, with run.dry_run false.
// The stricter answer stands, so a module written the way a YAML exclude_dry_run rule reads,
// exempting input.run.dry_run, holds such a dry run exactly where the YAML engine does, and a module
// that reads change_free or dry_run_findings decides on them as well. A policy whose warnings are
// notes notes what either pass warned about, for the same reason.
func (p *RegoProgram) Decide(r *run.Run) RegoDecision {
	d := p.decide(r)
	if r == nil || !r.DryRun || r.ChangeFree() || d.Err != nil {
		return d
	}
	asReal := *r
	asReal.DryRun = false
	return stricter(d, p.decide(&asReal))
}

// stricter combines two decisions about one run, keeping every refusal, hold, and demand either
// made. An error in either is the answer, since a decision that could not be made is a refusal.
func stricter(a, b RegoDecision) RegoDecision {
	if a.Err != nil {
		return a
	}
	if b.Err != nil {
		return b
	}
	return RegoDecision{
		Deny:                    mergeReasons(a.Deny, b.Deny),
		Hold:                    mergeReasons(a.Hold, b.Hold),
		Note:                    mergeReasons(a.Note, b.Note),
		RequireDistinctApprover: a.RequireDistinctApprover || b.RequireDistinctApprover,
		PlanGate:                a.PlanGate || b.PlanGate,
	}
}

// mergeReasons returns the reasons in a followed by those in b that a does not already hold.
func mergeReasons(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// decide evaluates the program against r once, with the input r describes.
func (p *RegoProgram) decide(r *run.Run) RegoDecision {
	if r == nil {
		return RegoDecision{Err: fmt.Errorf("%w: no run to evaluate", ErrRego)}
	}
	// A program that was never compiled has no query to run. Only CompileRego and UnmarshalJSON
	// build one, so this is a program assembled by hand, and it decides nothing but a refusal.
	if p == nil || p.digest == "" {
		return RegoDecision{Err: fmt.Errorf("%w: the program was never compiled", ErrRego)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	rs, err := p.query.Eval(ctx, rego.EvalInput(RegoInput(r)))
	if err != nil {
		if ctx.Err() != nil {
			return RegoDecision{Err: p.timedOut()}
		}
		return RegoDecision{Err: fmt.Errorf("%w: evaluation failed: %s", ErrRego,
			clip(err.Error()))}
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return RegoDecision{Err: fmt.Errorf("%w: package %s is undefined for this run", ErrRego, p.pkg)}
	}
	doc, ok := rs[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return RegoDecision{Err: fmt.Errorf("%w: package %s did not evaluate to a document",
			ErrRego, p.pkg)}
	}
	return p.decision(doc)
}

// timedOut is the refusal an evaluation that ran past its timeout comes back as. It names the
// limit, so the person whose submission was refused, and the operator reading the refusal, can tell
// a slow policy from a broken one and know which setting moves it.
func (p *RegoProgram) timedOut() error {
	if p.timeout < MaxRegoTimeout {
		return fmt.Errorf("%w: evaluation exceeded its %s timeout, which the policy's timeout "+
			"setting can raise to at most %s", ErrRego, p.timeout, MaxRegoTimeout)
	}
	return fmt.Errorf("%w: evaluation exceeded its %s timeout, the most a policy may set",
		ErrRego, p.timeout)
}

// decision reads the decision rules out of the evaluated package.
func (p *RegoProgram) decision(doc map[string]any) RegoDecision {
	var d RegoDecision
	var err error
	if d.Deny, err = regoReasons(regoRuleDeny, doc[regoRuleDeny], "denied"); err != nil {
		return RegoDecision{Err: err}
	}
	if p.rules[regoRuleAllow] {
		allowed, err := regoBool(regoRuleAllow, doc[regoRuleAllow])
		if err != nil {
			return RegoDecision{Err: err}
		}
		if !allowed {
			d.Deny = append(d.Deny, "not allowed")
		}
	}
	if d.Hold, err = regoReasons(regoRuleHold, doc[regoRuleHold], "held"); err != nil {
		return RegoDecision{Err: err}
	}
	warn, err := regoReasons(regoRuleWarn, doc[regoRuleWarn], "warned")
	if err != nil {
		return RegoDecision{Err: err}
	}
	// Only a program compiled to note its warnings notes them. Anything else holds, so a program
	// whose setting was somehow lost reads its warnings in the stricter direction.
	if p.warn == RegoWarnNote {
		d.Note = warn
	} else {
		d.Hold = append(d.Hold, warn...)
	}
	if d.RequireDistinctApprover, err = regoBool(regoRuleDistinct, doc[regoRuleDistinct]); err != nil {
		return RegoDecision{Err: err}
	}
	if d.PlanGate, err = regoBool(regoRulePlanGate, doc[regoRulePlanGate]); err != nil {
		return RegoDecision{Err: err}
	}
	return d
}

// regoReasons reads a deny, hold, or warn rule as a list of messages. The rule may be a set or
// array of messages, the Conftest convention, a single message, or a boolean, where true carries
// fallback as its reason. An undefined rule decides nothing. Any other shape is an error, because a
// rule whose meaning cannot be read must not be read as having said nothing.
func regoReasons(rule string, v any, fallback string) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		if x {
			return []string{fallback}, nil
		}
		return nil, nil
	case string:
		return []string{clip(x)}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, clip(regoMessage(item)))
		}
		sort.Strings(out)
		return out, nil
	default:
		return nil, fmt.Errorf("%w: rule %s is a %T, not a set of messages or a boolean",
			ErrRego, rule, v)
	}
}

// regoMessage renders one element of a message set: a string as itself, a Conftest result object by
// its msg field, anything else as JSON.
func regoMessage(item any) string {
	if s, ok := item.(string); ok {
		return s
	}
	if obj, ok := item.(map[string]any); ok {
		if msg, ok := obj["msg"].(string); ok {
			return msg
		}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return fmt.Sprint(item)
	}
	return string(raw)
}

// regoBool reads a boolean rule. Undefined is false, the ordinary Rego reading of a rule whose body
// did not hold, and anything that is not a boolean is an error.
func regoBool(rule string, v any) (bool, error) {
	switch x := v.(type) {
	case nil:
		return false, nil
	case bool:
		return x, nil
	default:
		return false, fmt.Errorf("%w: rule %s is a %T, not a boolean", ErrRego, rule, v)
	}
}

// clip bounds a message to maxRegoReason characters.
func clip(s string) string {
	if len(s) <= maxRegoReason {
		return s
	}
	return s[:maxRegoReason] + "..."
}

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

// regoInputSchema is the shape of the input document, which checkInputRefs holds every module's
// references against. A nested map is an object with fixed keys, regoAnyKeys is an object whose keys
// are the operator's own, and nil is a value with nothing named below it. A test holds this to the
// document RegoInput builds, so the two cannot drift.
var regoInputSchema = map[string]any{
	"version": nil,
	"run": map[string]any{
		"id": nil, "tool": nil, "command": nil, "playbook": nil,
		"inventory": map[string]any{"id": nil, "path": nil},
		"limit":     nil, "queue": nil, "project_id": nil, "dry_run": nil, "kind": nil,
		"dry_run_findings": nil, "change_free": nil,
		"step_name": nil, "source": nil, "source_id": nil, "template_id": nil,
		"proposed_from": nil, "intent": nil, "image": nil, "tags": nil, "skip_tags": nil,
		"labels":          regoAnyKeys,
		"extra_var_names": nil, "credential_ids": nil,
	},
	"actor":         map[string]any{"name": nil, "type": nil, "kind": nil, "account_id": nil},
	"plan":          map[string]any{"planned": nil, "destroys": nil},
	"risk":          map[string]any{"level": nil, "reasons": nil},
	"reversibility": map[string]any{"class": nil, "reasons": nil},
	"time":          map[string]any{"submitted_at": nil, "submitted_at_ns": nil},
}

// regoAnyKeysMarker is the type of regoAnyKeys.
type regoAnyKeysMarker struct{}

// regoAnyKeys marks an object in the schema whose keys are not fixed, such as the run's labels.
var regoAnyKeys = regoAnyKeysMarker{}

// checkInputRefs refuses a module that references an input field the document does not have.
//
// In Rego a reference to a missing field is undefined, and an undefined body is a rule that does not
// fire. So a deny rule reading input.run.toool instead of input.run.tool loads cleanly, evaluates
// cleanly, and refuses nothing, forever. The document is fixed and versioned, so a reference to a
// field outside it is a mistake that can be named at load, which is the only place it is cheap.
// The check covers references spelled from input; a module that copies input into a variable and
// reads through the copy is checked only as far as the copy.
func checkInputRefs(file string, mod *ast.Module) error {
	var bad error
	ast.WalkRefs(mod, func(ref ast.Ref) bool {
		if bad != nil || !ref.HasPrefix(ast.InputRootRef) {
			return false
		}
		var node any = regoInputSchema
		path := "input"
		for _, term := range ref[1:] {
			key, ok := term.Value.(ast.String)
			switch shape := node.(type) {
			case map[string]any:
				if !ok {
					// A variable or number indexing a fixed object cannot be checked, and is left to
					// the evaluation.
					return false
				}
				next, known := shape[string(key)]
				if !known {
					bad = fmt.Errorf("%w: %s: %s.%s is not a field of the input document",
						ErrRego, file, path, string(key))
					return false
				}
				node = next
				path += "." + string(key)
			case regoAnyKeysMarker:
				return false
			default:
				if ok {
					bad = fmt.Errorf("%w: %s: %s has no field %q", ErrRego, file, path, string(key))
				}
				return false
			}
		}
		return false
	})
	return bad
}

// inputFieldsRead returns the top-level input fields a module references, such as plan.
func inputFieldsRead(mod *ast.Module) []string {
	seen := map[string]bool{}
	ast.WalkRefs(mod, func(ref ast.Ref) bool {
		if len(ref) > 1 && ref.HasPrefix(ast.InputRootRef) {
			if key, ok := ref[1].Value.(ast.String); ok {
				seen[string(key)] = true
			}
		}
		return false
	})
	return sortedKeys(seen)
}

// regoVerdict returns a copy of a Rego policy standing for one decision about one run, labeled
// with what decided and why so the hold, the refusal, and the record all name the bundle that
// decided rather than only the rule's name.
func (p *Policy) regoVerdict(effect string, reasons []string, err error) *Policy {
	cp := *p
	cp.Effect = effect
	why := strings.Join(reasons, ", ")
	if err != nil {
		why = err.Error()
	}
	cp.Name = p.regoLabel(why)
	return &cp
}

// regoLabel names a Rego policy's decision about one run: the policy, why, and the first twelve
// characters of its bundle digest.
func (p *Policy) regoLabel(why string) string {
	return fmt.Sprintf("%s (%s, rego sha256:%s)", p.Label(), why, p.Rego.Digest()[:12])
}

// noteLabel names what one Rego policy noted about a run in the form a hold is named, carrying at
// most maxNoteReasons of its messages and saying how many more there were.
func (p *Policy) noteLabel(reasons []string) string {
	shown := reasons
	if len(reasons) > maxNoteReasons {
		shown = append(slices.Clone(reasons[:maxNoteReasons]),
			fmt.Sprintf("and %d more", len(reasons)-maxNoteReasons))
	}
	return p.regoLabel(strings.Join(shown, ", "))
}
