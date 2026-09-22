package license

import "errors"

// ErrLapsed marks a refusal whose cause is an expired term rather than an unlicensed install. The
// two read the same to a buyer and must not read the same in code: refusing something a customer
// never bought is the gate working, while refusing something they had running until midnight is
// taking away what they already had.
//
// The terms make that a contractual line rather than a preference. A lapsed license takes nothing,
// we do not reserve a right to disable the software, and we will not brick a running install over
// a billing dispute. Any gate that can stop an install from running has to tell the two apart and
// pick the side that keeps it running.
var ErrLapsed = errors.New("license term lapsed")
