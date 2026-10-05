//go:build !unix

package roundhouse

import "io/fs"

// ownedBySelf reports false, since this platform describes ownership with no uid to compare, so no
// path is exempt from the mount guard here.
func ownedBySelf(fs.FileInfo) bool { return false }
