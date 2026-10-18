package inventory

import (
	"errors"
	"fmt"
	"strings"
)

// MaxHosts bounds how many hosts an inventory may expand into. A range such as web[0000:9999] is
// one line that names ten thousand hosts, and nesting a few of them names more than any fleet
// holds, so the bound turns a pathological document into an error rather than into the memory it
// would take.
const MaxHosts = 100000

// ErrTooManyHosts is returned when an inventory names more hosts than MaxHosts. Its text names the
// bound, since every caller that answers with it hands the sentence to the person who wrote the
// inventory.
var ErrTooManyHosts = fmt.Errorf("inventory names more than %d hosts, the most one inventory may "+
	"name. Split it into several inventories", MaxHosts)

// Host is one host an inventory names, with the variables set on the host itself.
type Host struct {
	// Name is the inventory host name, the name Ansible addresses the host by in a limit.
	Name string `json:"name"`
	// Vars are the variables set on the host itself: on its host line or under its key, and the
	// ansible_port a host:port entry sets. Group variables are not folded in, which is how AWX
	// reads a host's own ansible_host.
	Vars map[string]any `json:"vars,omitempty"`
}

// Address returns the address Ansible connects to: ansible_host when the host sets it, otherwise
// the host name. AWX matches a provisioning callback against the same value.
func (h Host) Address() string {
	if v, ok := h.Vars["ansible_host"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return h.Name
}

// Hosts returns every host a static inventory document names, sorted by name, each with its own
// variables. The document is read by the native engine exactly as Ansible reads it: the YAML plugin
// first, which takes JSON too, then the INI plugin, with Ansible's host ranges, ports, and value
// typing. A document only Ansible can resolve, such as a plugin configuration, is an error rather
// than an empty host list, and so is the JSON ansible-inventory --list prints, which no Ansible
// plugin reads back as an inventory file.
func Hosts(content string) ([]Host, error) {
	return NativeHosts(content)
}

// CheckHostBound returns ErrTooManyHosts when content names more hosts than MaxHosts, and nil
// otherwise. It reads content with the native engine a run resolves hosts with, so an inventory it
// accepts is one a launch does not refuse for its size. Content only Ansible can resolve, and
// content Ansible would not read, return nil: their hosts are not known until a run resolves them,
// and saving them is not refused here.
func CheckHostBound(content string) error {
	if _, err := nativeData(content); errors.Is(err, ErrTooManyHosts) {
		return err
	}
	return nil
}
