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

// HostLister enumerates the hosts in an inventory so a run can be split across them.
type HostLister interface {
	Hosts(ctx context.Context, inventory string) ([]string, error)
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

// Hosts returns the sorted set of hosts in the inventory by invoking ansible-inventory.
func (a *ansibleRunner) Hosts(ctx context.Context, inventory string) ([]string, error) {
	if inventory == "" {
		return nil, ErrNoInventory
	}

	cmd := exec.CommandContext(ctx, defaultInventoryBinary, "-i", inventory, "--list")
	cmd.Env = a.baseEnv
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunch, err)
	}

	var doc struct {
		Meta struct {
			HostVars map[string]json.RawMessage `json:"hostvars"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInventoryParse, err)
	}

	hosts := make([]string, 0, len(doc.Meta.HostVars))
	for host := range doc.Meta.HostVars {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts, nil
}
