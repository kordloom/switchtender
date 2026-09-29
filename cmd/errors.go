package cmd

import "errors"

// ErrUsage is returned when CLI arguments or flags are invalid.
var ErrUsage = errors.New("invalid usage")

// errNoIdentityHome is returned when there is nowhere durable to keep the producer signing
// identity, so the install would otherwise mint one somewhere a restart empties.
var errNoIdentityHome = errors.New("no durable home for the signing identity")

// errPublishedKey is returned when the encryption key or salt is a value the documentation printed,
// so anyone holding a copy of the database could open every credential sealed under it.
var errPublishedKey = errors.New("the encryption key or salt is a published example value")

// errBadAuditKey is returned when SWITCHTENDER_AUDIT_KEY is set but is not a usable signing seed.
var errBadAuditKey = errors.New("SWITCHTENDER_AUDIT_KEY is not a usable signing seed")

// errNoDatabase is returned when a command that works on an existing install is pointed at a SQLite
// file that does not exist.
var errNoDatabase = errors.New("no database")

// errContainerBuild is returned when a binary built into the container image is asked to verify
// against the release archive's hashes, which it cannot match by construction.
var errContainerBuild = errors.New("this binary was built into the container image")

// errLostIdentity is returned when the audit chain is bound to an install whose signing key is not
// where this process looks for it, so creating one would silently start a second install.
var errLostIdentity = errors.New("this install's signing key is missing")

// errForeignIdentity is returned when the signing key found names a different install than the one
// the audit chain is bound to.
var errForeignIdentity = errors.New("the signing key belongs to a different install than this chain")

// errEmptyPin is returned when a pin flag was given with no value, so the check it asked for would
// silently not happen.
var errEmptyPin = errors.New("an empty key pin was given")
