// Package wallcheck fingerprints the private paid module so the public tree can prove, on every CI
// run and without the paid source, that none of that code has been copied into it.
//
// A fingerprint is a truncated SHA-256 over a domain prefix, a kind, and a normalized body. Go
// declarations are hashed as token streams with comments dropped, once exactly and once with every
// identifier renamed in order of appearance, so reformatting or renaming a copy does not hide it.
// Every file of any kind is also hashed whole and in shingles of consecutive significant lines, so
// a copied page, script, or a file split across two others is caught too. The committed list holds
// only hashes: it reveals how much paid code exists and nothing of what it says.
package wallcheck

import (
	"crypto/sha256"
	"encoding/hex"
)

// Kind names what a fingerprint was taken over.
type Kind string

// Fingerprint kinds. The kind is part of the hashed input, so equal bodies of different kinds never
// collide.
const (
	// KindExact is a declaration's token stream.
	KindExact Kind = "exact"
	// KindAlpha is a declaration's token stream with identifiers renamed by first appearance.
	KindAlpha Kind = "alpha"
	// KindFile is a whole file's significant lines, or its raw bytes when it is binary.
	KindFile Kind = "file"
	// KindShingle is a run of consecutive significant lines.
	KindShingle Kind = "shingle"
)

// domain prefixes every hashed body so these hashes cannot be confused with any other SHA-256 use.
const domain = "switchtender-wall-v1"

// hashLen is the number of hex characters kept from each digest.
const hashLen = 16

// ListPath is where the public repository keeps the committed list, relative to its root.
const ListPath = "internal/wallcheck/paid-fingerprints.txt"

// Config sets the floors and exclusions the scans use.
type Config struct {
	// ExactFloor is the fewest tokens a declaration needs before its exact hash is taken.
	ExactFloor int
	// AlphaFloor is the fewest tokens a declaration needs before its renamed hash is taken.
	AlphaFloor int
	// PresentFloor is the fewest tokens a non-test paid declaration needs before finding it already
	// public fails Gen instead of being dropped from the list.
	PresentFloor int
	// FileFloor is the fewest significant lines a text file needs before its whole-file hash is taken.
	FileFloor int
	// ShingleLines is the number of consecutive significant lines in one shingle.
	ShingleLines int
	// ShingleHits is the number of distinct shingle matches in one public file that makes a finding.
	ShingleHits int
	// Exclude lists slash paths, relative to the public root, that public scans skip.
	Exclude []string
}

// DefaultConfig returns the floors the committed list is generated and checked with. The public
// scan skips the paid submodule mount, which holds the paid code itself when checked out, and the
// list file.
func DefaultConfig() Config {
	return Config{
		ExactFloor:   40,
		AlphaFloor:   80,
		PresentFloor: 80,
		FileFloor:    6,
		ShingleLines: 12,
		ShingleHits:  3,
		Exclude:      []string{"internal/paid/impl", ListPath},
	}
}

// Fingerprint is one hashed unit of source and where it came from.
type Fingerprint struct {
	// Hash is the truncated hex digest.
	Hash string
	// Kind is what the hash was taken over.
	Kind Kind
	// Path is the slash path of the file, relative to the scanned root.
	Path string
	// Line is the first line of the unit.
	Line int
	// Decl is func, type, or value for a declaration, and empty for file and shingle kinds.
	Decl string
	// Tokens is the declaration's token count, and zero for file and shingle kinds.
	Tokens int
	// Test reports whether the file is a test file.
	Test bool
}

// hash returns the truncated SHA-256 of the domain prefix, the kind, and the body.
func hash(kind Kind, body []byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))[:hashLen]
}
