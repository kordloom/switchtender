package wallcheck

import "errors"

// Sentinel errors for fingerprint generation and checking.
var (
	// ErrPresent is returned by Gen when a distinctive paid unit already exists in the public tree.
	ErrPresent = errors.New("paid code already present in public tree")
	// ErrParse is returned when a paid Go file cannot be parsed, since its units would go unhashed.
	ErrParse = errors.New("paid go file parse")
	// ErrList is returned when a fingerprint list line is malformed.
	ErrList = errors.New("fingerprint list")
	// ErrWalk is returned when a tree cannot be read.
	ErrWalk = errors.New("tree walk")
)
