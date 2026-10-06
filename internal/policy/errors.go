package policy

import "errors"

// ErrNotFound is returned when a policy id does not exist.
var ErrNotFound = errors.New("policy not found")

// ErrReadOnly is returned when a policy change is attempted against a source that does not accept
// them, which is what a file-backed policy set is: the file is the source of truth, so a change
// belongs in a diff rather than in an API call that would appear to succeed and do nothing.
var ErrReadOnly = errors.New("policies are read-only")

// ErrUnreachable is returned by a policy store that has no way to read the policies, so a caller
// can tell "there are no policies" apart from "I cannot see them".
var ErrUnreachable = errors.New("approval policies are not reachable from this process")

// ErrRego is returned when a Rego policy cannot be compiled or cannot decide about a run. A Rego
// policy that cannot decide is read as a refusal, never as a pass.
var ErrRego = errors.New("rego policy")

// ErrExemptionAccount is returned when an exemption names an agent's token label without the
// account the token is bound to. A label is not unique across accounts, so an exemption keyed on it
// alone also covers a token minted for another account under the same name.
var ErrExemptionAccount = errors.New("an exemption that names an actor must also name its account")

// ErrRegoNotStored is returned by a database-backed store asked to save a Rego policy. A Rego policy
// is read from the policy file, and a store that accepted one would keep its name and drop its
// modules, leaving a rule with no criteria that holds every run.
var ErrRegoNotStored = errors.New("a rego policy is read from the policy file and is never stored")
