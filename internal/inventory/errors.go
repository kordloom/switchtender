package inventory

import "errors"

var (
	// ErrNotFound is returned when an inventory does not exist in the store.
	ErrNotFound = errors.New("inventory not found")
	// ErrHostFilter is returned when a smart inventory's host filter cannot be read.
	ErrHostFilter = errors.New("invalid host filter")
	// ErrSourceVars is returned when a constructed inventory's plugin options cannot be used.
	ErrSourceVars = errors.New("invalid constructed inventory options")
	// ErrComposition is returned when a composed inventory's definition is inconsistent, such as a
	// constructed inventory with no inputs or a static inventory carrying a host filter.
	ErrComposition = errors.New("invalid composed inventory")
	// ErrNoHosts is returned when a composed inventory resolves to no host at launch, which would
	// otherwise start a run that reaches nothing and reports that it ran.
	ErrNoHosts = errors.New("composed inventory resolved to no hosts")
	// ErrResolve is returned when a composed inventory cannot be resolved, such as when Ansible
	// cannot read an input or evaluate the constructed options.
	ErrResolve = errors.New("composed inventory could not be resolved")
	// ErrListing is returned when an ansible-inventory listing cannot be read.
	ErrListing = errors.New("invalid inventory listing")
	// ErrNeedsAnsible is returned when an inventory's definition can only be resolved by Ansible,
	// such as a dynamic source, a vault-encrypted value, or a constructed inventory, and Ansible is
	// not installed where it is needed.
	ErrNeedsAnsible = errors.New("this inventory needs Ansible")
	// ErrInvalidInventory is returned when an inventory document is one Ansible would not read as
	// written: neither its YAML plugin nor its INI plugin accepts it whole.
	ErrInvalidInventory = errors.New("invalid inventory document")
	// ErrDisagreement is returned when Ansible reads an inventory differently from the native
	// engine, which refuses the run rather than execute against hosts or variables nobody resolved.
	ErrDisagreement = errors.New("the native engine and Ansible read this inventory differently")
	// errAliasExpansion marks a YAML document whose aliases expand to more values than its size can
	// justify, a decompression bomb. It is refused rather than handed to Ansible, which does not
	// bound alias expansion and hangs on the same document.
	errAliasExpansion = errors.New("yaml aliases expand too far")
)
