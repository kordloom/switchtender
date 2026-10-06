package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// filePolicyID derives a stable identifier from a policy's name.
//
// It is a hash rather than a counter so it survives reordering the file, and so two installs reading
// the same file agree on it. A recorded approval names the policy that held the run, and that name
// has to still resolve after the file is edited.
func filePolicyID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "pol_file_" + hex.EncodeToString(sum[:6])
}

// fileDoc is the shape of a policy file. The wrapper exists so the file can gain other top-level
// keys later without breaking every file already written against it.
type fileDoc struct {
	// Policies are the approval policies this file declares.
	Policies []filePolicy `yaml:"policies" json:"policies"`
	// Rego are the Rego policies this file loads, each compiled from modules beside it.
	Rego []fileRego `yaml:"rego,omitempty" json:"rego,omitempty"`
}

// fileRego is one Rego policy as an operator declares it: a name, the modules that make it up, and
// optionally the package that decides, the syntax the modules are written in, what a warning does,
// and how long one evaluation may run.
//
// The modules are read from paths relative to the policy file, so the YAML and the Rego it loads
// live in one repository, change in one reviewed diff, and are deployed together by one merge.
type fileRego struct {
	// Name identifies the policy, as a YAML policy's name does.
	Name string `yaml:"name" json:"name"`
	// Files are the Rego modules, relative to the policy file's directory.
	Files []string `yaml:"files" json:"files"`
	// Package is the package whose rules decide. Omit it to use the first module's package.
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	// Syntax is v1, the default, or v0 for a module written before OPA 1.0.
	Syntax string `yaml:"syntax,omitempty" json:"syntax,omitempty"`
	// Warn is what the package's warn rule does: hold, the default, holds the run for approval, and
	// note records the warning on the run and lets it go ahead.
	Warn string `yaml:"warn,omitempty" json:"warn,omitempty"`
	// Timeout bounds one evaluation, written as a duration with a unit such as 750ms or 2s. Omit it
	// for 500ms. It may be at most 10s.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// RequireReason asks for the approver's reason on a decision about a run this policy holds:
	// denials or always. Omit for none.
	RequireReason string `yaml:"require_reason,omitempty" json:"require_reason,omitempty"`
}

// fileStamp is what a file looked like when it was last parsed.
type fileStamp struct {
	// modTime is the file's modification time.
	modTime time.Time
	// size is the file's size, since a modification time alone can repeat within a filesystem's
	// timestamp granularity.
	size int64
}

// filePolicy is one policy as an operator writes it.
//
// It is deliberately not policy.Policy. A stored policy carries an id and a creation time the
// server assigns, and asking a person to invent those in a file they review would put two things in
// the diff that no reviewer can check. The name is the identity here, because that is what a
// reviewer reads.
type filePolicy struct {
	// Name identifies the policy and is what a reviewer reads in a diff.
	Name string `yaml:"name" json:"name"`
	// Tool matches a run's execution tool. Empty matches any.
	Tool string `yaml:"tool,omitempty" json:"tool,omitempty"`
	// CommandContains matches when a run's command contains this text. Empty matches any.
	CommandContains string `yaml:"command_contains,omitempty" json:"command_contains,omitempty"`
	// InventoryID matches a run targeting this stored inventory. Empty matches any.
	InventoryID string `yaml:"inventory_id,omitempty" json:"inventory_id,omitempty"`
	// Queue matches a run routed to this worker queue. Empty matches any.
	Queue string `yaml:"queue,omitempty" json:"queue,omitempty"`
	// ExcludeDryRun leaves dry-run runs unmatched.
	ExcludeDryRun bool `yaml:"exclude_dry_run,omitempty" json:"exclude_dry_run,omitempty"`
	// MaxDestroy holds a matched terraform or opentofu run when its plan would destroy more than
	// this many resources. Omit it for a blanket policy, which is the safe default.
	MaxDestroy *int `yaml:"max_destroy,omitempty" json:"max_destroy,omitempty"`
	// ActorKind matches who fired the run: agent or human. Omit to match any actor.
	ActorKind string `yaml:"actor_kind,omitempty" json:"actor_kind,omitempty"`
	// RequireDistinctApprover refuses a decision by the person who asked for the change.
	RequireDistinctApprover bool `yaml:"require_distinct_approver,omitempty" json:"require_distinct_approver,omitempty"`
	// Actor matches the exact requesting actor recorded on the run. Omit to match any.
	Actor string `yaml:"actor,omitempty" json:"actor,omitempty"`
	// Account matches the username of the account the requesting credential is bound to. Omit to
	// match any. An exemption that names an actor must name it too.
	Account string `yaml:"account,omitempty" json:"account,omitempty"`
	// MinRisk matches only runs assessed at least this risky: low, medium, or high. Omit for any.
	MinRisk string `yaml:"min_risk,omitempty" json:"min_risk,omitempty"`
	// Reversibility matches only runs at least as hard to undo as this class: reversible, costly,
	// or irreversible. It is a floor, so costly covers costly and irreversible. Omit for any.
	Reversibility string `yaml:"reversibility,omitempty" json:"reversibility,omitempty"`
	// Effect is what a match does: require_approval, the default, deny, or exempt.
	Effect string `yaml:"effect,omitempty" json:"effect,omitempty"`
	// RequireReason asks for the approver's reason on a decision: denials or always. Omit for none.
	RequireReason string `yaml:"require_reason,omitempty" json:"require_reason,omitempty"`
}

