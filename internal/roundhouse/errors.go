package roundhouse

import "errors"

var (
	// ErrNoPlaybook is returned when a Spec has no playbook path.
	ErrNoPlaybook = errors.New("no playbook")
	// ErrNoInventory is returned when host enumeration is requested without an inventory.
	ErrNoInventory = errors.New("no inventory")
	// ErrLaunch is returned when the executor could not start or supervise the process.
	ErrLaunch = errors.New("launch error")
	// ErrInventoryParse is returned when ansible-inventory output cannot be parsed.
	ErrInventoryParse = errors.New("inventory parse")
	// ErrNoImage is returned when a container execution is requested without an image reference.
	ErrNoImage = errors.New("no image")
	// ErrContainerDisabled is returned when a run needs a container image but container execution
	// environments are not enabled on this executor.
	ErrContainerDisabled = errors.New("container execution environments disabled: start with --allow-container-ee")
)
