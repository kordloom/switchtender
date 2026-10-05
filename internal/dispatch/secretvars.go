package dispatch

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// openSecretVars opens a run's sealed secret survey answers for this execution only and returns
// them keyed by variable name, or nil when the run carries none.
//
// A run that names secret answers it cannot open fails rather than executing without them. A tool
// handed an empty password does something nobody chose, so a relay worker, which never receives the
// sealed values, and an executor with no key both stop here with the reason.
//
// Each sealed value is held to the digest the run was created with before it is opened. The run's
// spec carries those digests, so an approval binds them, and checking the spec at execution proves
// only that the digests did not move. Without this the ciphertext beside them could: a sealed answer
// copied in from another run, under the same name, would open cleanly and reach the tool while
// everything the approver saw read the same.
func (d *Dispatcher) openSecretVars(r *run.Run) (map[string]string, error) {
	if len(r.SealedNames) == 0 && len(r.SealedVars) == 0 && len(r.SealedDigests) == 0 {
		return nil, nil
	}
	if d.sealer == nil || !d.sealer.Enabled() {
		return nil, fmt.Errorf("%w: this run carries secret survey answers and this executor holds "+
			"no encryption key to open them: %w", ErrSecretAnswer, credential.ErrNoKey)
	}
	names := r.SealedNames
	if len(names) == 0 {
		names = run.SealedVarNames(r.SealedVars)
	}
	// A bound answer whose sealed value is gone is missing, not optional. Reading the names alone
	// would let a row that lost an answer run without it.
	for _, digest := range r.SealedDigests {
		if !slices.Contains(names, digest.Var) {
			names = append(names, digest.Var)
		}
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		sealed, ok := r.SealedVars[name]
		if !ok {
			return nil, fmt.Errorf("%w: the answer to %q did not reach this executor. Secret "+
				"answers are opened only by an executor that reads the database and holds the "+
				"encryption key, the same rule credentials follow", ErrSecretAnswer, name)
		}
		if !r.SealedMatches(name, sealed) {
			return nil, fmt.Errorf("%w: the sealed answer to %q is not the one this run was created "+
				"with, so it is not opened: an approval of this run covers the answer it was "+
				"created with and no other", ErrSecretAnswer, name)
		}
		plain, err := d.sealer.Open(sealed)
		if err != nil {
			return nil, fmt.Errorf("%w: the answer to %q does not open with this server's "+
				"encryption key: %w", ErrSecretAnswer, name, err)
		}
		out[name] = plain
	}
	return out, nil
}

// varsWithSecrets returns the variables an execution receives: the run's extra vars with its opened
// secret answers merged over them. The run's own map is never written, so the answers stay out of
// anything that reads the run.
func varsWithSecrets(vars map[string]any, secrets map[string]string) map[string]any {
	if len(secrets) == 0 {
		return vars
	}
	out := make(map[string]any, len(vars)+len(secrets))
	maps.Copy(out, vars)
	for k, v := range secrets {
		out[k] = v
	}
	return out
}

// secretValues returns the opened answers as a list for the masker.
func secretValues(secrets map[string]string) []string {
	out := make([]string, 0, len(secrets))
	for _, v := range secrets {
		out = append(out, v)
	}
	return out
}
