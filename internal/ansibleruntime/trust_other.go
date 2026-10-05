//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package ansibleruntime

import "fmt"

// checkChain refuses every runtime directory where the owner and mode checks a runtime is trusted
// by are not built, which is Windows, where ansible-core does not run as a control node.
func checkChain(dir string) error {
	return fmt.Errorf("%w: %s: a managed runtime is not trusted on this platform", ErrUntrusted,
		dir)
}

// checkFile refuses every runtime file on this platform, for the reason checkChain gives.
func checkFile(path string) error {
	return fmt.Errorf("%w: %s: a managed runtime is not trusted on this platform", ErrUntrusted,
		path)
}

// checkTree refuses every environment on this platform, for the reason checkChain gives.
func checkTree(root string) error {
	return fmt.Errorf("%w: %s: a managed runtime is not trusted on this platform", ErrUntrusted,
		root)
}

// clearGroupOtherWrite does nothing on this platform, where install refuses before building.
func clearGroupOtherWrite(string) error {
	return nil
}

// holdEnv refuses on this platform, where no managed runtime is ever trusted.
func holdEnv(env string, _ bool) (func(), error) {
	return nil, fmt.Errorf("%w: %s: a managed runtime is not trusted on this platform",
		ErrUntrusted, env)
}
