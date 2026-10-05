//go:build !unix

package dispatch

import "errors"

// makeFIFO reports that this platform has no named pipes in its filesystem, so a test that needs
// one skips.
func makeFIFO(string) error {
	return errors.New("named pipes in the filesystem are a Unix feature")
}
