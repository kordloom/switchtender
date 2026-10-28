//go:build paid

package cmd

// The paid module registers every paid capability from its init. It lives in a private repository
// mounted at internal/paid/impl as a git submodule, so this import resolves only in a checkout that
// fetched it, and only under the paid build tag. A build without either is Community.
import _ "github.com/kordloom/switchtender/internal/paid/impl"
