package roundhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
)

// defaultInventoryBinary is the executable used to enumerate inventory hosts.
const defaultInventoryBinary = "ansible-inventory"

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

// Dump returns the raw ansible-inventory --list JSON for a source, with env layered over the base
// environment so cloud plugins see their credentials.
func (a *ansibleRunner) Dump(ctx context.Context, source string, env []string) ([]byte, error) {
	if source == "" {
		return nil, ErrNoInventory
	}
	cmd := exec.CommandContext(ctx, defaultInventoryBinary, "-i", source, "--list")
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
	cmd := exec.CommandContext(ctx, defaultInventoryBinary, args...)
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
