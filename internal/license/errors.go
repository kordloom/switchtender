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

// ErrRequiresTier marks a refusal for a feature this build does not carry: the paid code that would
// do it is not in the binary, so no license file can turn it on. The wrapped text is the line an
// unlicensed install prints, so a Community build and an unlicensed paid build refuse in one voice.
var ErrRequiresTier = errors.New("requires a paid tier")
