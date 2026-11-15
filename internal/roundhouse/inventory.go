package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/run"
)

// The Ansible commands the host runner starts, by name, from the directory its locator names or
// from PATH.
const (
	// defaultInventoryBinary is the executable used to enumerate inventory hosts.
	defaultInventoryBinary = "ansible-inventory"
	// defaultPlaybookBinary is the executable a playbook runs with.
	defaultPlaybookBinary = "ansible-playbook"
)

// HostLister enumerates the hosts in an inventory so a run can be split across them. A non-empty
// limit narrows enumeration to the hosts an Ansible pattern matches, so a shard cannot reach a host
// the caller excluded.
type HostLister interface {
	Hosts(ctx context.Context, inventory, limit string) ([]string, error)
}

// InventoryDumper renders an inventory source to the JSON ansible-playbook can consume directly,
// which is how a dynamic source becomes a concrete host list.
type InventoryDumper interface {
	Dump(ctx context.Context, source string, env []string) ([]byte, error)
}

// InventoryLister renders several inventory sources as one ansible-inventory listing, narrowed to
// the hosts a limit matches, which is how a constructed inventory runs the constructed plugin over
// its inputs and how a smart inventory reads each input's hosts and resolved variables.
type InventoryLister interface {
	ListInventory(ctx context.Context, sources []string, limit string) ([]byte, error)
}

// maxListingStderr bounds how much of ansible-inventory's error output an error carries.
const maxListingStderr = 2048

// unparsedIsFailed makes ansible-inventory exit with an error when any source it was given is one
// no plugin parsed, or a plugin failed on. Without it a source Ansible cannot read is a warning and
// the listing goes on without that source's hosts, and a strict constructed expression that fails
// exits 0 with every host unfiltered.
const unparsedIsFailed = "ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=true"

// ListInventory returns the ansible-inventory --list JSON for every source read together, narrowed
// to the hosts limit matches when one is given. Every source must parse, so an input Ansible cannot
// read, or a constructed expression that does not evaluate, is an error and not a listing that
// quietly lacks it. A failure carries the head and the tail of what Ansible printed, because that
// is the only place the cause is explained, and the line naming the host and variable comes early.
func (a *ansibleRunner) ListInventory(ctx context.Context, sources []string, limit string) ([]byte, error) {
	if len(sources) == 0 {
		return nil, ErrNoInventory
	}
	if err := run.CheckLimitFile(limit); err != nil {
		return nil, err
	}
	args := make([]string, 0, 2*len(sources)+3)
	for _, s := range sources {
		args = append(args, "-i", s)
	}
	args = append(args, "--list")
	if limit != "" {
		args = append(args, "--limit", limit)
	}
	bin, release, err := a.command(ctx, defaultInventoryBinary)
	if err != nil {
		return nil, err
	}
	defer release()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(append([]string{}, a.baseEnv...), unparsedIsFailed)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %w: %s", ErrLaunch, err, headAndTail(string(exit.Stderr),
				maxListingStderr))
		}
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	return out, nil
}

// headAndTail returns s trimmed, cut to the first and last half of limit bytes with a marker
// between them when it is longer, each cut moved back to a whole UTF-8 character.
func headAndTail(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	half := limit / 2
	head, tail := s[:half], s[len(s)-half:]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	for len(tail) > 0 && !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	return head + " [...] " + tail
}

// Dump returns the raw ansible-inventory --list JSON for a source, with env layered over the base
// environment so cloud plugins see their credentials.
func (a *ansibleRunner) Dump(ctx context.Context, source string, env []string) ([]byte, error) {
	if source == "" {
		return nil, ErrNoInventory
	}
	bin, release, err := a.command(ctx, defaultInventoryBinary)
	if err != nil {
		return nil, err
	}
	defer release()
	cmd := exec.CommandContext(ctx, bin, "-i", source, "--list")
	cmd.Env = append(append([]string{}, a.baseEnv...), env...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	return out, nil
}

// Hosts returns the sorted set of hosts in the inventory by invoking ansible-inventory, narrowed to
// the hosts limit matches when one is given.
func (a *ansibleRunner) Hosts(ctx context.Context, inventory, limit string) ([]string, error) {
	if inventory == "" {
		return nil, ErrNoInventory
	}
	if err := run.CheckLimitFile(limit); err != nil {
		return nil, err
	}

	args := []string{"-i", inventory, "--list"}
	if limit != "" {
		args = append(args, "--limit", limit)
	}
	bin, release, err := a.command(ctx, defaultInventoryBinary)
	if err != nil {
		return nil, err
	}
	defer release()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = a.baseEnv
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	return parseInventoryHosts(out)
}

// parseInventoryHosts reads the host names out of an ansible-inventory --list document.
//
// The names come from the groups, not from _meta.hostvars. Ansible only writes a host into hostvars
// when that host HAS variables, so an ordinary inventory whose hosts carry none produces an empty
// hostvars and every host still listed under its group. Reading hostvars alone therefore enumerated
// nothing for the most common inventory there is, which left a sharded submit believing the fleet
// held fewer than two hosts and quietly running it unsharded instead, reporting success. hostvars is
// still unioned in, because a dynamic inventory plugin may name a host there that belongs to no
// group.
func parseInventoryHosts(out []byte) ([]string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInventoryParse, err)
	}

	seen := make(map[string]bool)
	for name, raw := range doc {
		if name == "_meta" {
			var meta struct {
				HostVars map[string]json.RawMessage `json:"hostvars"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				return nil, fmt.Errorf("%w: _meta: %w", ErrInventoryParse, err)
			}
			for host := range meta.HostVars {
				seen[host] = true
			}
			continue
		}
		var group struct {
			Hosts []string `json:"hosts"`
		}
		// A group may carry only children, and "all" usually does, so a shape without hosts is
		// ordinary rather than an error.
		if err := json.Unmarshal(raw, &group); err != nil {
			continue
		}
		for _, host := range group.Hosts {
			seen[host] = true
		}
	}

	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts, nil
}
