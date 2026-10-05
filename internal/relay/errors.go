package relay

import "errors"

var (
	// ErrDeliveryRefused is returned on a worker when the control node declined to send a run's
	// secrets with its claim, and carries the control node's reason.
	ErrDeliveryRefused = errors.New("the control node did not deliver this run's secrets")
	// ErrDeliveryReplay is returned when a sealed delivery this worker already opened is presented
	// again. A delivery opens once, for the claim it was sealed for.
	ErrDeliveryReplay = errors.New("this sealed delivery was already opened once")
	// ErrNoDeliveryKey is returned when a delivery arrives at a worker that holds no delivery key.
	ErrNoDeliveryKey = errors.New("this worker holds no delivery key")
	// ErrPlanFileTooLarge is returned when a plan file is larger than a relay worker can hand the
	// control node, MaxPlanFileBytes.
	ErrPlanFileTooLarge = errors.New("the plan file is too large to cross the relay")
)