// FileStore serves approval policies from a file on disk rather than from the database.
//
// Policies decide which runs a person has to approve, so who may change them is the whole question.
// Kept as rows, they are changed by anyone the API lets through, and the change leaves a row that
// looks exactly like the row before it. Kept in a file, a change is a diff: it goes through whatever
// review the repository holding it requires, it is attributable to a commit, and an auditor can read
// the policy that was in force at any point in history by checking out that commit.
//
// The file is the source of truth while it is configured. Writes are refused rather than quietly
// applied to a database nobody is reading, because a policy change that appears to succeed and has
// no effect is worse than one that is rejected.
type FileStore struct {
	// path is the file being served.
	path string
	// mu guards the cached parse.
	mu sync.RWMutex
	// cached holds the last successful parse.
	cached []*Policy
	// modTime is the file's modification time when cached was parsed.
	modTime time.Time
	// size is the file's size when cached was parsed, since a modification time alone can repeat
	// within a filesystem's timestamp granularity.
	size int64
	// regoStamps are the Rego modules the cached parse read, so editing one reloads the set just
	// as editing the policy file does.
	regoStamps map[string]fileStamp
}

// compile-time proof that FileStore is a Store.
var _ Store = (*FileStore)(nil)

// NewFileStore reads path and returns a store serving the policies it declares.
//
// It fails when the file cannot be read or parsed, and the caller is expected to refuse to start.
// A malformed policy file must never degrade to no policies: that turns a typo into an install
// where nothing is gated and nothing says so.
func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the file being served, for logging and for the doctor.
func (s *FileStore) Path() string { return s.path }

