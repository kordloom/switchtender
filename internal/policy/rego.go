package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/paid"
	"github.com/kordloom/switchtender/internal/run"
)

// Rego syntaxes a policy file may name for a module.
const (
	// RegoSyntaxV1 is the Rego syntax OPA 1.0 made the default: rule bodies after if, partial sets
	// declared with contains. It is what a module is parsed as when the file names no syntax.
	RegoSyntaxV1 = "v1"
	// RegoSyntaxV0 is the syntax OPA used before 1.0, which most Conftest policies in the wild are
	// written in. Accepting it is what lets an existing rule port without being rewritten first.
	RegoSyntaxV0 = "v0"
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

// RegoUnavailableReason is the hold a stored Rego policy places on every run in a build that cannot
// evaluate it. The policy came from a control node or a backup that could; this build keeps the
// rule in force the only way it can, by holding what the rule might have held and saying why.
const RegoUnavailableReason = "Rego policies require Team; this build cannot evaluate this " +
	"policy, so the run is held"

// maxNoteReasons bounds how many of one policy's messages a note carries. A note is written on
// every run the policy warns about, held or not, and committed with that run's outcome, so a module
// emitting a message per variable name would otherwise grow every record it touches.
const maxNoteReasons = 10

// RegoModule is one Rego source file a Rego policy compiles.
type RegoModule = paid.RegoModule

// RegoDecision is what a Rego policy decided about one run, in the vocabulary the YAML rules use.
type RegoDecision = paid.RegoDecision

// RegoProgram is a Rego policy as the gate holds it: one or more modules, the package whose rules
// decide, the digest that names this exact bundle in the evidence, and the settings beside them. It
// is immutable once built, so copies of a Policy may share it.
//
// Compiling and evaluating the modules is paid work, done by the compiler the paid module registers
// with internal/paid. A Community build has only the stub, so CompileRego refuses there, and a
// program that arrives already compiled elsewhere, over the relay or in a backup, is kept with no
// evaluator behind it: it holds every run and says why, since dropping it would ungate the runs it
// was written to hold.
type RegoProgram struct {
	// pkg is the decision package's path, for example data.switchtender.
	pkg string
	// syntax is the Rego syntax the modules are parsed as.
	syntax string
	// modules are the sources, sorted by file.
	modules []RegoModule
	// digest is the hex SHA-256 over the canonical bundle.
	digest string
	// timeout bounds one evaluation.
	timeout time.Duration
	// warn is what the warn rule does: RegoWarnHold or RegoWarnNote.
	warn string
	// compiled evaluates the bundle, nil in a build that cannot.
	compiled paid.RegoProgram
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

// regoSyntax returns the syntax name a policy spelled, v1 for none, refusing one this build does
// not know.
func regoSyntax(syntax string) (string, error) {
	switch syntax {
	case "":
		return RegoSyntaxV1, nil
	case RegoSyntaxV1, RegoSyntaxV0:
		return syntax, nil
	default:
		return "", fmt.Errorf("%w: syntax must be %q or %q, not %q",
			ErrRego, RegoSyntaxV1, RegoSyntaxV0, syntax)
	}
}

// regoModules returns modules sorted by file, refusing an empty bundle and a file named twice.
func regoModules(modules []RegoModule) ([]RegoModule, error) {
	if len(modules) == 0 {
		return nil, fmt.Errorf("%w: no module to compile", ErrRego)
	}
	seen := make(map[string]bool, len(modules))
	for _, m := range modules {
		if seen[m.File] {
			return nil, fmt.Errorf("%w: %s is named twice", ErrRego, m.File)
		}
		seen[m.File] = true
	}
	sorted := slices.Clone(modules)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].File < sorted[j].File })
	return sorted, nil
}

// regoPackage returns pkg spelled as a data path, so data.switchtender and switchtender name the
// same package and digest the same way.
func regoPackage(pkg string) string {
	if pkg != "" && !strings.HasPrefix(pkg, "data.") {
		return "data." + pkg
	}
	return pkg
}

// CompileRego compiles modules into a program deciding from the named package. An empty pkg means
// the package the first module declares. syntax is v1 or v0, empty meaning v1. The options set what
// the warn rule does and how long one evaluation may run. Without them a warning holds the run and
// an evaluation may run for DefaultRegoTimeout.
//
// Everything that can be found wrong before a run arrives is found here, so a policy file with a
// mistake in it is refused at load the way a malformed YAML rule is, rather than loaded and quietly
// deciding nothing. The settings, the syntax name, and the bundle's shape are checked here; the
// paid compiler then refuses a parse or compile error, a call to a builtin that reaches outside the
// process, a package that defines no decision rule, a reference to an input field the document does
// not have, a module that reads input.plan without ever asking for a plan, and warn set to note on a
// package with no warn rule for it to change.
//
// A Community build has no compiler and refuses with paid.ErrRequiresTier naming Team, wrapped in
// ErrRego, so the policy file does not load and the refusal reads as the license refusal does.
func CompileRego(pkg, syntax string, modules []RegoModule, opts ...RegoOption) (*RegoProgram, error) {
	settings := regoSettings{warn: RegoWarnHold, timeout: DefaultRegoTimeout}
	for _, opt := range opts {
		opt(&settings)
	}
	if err := settings.check(); err != nil {
		return nil, err
	}
	syntax, err := regoSyntax(syntax)
	if err != nil {
		return nil, err
	}
	sorted, err := regoModules(modules)
	if err != nil {
		return nil, err
	}
	// The modules go to the compiler in the order given, since an empty pkg names the first one's.
	compiled, err := paid.CurrentRegoCompiler().Compile(modules, paid.RegoSettings{
		Package: regoPackage(pkg), Syntax: syntax, Timeout: settings.timeout, Warn: settings.warn,
	})
	if err != nil {
		if errors.Is(err, paid.ErrRequiresTier) {
			return nil, fmt.Errorf("%w: %w", ErrRego, err)
		}
		return nil, err
	}
	resolved := compiled.Package()
	return &RegoProgram{
		pkg: resolved, syntax: syntax, modules: sorted, digest: RegoDigest(resolved, syntax, sorted),
		timeout: settings.timeout, warn: settings.warn, compiled: compiled,
	}, nil
}

