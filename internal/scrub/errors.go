package scrub

import "errors"

// ErrEcho means an update sent a scrubbed value back in a form that cannot be matched to what is
// stored, so the update is refused rather than storing a mask over a real secret.
var ErrEcho = errors.New("scrubbed value echoed back")
