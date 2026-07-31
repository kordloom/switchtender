package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

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
	// ExcludeDryRun leaves dry-run runs unmatched.
	ExcludeDryRun bool `yaml:"exclude_dry_run,omitempty" json:"exclude_dry_run,omitempty"`
	// MaxDestroy holds a matched terraform or opentofu run when its plan would destroy more than
	// this many resources. Omit it for a blanket policy, which is the safe default.
	MaxDestroy *int `yaml:"max_destroy,omitempty" json:"max_destroy,omitempty"`
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
	fresh := s.cached != nil && info.ModTime().Equal(s.modTime) && info.Size() == s.size
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
		parsed = append(parsed, &Policy{
			// The id is derived from the name so it is stable across reloads and across installs
			// reading the same file, and so an approval recorded against a policy still resolves.
			ID:              filePolicyID(fp.Name),
			Name:            fp.Name,
			Tool:            fp.Tool,
			CommandContains: fp.CommandContains,
			InventoryID:     fp.InventoryID,
			ExcludeDryRun:   fp.ExcludeDryRun,
			MaxDestroy:      maxDestroy,
			CreatedAt:       info.ModTime().UTC(),
		})
	}

	s.mu.Lock()
	s.cached = parsed
	s.modTime = info.ModTime()
	s.size = info.Size()
	s.mu.Unlock()
	return parsed, nil
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
	out := make([]*Policy, len(cached))
	for i, p := range cached {
		cp := *p
		out[i] = &cp
	}
	return out, nil
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
	return fmt.Errorf("%w: policies are read from %s, so change them there and let review and "+
		"deployment apply it", ErrReadOnly, s.path)
}

// Delete refuses, for the same reason as Save.
func (s *FileStore) Delete(context.Context, string) error {
	return fmt.Errorf("%w: policies are read from %s, so remove it there and let review and "+
		"deployment apply it", ErrReadOnly, s.path)
}