// load re-reads the file when it has changed and returns the current policies. A change takes effect
// without a restart, which matters because the point of holding policies in a repository is that
// merging a pull request is what deploys them.
func (s *FileStore) load() ([]*Policy, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, fmt.Errorf("policy file: %w", err)
	}
	s.mu.RLock()
	fresh := s.cached != nil && info.ModTime().Equal(s.modTime) && info.Size() == s.size &&
		stampsCurrent(s.regoStamps)
	cached := s.cached
	s.mu.RUnlock()
	if fresh {
		return cached, nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("policy file: %w", err)
	}
	var doc fileDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("policy file %s: %w", s.path, err)
	}
	parsed := make([]*Policy, 0, len(doc.Policies))
	for i, fp := range doc.Policies {
		if fp.Name == "" {
			return nil, fmt.Errorf("policy file %s: policy %d has no name", s.path, i)
		}
		if fp.Tool != "" && !run.ValidTool(fp.Tool) {
			return nil, fmt.Errorf("policy file %s: policy %q names unknown tool %q",
				s.path, fp.Name, fp.Tool)
		}
		// An omitted threshold is a blanket policy, which holds every matching run. Defaulting the
		// other way would turn a policy an operator meant as a hard gate into one that only fires
		// on a large destroy.
		maxDestroy := DisabledMaxDestroy
		if fp.MaxDestroy != nil {
			if *fp.MaxDestroy < 0 {
				return nil, fmt.Errorf("policy file %s: policy %q has a negative max_destroy; omit "+
					"it for a blanket policy", s.path, fp.Name)
			}
			maxDestroy = *fp.MaxDestroy
		}
		p := &Policy{
			// The id is derived from the name so it is stable across reloads and across installs
			// reading the same file, and so an approval recorded against a policy still resolves.
			ID:                      filePolicyID(fp.Name),
			Name:                    fp.Name,
			Tool:                    fp.Tool,
			CommandContains:         fp.CommandContains,
			InventoryID:             fp.InventoryID,
			Queue:                   fp.Queue,
			ExcludeDryRun:           fp.ExcludeDryRun,
			MaxDestroy:              maxDestroy,
			ActorKind:               fp.ActorKind,
			RequireDistinctApprover: fp.RequireDistinctApprover,
			Actor:                   fp.Actor,
			Account:                 fp.Account,
			MinRisk:                 fp.MinRisk,
			Reversibility:           fp.Reversibility,
			Effect:                  fp.Effect,
			RequireReason:           fp.RequireReason,
			CreatedAt:               info.ModTime().UTC(),
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("policy file %s: policy %q: %w", s.path, fp.Name, err)
		}
		parsed = append(parsed, p)
	}
	regoPolicies, stamps, err := s.loadRego(doc, info.ModTime().UTC())
	if err != nil {
		return nil, err
	}
	parsed = append(parsed, regoPolicies...)

	s.mu.Lock()
	s.cached = parsed
	s.modTime = info.ModTime()
	s.size = info.Size()
	s.regoStamps = stamps
	s.mu.Unlock()
	return parsed, nil
}

// regoModulePath resolves a rego policy's files entry against the policy file's directory and refuses
// one that escapes it. A files entry is a path relative to the policy file, so an absolute path or one
// that climbs out with .. would read a module from anywhere on disk, which the policy file's own
// directory is meant to bound. The check is lexical, which is enough here: the policy file is reviewed
// deployment configuration, so this stops a stray path, not a planted symbolic link.
func regoModulePath(dir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: a module file has no name", ErrRego)
	}
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("%w: module file %q is an absolute path, but a files entry is read "+
			"relative to the policy file", ErrRego, rel)
	}
	full := filepath.Join(dir, rel)
	within, err := filepath.Rel(dir, full)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: module file %q is outside the policy directory, which it may not "+
			"leave", ErrRego, rel)
	}
	return full, nil
}

