package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
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

// ListInventory returns the ansible-inventory --list JSON for every source read together, narrowed
// to the hosts limit matches when one is given. A failure carries the tail of what Ansible printed,
// because an unreadable input or a constructed expression that does not evaluate is only ever
// explained there.
func (a *ansibleRunner) ListInventory(ctx context.Context, sources []string, limit string) ([]byte, error) {
	if len(sources) == 0 {
		return nil, ErrNoInventory
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
	cmd.Env = a.baseEnv
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			tail := strings.TrimSpace(string(exit.Stderr))
			if len(tail) > maxListingStderr {
				tail = tail[len(tail)-maxListingStderr:]
			}
			return nil, fmt.Errorf("%w: %w: %s", ErrLaunch, err, tail)
		}
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}
	return out, nil
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
