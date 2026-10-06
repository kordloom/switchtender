package forgelink

import "errors"

var (
	// ErrNotFound is returned when no link matches.
	ErrNotFound = errors.New("forge account link not found")
	// ErrLinked is returned when the forge account is already linked to a SwitchTender account, or
	// the SwitchTender account already links an account on that forge.
	ErrLinked = errors.New("forge account already linked")
)