// loadRego compiles the Rego policies the file declares and records what each module looked like,
// so a later edit to a module is noticed. Any error refuses the whole file, exactly as a malformed
// YAML rule does: a Rego policy that cannot compile must not degrade to no policy.
func (s *FileStore) loadRego(doc fileDoc, created time.Time) ([]*Policy, map[string]fileStamp, error) {
	stamps := map[string]fileStamp{}
	if len(doc.Rego) == 0 {
		return nil, stamps, nil
	}
	names := make(map[string]bool, len(doc.Policies)+len(doc.Rego))
	for _, fp := range doc.Policies {
		names[fp.Name] = true
	}
	dir := filepath.Dir(s.path)
	out := make([]*Policy, 0, len(doc.Rego))
	for i, fr := range doc.Rego {
		if fr.Name == "" {
			return nil, nil, fmt.Errorf("policy file %s: rego policy %d has no name", s.path, i)
		}
		// The id is derived from the name, so two rules sharing one would share an id and a
		// recorded hold could not say which of them it meant.
		if names[fr.Name] {
			return nil, nil, fmt.Errorf("policy file %s: rego policy %q shares its name with "+
				"another policy in the file", s.path, fr.Name)
		}
		names[fr.Name] = true
		if len(fr.Files) == 0 {
			return nil, nil, fmt.Errorf("policy file %s: rego policy %q names no files",
				s.path, fr.Name)
		}
		modules := make([]RegoModule, 0, len(fr.Files))
		for _, rel := range fr.Files {
			full, err := regoModulePath(dir, rel)
			if err != nil {
				return nil, nil, fmt.Errorf("policy file %s: rego policy %q: %w", s.path, fr.Name, err)
			}
			info, err := os.Stat(full)
			if err != nil {
				return nil, nil, fmt.Errorf("policy file %s: rego policy %q: %w", s.path, fr.Name, err)
			}
			src, err := os.ReadFile(full)
			if err != nil {
				return nil, nil, fmt.Errorf("policy file %s: rego policy %q: %w", s.path, fr.Name, err)
			}
			stamps[full] = fileStamp{modTime: info.ModTime(), size: info.Size()}
			modules = append(modules, RegoModule{File: rel, Source: string(src)})
		}
		opts := []RegoOption{WithRegoWarn(fr.Warn)}
		if fr.Timeout != "" {
			// A bare number is refused rather than read in some unit, since 500 means half a second
			// to one reader and more than eight minutes to another.
			limit, err := time.ParseDuration(fr.Timeout)
			if err != nil {
				return nil, nil, fmt.Errorf("policy file %s: rego policy %q: timeout %q is not a "+
					"duration: write it with a unit, such as 500ms or 2s", s.path, fr.Name, fr.Timeout)
			}
			opts = append(opts, WithRegoTimeout(limit))
		}
		prog, err := CompileRego(fr.Package, fr.Syntax, modules, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("policy file %s: rego policy %q: %w", s.path, fr.Name, err)
		}
		p := &Policy{
			ID:            filePolicyID(fr.Name),
			Name:          fr.Name,
			MaxDestroy:    DisabledMaxDestroy,
			Rego:          prog,
			RequireReason: fr.RequireReason,
			CreatedAt:     created,
		}
		if err := p.Validate(); err != nil {
			return nil, nil, fmt.Errorf("policy file %s: rego policy %q: %w", s.path, fr.Name, err)
		}
		out = append(out, p)
	}
	return out, stamps, nil
}

// stampsCurrent reports whether every recorded file still looks as it did when it was parsed. A file
// that can no longer be read is not current, so the reload that follows reports why.
func stampsCurrent(stamps map[string]fileStamp) bool {
	for path, was := range stamps {
		info, err := os.Stat(path)
		if err != nil || !info.ModTime().Equal(was.modTime) || info.Size() != was.size {
			return false
		}
	}
	return true
}

// List returns every policy the file declares, in the order it declares them.
//
// The cached parse is copied out rather than shared. Handing callers the cached slice and its
// pointers meant a caller mutating a policy changed what the store served to everyone afterwards,
// and concurrent readers alongside any such caller were a data race on shared values.
func (s *FileStore) List(_ context.Context) ([]*Policy, error) {
	cached, err := s.load()
	if err != nil {
		return nil, err
	}
	// The license is checked on every read, not only at startup.
	//
	// The file hot-reloads, and the check lived in serve's startup path alone, so a Community
	// install started with a plain file and then had deny rules, risk floors and actor scoping
	// added to it afterward ran the full policy engine, uncapped, for as long as the process
	// lived. The gate was one edit away from not existing.
	//
	// It returns an error rather than quietly dropping the rules it cannot license. Dropping them
	// would ungate runs those rules were written to hold, which is the one direction this must
	// never fail: the dispatcher already treats an unreadable policy set as a refusal to run,
	// which is disruptive once and in the safe direction.
	if lerr := s.allowed(cached); lerr != nil {
		return nil, lerr
	}
	out := make([]*Policy, len(cached))
	for i, p := range cached {
		cp := *p
		out[i] = &cp
	}
	return out, nil
}

