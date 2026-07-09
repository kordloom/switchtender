package roundhouse

import "errors"

var (
	// ErrNoPlaybook is returned when a Spec has no playbook path.
	ErrNoPlaybook = errors.New("no playbook")
	// ErrNoCommand is returned when a bash, terraform, or python Spec has no command.
	ErrNoCommand = errors.New("no command")
	// ErrUnknownTool is returned when a Spec names an execution tool the runner does not support.
	ErrUnknownTool = errors.New("unknown execution tool")
	// ErrBadWorkDir is returned when a tool's working directory escapes its project checkout.
	ErrBadWorkDir = errors.New("working directory escapes the project")
	// ErrNoInventory is returned when host enumeration is requested without an inventory.
	ErrNoInventory = errors.New("no inventory")
	// ErrLaunch is returned when the executor could not start or supervise the process.
	ErrLaunch = errors.New("launch error")
	// ErrInventoryParse is returned when ansible-inventory output cannot be parsed.
	ErrInventoryParse = errors.New("inventory parse")
	// ErrNoImage is returned when a container execution is requested without an image reference.
	ErrNoImage = errors.New("no image")
	// ErrBadImage is returned when a container image reference is malformed or unsafe to pass to the
	// container CLI.
	ErrBadImage = errors.New("bad image reference")
	// ErrForbiddenMount is returned when a run would bind mount a sensitive host path, such as the
	// filesystem root, a system directory, or the docker socket, into a container.
	ErrForbiddenMount = errors.New("forbidden mount path")
	// ErrContainerDisabled is returned when a run needs a container image but container execution
	// environments are not enabled on this executor.
	ErrContainerDisabled = errors.New("container execution environments disabled: start with --allow-container-ee")
)
