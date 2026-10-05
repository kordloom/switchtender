//go:build !unix

package roundhouse

import "os/exec"

// runSupervised runs cmd directly. The tool shim that stops a tool whose executor died is built on
// Unix process groups and pipes, so here a tool outlives an executor killed outright, as the
// reliability page says.
func runSupervised(cmd *exec.Cmd, _ [][]string) error {
	return cmd.Run()
}

// configureContainerClient leaves a container client as it is, killed on cancel by its context.
func configureContainerClient(*exec.Cmd) {}