// allowed reports whether the current license covers this policy set, by count and by the features
// the rules use. It is the same pair of checks serve makes at startup, applied to what the file says
// now rather than to what it said then.
//
// A lapse is not a refusal. Refusing here stops every run, because the dispatcher reads an
// unreadable policy set as a reason not to run, and the server will not start either. Applied to an
// expired term that meant a paid install went completely dark at midnight: not degraded, not capped,
// dark. The terms promise the opposite in as many words, that a lapsed license takes nothing and
// that a running install is not bricked over a billing dispute, and it is one of the seven
// commitments written as contractual.
//
// So a lapsed term keeps serving the rules it was already serving. That is the only option of the
// three that is safe in both directions at once: refusing takes the install down, dropping the
// advanced rules silently ungates the runs those rules exist to hold, and continuing to enforce
// them takes nothing from anybody. Enforcing a constraint the customer wrote is not a feature being
// given away, it is their own safety rule still working.
//
// What a lapse stops is authoring new paid policy through the API, which the create and update
// handlers still refuse. It does not stop it through the file, because the file hot-reloads and
// this check is what reads it: a lapsed install can add a deny rule by editing the file, and it
// takes effect. That is deliberate rather than overlooked. The alternative is to remember the set
// in force at the moment the lapse was noticed and refuse anything larger, which means refusing to
// load a file whose only change was to tighten a rule, on an install that is already paying no
// attention to the term. Status names the lapse.
func (s *FileStore) allowed(set []*Policy) error {
	for _, p := range set {
		if p.Advanced() {
			if err := license.Allow(license.FeaturePolicyFull); err != nil {
				if errors.Is(err, license.ErrLapsed) {
					break
				}
				if rego := firstRego(set); rego != nil {
					return fmt.Errorf("the policy file needs a license it does not have: Rego "+
						"policy %q is part of the full policy engine: %w", rego.Name, err)
				}
				return fmt.Errorf("the policy file needs a license it does not have: %w", err)
			}
			break
		}
	}
	if err := license.AllowPolicies(len(set)); err != nil && !errors.Is(err, license.ErrLapsed) {
		return fmt.Errorf("the policy file needs a license it does not have: %w", err)
	}
	return nil
}

// firstRego returns the first Rego policy in set, or nil, so a refusal names the Rego policy rather
// than leaving an operator to guess which line of the file the license does not cover.
func firstRego(set []*Policy) *Policy {
	for _, p := range set {
		if p.Rego != nil {
			return p
		}
	}
	return nil
}

// Get returns the policy with the given id, or ErrNotFound.
func (s *FileStore) Get(ctx context.Context, id string) (*Policy, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.ID == id {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// Save refuses. The file is the source of truth, so a policy change belongs in a diff.
func (s *FileStore) Save(context.Context, *Policy) error {
	return s.ReadOnly()
}

// ReadOnly returns why the file store refuses writes.
func (s *FileStore) ReadOnly() error {
	return fmt.Errorf("%w: policies are read from %s, so change them there and let review and "+
		"deployment apply it", ErrReadOnly, s.path)
}

// Delete refuses, for the same reason as Save.
func (s *FileStore) Delete(context.Context, string) error {
	return fmt.Errorf("%w: policies are read from %s, so remove it there and let review and "+
		"deployment apply it", ErrReadOnly, s.path)
}

// Unreachable is a policy Store that cannot answer, for a process that has no way to read the
// policies at all.
//
// A relay worker leases runs across a segment boundary and never sees the control node's database.
// Handing it a nil store made the plan-content gate silently vanish, because that gate runs where
// the run executes: a terraform apply scoped by a destroy threshold was planned and held when the
// control node claimed it, and applied straight to production when a worker did, decided by a race
// between claim loops. Erroring instead makes the gate fail closed, so the run is refused with a
// reason rather than applied past a check nobody performed.
type Unreachable struct{}

// compile-time proof that Unreachable is a Store.
var _ Store = Unreachable{}

// List reports that the policies cannot be read from here.
func (Unreachable) List(context.Context) ([]*Policy, error) {
	return nil, fmt.Errorf("%w: this process leases runs across a relay and cannot read the "+
		"approval policies, so it cannot tell whether this run needs one", ErrUnreachable)
}

// Get reports that the policies cannot be read from here.
func (Unreachable) Get(context.Context, string) (*Policy, error) {
	return nil, ErrUnreachable
}

// Save refuses, since there is nothing here to write to.
func (Unreachable) Save(context.Context, *Policy) error { return ErrUnreachable }

// Delete refuses, since there is nothing here to write to.
func (Unreachable) Delete(context.Context, string) error { return ErrUnreachable }
