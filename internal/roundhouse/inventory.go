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
