package dispatch

import "errors"

var (
	// ErrNoPlaybook is returned when a run is submitted without a playbook path.
	ErrNoPlaybook = errors.New("no playbook")
	// ErrNoHostLister is returned when a split is requested but the runner cannot list hosts.
	ErrNoHostLister = errors.New("host listing unavailable")
	// ErrNoSteps is returned when a pipeline is submitted with no steps.
	ErrNoSteps = errors.New("no steps")
	// ErrNotSplit is returned when a shard retry targets a run that is not a split parent.
	ErrNotSplit = errors.New("not a split run")
	// ErrNotFinished is returned when a shard retry targets a run that has not finished.
	ErrNotFinished = errors.New("run not finished")
	// ErrNoFailedShards is returned when a shard retry finds nothing to retry.
	ErrNoFailedShards = errors.New("no failed shards")
	// ErrUnnamedStep is returned when a dependency declaring pipeline has a step without a name.
	ErrUnnamedStep = errors.New("step missing name")
	// ErrDuplicateStep is returned when two pipeline steps share a name.
	ErrDuplicateStep = errors.New("duplicate step name")
	// ErrUnknownDependency is returned when a step depends on a name no step carries.
	ErrUnknownDependency = errors.New("unknown dependency")
	// ErrDependencyCycle is returned when pipeline dependencies form a cycle.
	ErrDependencyCycle = errors.New("dependency cycle")
)
