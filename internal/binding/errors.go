package binding

import "errors"

var (
	// ErrShortSeed is returned by DeriveKey for a seed shorter than the 32 bytes a key is derived
	// from, so a truncated or empty identity never yields a key a writer could guess.
	ErrShortSeed = errors.New("binding seed is shorter than 32 bytes")
	// ErrDerive is returned by DeriveKey when the key derivation itself fails.
	ErrDerive = errors.New("binding key derivation")
	// ErrSerialize is returned by an attempt to marshal a Key, which is never written anywhere.
	ErrSerialize = errors.New("binding key is never serialized")
	// ErrParent is returned by a record builder given a child whose ParentID does not name the
	// parent it was given, which is a caller's mistake rather than a state of the store.
	ErrParent = errors.New("run is not a child of the given parent")
	// ErrDrift means a row, its parent, or a chain entry it derives from no longer matches what the
	// process sealed. Its text starts with "refused", names the row, and says what moved.
	ErrDrift = errors.New("refused")
	// ErrUnverified means a store read the check depends on failed after its retries, so nothing
	// is known about the row either way. Its text never claims the row was changed.
	ErrUnverified = errors.New("could not verify")
)