// RegoDigest returns the hex SHA-256 naming a bundle: the package, the syntax, and every module's
// path and text, sorted by path. It is the value the evidence records for a Rego policy, so a
// reader holding the modules at a commit can recompute it and compare.
func RegoDigest(pkg, syntax string, modules []RegoModule) string {
	sorted := slices.Clone(modules)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].File < sorted[j].File })
	raw, err := json.Marshal(struct {
		Package string       `json:"package"`
		Syntax  string       `json:"syntax"`
		Modules []RegoModule `json:"modules"`
	}{Package: pkg, Syntax: syntax, Modules: sorted})
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
// reversibility. A program this build cannot read is taken to read everything, so a caller that
// prepares the field prepares it.
func (p *RegoProgram) Reads(field string) bool {
	if p.compiled == nil {
		return true
	}
	return p.compiled.Reads(field)
}

// Warn returns what the program's warn rule does: RegoWarnHold or RegoWarnNote.
func (p *RegoProgram) Warn() string { return p.warn }

// Timeout returns how long one evaluation of the program may run.
func (p *RegoProgram) Timeout() time.Duration { return p.timeout }

// Evaluable reports whether this build can evaluate the program. False means every decision it
// gives is the hold RegoUnavailableReason names.
func (p *RegoProgram) Evaluable() bool { return p.compiled != nil }

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

// UnmarshalJSON rebuilds the program the sources describe, with the settings they arrived under,
// and refuses one whose sources do not produce the digest it claims. A relay worker reads the control
// node's policies this way, and a program that cannot be rebuilt here is an error the worker fails
// closed on, never an empty rule.
//
// In a build with the paid compiler the modules are compiled again, so a module that no longer
// compiles is refused. In a Community build the program is kept uncompiled: its digest, settings,
// and sources are intact, it shows in the in-force record as it did where it came from, and every
// decision it gives is a hold naming the tier, which is the one safe reading of a rule this build
// cannot evaluate.
func (p *RegoProgram) UnmarshalJSON(raw []byte) error {
	var in regoProgramJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("%w: decode: %w", ErrRego, err)
	}
	settings := regoSettings{warn: in.Warn, timeout: DefaultRegoTimeout}
	if in.Timeout != "" {
		limit, err := time.ParseDuration(in.Timeout)
		if err != nil {
			return fmt.Errorf("%w: timeout %q is not a duration: %w", ErrRego, in.Timeout, err)
		}
		settings.timeout = limit
	}
	if err := settings.check(); err != nil {
		return err
	}
	syntax, err := regoSyntax(in.Syntax)
	if err != nil {
		return err
	}
	sorted, err := regoModules(in.Modules)
	if err != nil {
		return err
	}
	pkg := regoPackage(in.Package)
	if pkg == "" {
		return fmt.Errorf("%w: the program names no package", ErrRego)
	}
	if digest := RegoDigest(pkg, syntax, sorted); digest != in.SHA256 {
		return fmt.Errorf("%w: the modules digest to %s, not the %s they arrived under",
			ErrRego, digest, in.SHA256)
	}
	compiled, err := paid.CurrentRegoCompiler().Compile(sorted, paid.RegoSettings{
		Package: pkg, Syntax: syntax, Timeout: settings.timeout, Warn: settings.warn,
	})
	switch {
	case err == nil:
	case errors.Is(err, paid.ErrRequiresTier):
		compiled = nil
	default:
		return err
	}
	*p = RegoProgram{
		pkg: pkg, syntax: syntax, modules: sorted, digest: in.SHA256,
		timeout: settings.timeout, warn: settings.warn, compiled: compiled,
	}
	return nil
}

// Decide evaluates the program against r and reports what it decided.
//
// It never fails open. A run it cannot build an input for, an evaluation error, a timeout, a
// package that evaluates to nothing, and a decision rule of the wrong type all come back as a
// decision carrying Err, which every caller reads as a refusal. A program this build cannot
// evaluate holds the run with RegoUnavailableReason.
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

// decide evaluates the program against r once, with the input r describes.
func (p *RegoProgram) decide(r *run.Run) RegoDecision {
	if r == nil {
		return RegoDecision{Err: fmt.Errorf("%w: no run to evaluate", ErrRego)}
	}
	// A program that was never built has nothing to evaluate. Only CompileRego and UnmarshalJSON
	// build one, so this is a program assembled by hand, and it decides nothing but a refusal.
	if p == nil || p.digest == "" {
		return RegoDecision{Err: fmt.Errorf("%w: the program was never compiled", ErrRego)}
	}
	if p.compiled == nil {
		return RegoDecision{Hold: []string{RegoUnavailableReason}}
	}
	return p.compiled.Evaluate(r)
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
