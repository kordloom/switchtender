package inventorytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
)

// ErrLimitUnsupported is returned when a test asks ListingRunner to apply a limit, which only real
// Ansible evaluates.
var ErrLimitUnsupported = errors.New("listing runner does not apply limits")

// ListingRunner is a runner for tests that resolves composed inventories without Ansible installed.
//
// It stands in for ansible-inventory with the native engine, whose agreement with Ansible the
// inventory package's conformance corpus proves. ListInventory merges the native resolution of
// every source in order, which is what the constructed plugin does when it is given no options, and
// skips the constructed plugin config itself. ReadInventories reads each file natively, as the
// executor's cross-check would with Ansible, so a test can show a disagreement by rewriting what it
// reports. Run records the inventory content the executor was handed, so a test can see exactly
// which hosts a run reached.
type ListingRunner struct {
	// Version is the ansible-core version it reports, 2.18.1 when empty.
	Version string
	// Missing makes it report Ansible as not installed.
	Missing bool
	// Commands is where it reports its Ansible commands come from.
	Commands ansibleruntime.Commands
	// Rewrite, when set, replaces what ReadInventories reports for the file named name, so a test
	// can make Ansible's reading disagree with the native engine's.
	Rewrite func(name string, listing []byte) []byte
	// Canned maps a document only Ansible can read, such as a plugin configuration, to the
	// ansible-inventory --list JSON ListInventory reports for it, standing in for the plugin.
	Canned map[string]string
	// mu guards executed, lists, and reads.
	mu sync.Mutex
	// executed holds the inventory content each Run received, in order.
	executed []string
	// lists counts ListInventory calls.
	lists int
	// reads counts ReadInventories calls.
	reads int
}

// Run records the content of the inventory the spec points at and succeeds.
func (l *ListingRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	body, err := os.ReadFile(spec.Inventory)
	if err != nil {
		return roundhouse.Result{ExitCode: 1}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.executed = append(l.executed, string(body))
	return roundhouse.Result{ExitCode: 0}, nil
}

// Executed returns the inventory content each run received, in order.
func (l *ListingRunner) Executed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.executed)
}

// Lists returns how many listings were asked for.
func (l *ListingRunner) Lists() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lists
}

// Reads returns how many cross-check readings were asked for.
func (l *ListingRunner) Reads() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// AnsibleCommands reports Commands.
func (l *ListingRunner) AnsibleCommands() ansibleruntime.Commands {
	return l.Commands
}

// AnsibleCoreVersion reports Version, or roundhouse.ErrAnsibleMissing when Missing is set.
func (l *ListingRunner) AnsibleCoreVersion(context.Context) (string, error) {
	if l.Missing {
		return "", roundhouse.ErrAnsibleMissing
	}
	if l.Version == "" {
		return "2.18.1", nil
	}
	return l.Version, nil
}

// listedGroup is one group of a listing as ansible-inventory prints it.
type listedGroup struct {
	// Hosts are the group's direct members.
	Hosts []string `json:"hosts,omitempty"`
	// Children are the group's child groups.
	Children []string `json:"children,omitempty"`
}

// ListInventory merges the native resolution of the documents in sources, later sources winning a
// host variable both set. A limit is refused, since only Ansible evaluates a pattern.
func (l *ListingRunner) ListInventory(_ context.Context, sources []string, limit string) ([]byte, error) {
	l.mu.Lock()
	l.lists++
	l.mu.Unlock()
	if l.Missing {
		return nil, roundhouse.ErrAnsibleMissing
	}
	if limit != "" {
		return nil, ErrLimitUnsupported
	}
	groups := map[string]*listedGroup{}
	hostVars := map[string]map[string]any{}
	for _, src := range sources {
		if strings.HasSuffix(filepath.Base(src), ".yml") {
			continue
		}
		body, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		raw := []byte(l.Canned[string(body)])
		if len(raw) == 0 {
			resolved, err := inventory.ResolveNative(string(body))
			if err != nil {
				return nil, fmt.Errorf("listing runner: %w", err)
			}
			if raw, err = resolved.ListJSON(); err != nil {
				return nil, err
			}
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		for name, raw := range doc {
			if name == "_meta" {
				var meta struct {
					// HostVars are the per host variables.
					HostVars map[string]map[string]any `json:"hostvars"`
				}
				if err := json.Unmarshal(raw, &meta); err != nil {
					return nil, err
				}
				for h, vars := range meta.HostVars {
					if hostVars[h] == nil {
						hostVars[h] = map[string]any{}
					}
					for k, v := range vars {
						hostVars[h][k] = v
					}
				}
				continue
			}
			if name == "all" {
				continue
			}
			var g listedGroup
			if err := json.Unmarshal(raw, &g); err != nil {
				return nil, err
			}
			have := groups[name]
			if have == nil {
				have = &listedGroup{}
				groups[name] = have
			}
			for _, h := range g.Hosts {
				if !slices.Contains(have.Hosts, h) {
					have.Hosts = append(have.Hosts, h)
				}
			}
			for _, c := range g.Children {
				if !slices.Contains(have.Children, c) {
					have.Children = append(have.Children, c)
				}
			}
		}
	}
	out := map[string]any{"_meta": map[string]any{"hostvars": hostVars}}
	for name, g := range groups {
		out[name] = g
	}
	return json.Marshal(out)
}

// ReadInventories reads each file natively, as the executor's cross-check reads it with Ansible,
// passing each listing through Rewrite when one is set.
func (l *ListingRunner) ReadInventories(_ context.Context, _ roundhouse.Spec, dir string,
	names []string) ([][]byte, string, error) {
	l.mu.Lock()
	l.reads++
	l.mu.Unlock()
	if l.Missing {
		return nil, "", roundhouse.ErrAnsibleMissing
	}
	out := make([][]byte, len(names))
	for i, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, "", err
		}
		resolved, err := inventory.ResolveNative(string(body))
		if err != nil {
			return nil, "", fmt.Errorf("listing runner: %w", err)
		}
		raw, err := resolved.ListJSON()
		if err != nil {
			return nil, "", err
		}
		if l.Rewrite != nil {
			raw = l.Rewrite(name, raw)
		}
		out[i] = raw
	}
	version, _ := l.AnsibleCoreVersion(context.Background())
	return out, version, nil
}

// Listing renders a static inventory document for a test: each group with its hosts, and each
// host's variables written under the first group that names it. A host with variables and no group
// is filed under ungrouped. It is the YAML plugin's static form, written as JSON, which both the
// native engine and Ansible read.
func Listing(groups map[string][]string, vars map[string]map[string]any) string {
	out := map[string]any{}
	written := map[string]bool{}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		hosts := map[string]any{}
		for _, h := range groups[name] {
			if v := vars[h]; len(v) > 0 && !written[h] {
				hosts[h] = v
				written[h] = true
				continue
			}
			hosts[h] = nil
		}
		out[name] = map[string]any{"hosts": hosts}
	}
	var loose []string
	for h := range vars {
		if !written[h] && len(vars[h]) > 0 {
			loose = append(loose, h)
		}
	}
	sort.Strings(loose)
	if len(loose) > 0 {
		hosts := map[string]any{}
		for _, h := range loose {
			hosts[h] = vars[h]
		}
		out["ungrouped"] = map[string]any{"hosts": hosts}
	}
	b, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(b)
}
