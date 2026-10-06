//go:build unix

package dispatch

import "syscall"

// makeFIFO creates a named pipe at path.
func makeFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
