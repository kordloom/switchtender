package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/server"
)

// The provisioning callback and fact cache flags of serve.
var (
	// serveCallbackRate is how many provisioning callbacks one client address may make a minute.
	serveCallbackRate int
	// serveCallbackKeyFailures is how many wrong host config keys one client address may present a
	// minute.
	serveCallbackKeyFailures int
	// serveFactCacheAdminOnly restricts reading cached facts to admins.
	serveFactCacheAdminOnly bool
)

// registerCallbackFlags adds the provisioning callback and fact cache flags to cmd.
func registerCallbackFlags(cmd *cobra.Command) {
	cmd.Flags().IntVar(&serveCallbackRate, "callback-rate-limit", server.DefaultCallbackRateLimit,
		"Provisioning callbacks one client address may make in a minute, on the native and the "+
			"AWX-compatible callback address together. Hosts behind one NAT address share it.")
	cmd.Flags().IntVar(&serveCallbackKeyFailures, "callback-key-failure-limit",
		server.DefaultCallbackKeyFailureLimit,
		"Wrong host config keys one client address may present in a minute before its callbacks "+
			"are refused for the rest of that minute.")
	cmd.Flags().BoolVar(&serveFactCacheAdminOnly, "fact-cache-admin-only", false,
		"Restrict reading cached facts to admins. By default an operator with read on the "+
			"inventory and on the gathering run may read them, with secret-looking values masked.")
}

// checkCallbackFlags refuses a callback limit below one, which would refuse every callback rather
// than bound them.
func checkCallbackFlags() error {
	if serveCallbackRate < 1 {
		return fmt.Errorf("%w: --callback-rate-limit must be at least 1, got %d", ErrUsage,
			serveCallbackRate)
	}
	if serveCallbackKeyFailures < 1 {
		return fmt.Errorf("%w: --callback-key-failure-limit must be at least 1, got %d", ErrUsage,
			serveCallbackKeyFailures)
	}
	return nil
}

// callbackServeOption carries the provisioning callback and fact cache flags onto the server, with
// limits as what checks a calling host against a template's own limit.
func callbackServeOption(limits server.CallbackLimitMatcher) server.Option {
	return func(s *server.Server) {
		for _, opt := range []server.Option{
			server.WithCallbackRateLimits(serveCallbackRate, serveCallbackKeyFailures),
			server.WithCallbackLimitMatcher(limits),
			server.WithFactCacheAdminOnly(serveFactCacheAdminOnly),
		} {
			opt(s)
		}
	}
}
