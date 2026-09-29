package main

import "errors"

// The failures below are the ones that make a check meaningless rather than red, so each is a named
// value a reader can match on and a phase can report by the same name every time. They are about
// the harness having nothing to assert on, never about the product having answered wrongly: a
// product failure is a finding, and one of these is the harness saying it was never in a position
// to look.
var (
	// ErrNoID is returned when a create answers successfully and names no object. The empty string
	// then travels into a run as the inventory or credential it was supposed to name.
	ErrNoID = errors.New("created and no id came back")
	// ErrNoToken is returned when a token mint answers successfully and carries no token. The
	// request builders attach Authorization only for a non-empty token, so the actor holding one
	// is an anonymous caller wearing a name.
	ErrNoToken = errors.New("minted and no token came back")
	// ErrNoCredential is returned when a refusal would be attempted with no credential at all. A
	// refusal says something about a caller's role only if the caller had one.
	ErrNoCredential = errors.New("attempted with no credential")
	// ErrUnreachable is returned when a read never arrived, which is not the same as a read that
	// was refused and must never be counted as one.
	ErrUnreachable = errors.New("the install was never reached")
	// ErrEmptyChain is returned when an audit chain verifies and holds nothing. An empty chain
	// verifies, so the verdict alone says only that nothing is broken.
	ErrEmptyChain = errors.New("the chain verifies and holds nothing")
	// ErrNothingRead is returned when a property was asked of nothing: no view answered, no row
	// came back, no receipt was written. The claim would otherwise hold over an empty set.
	ErrNothingRead = errors.New("nothing was read, so the property was never asked")
)
