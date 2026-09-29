package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/mcp"
)

var (
	// mcpServer is the SwitchTender API base URL the agent's calls are made against.
	mcpServer string
	// mcpToken is the operator-bound API token, preferred from the environment.
	mcpToken string
	// mcpTimeout bounds one API call.
	mcpTimeout time.Duration
	// mcpAllowAdhoc exposes the ad-hoc run tool alongside template launches.
	mcpAllowAdhoc bool
	// mcpAllowAdminToken skips the refusal to run on an admin token.
	mcpAllowAdminToken bool
)

// mcpTokenEnv is the environment variable the token is read from when the flag is absent. An
// environment variable is preferred to a flag because a flag value is visible in the process list to
// every other user on the host.
//
// An agent is given its own variable rather than the operator's. The two are different principals:
// an agent acts on somebody's behalf under a token whose grants and audit identity are its own, so
// reusing whatever token happens to be exported in a shell would silently run an agent with a
// person's authority. The shared variable is still read when the agent-specific one is unset, so an
// existing setup keeps working.
const mcpTokenEnv = "SWITCHTENDER_MCP_TOKEN"

// mcpFallbackTokenEnv is the shared token variable, read when the agent-specific one is unset.
const mcpFallbackTokenEnv = "SWITCHTENDER_TOKEN"

// mcpCmd serves the Model Context Protocol over stdio.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve the Model Context Protocol so an AI agent proposes changes through the gate.",
	Long: `Serve the Model Context Protocol over stdio.

An agent that speaks MCP connects to this command and can list job templates, propose a run, and read
what happened, including the run's evidence dossier. It cannot do anything else.

Every tool call is an ordinary authenticated API request carrying the token given here, so it passes
the same authorization, the same approval policy, and the same fail-closed audit append as a request
from a person. A proposed run is written into the tamper-evident chain, under the agent's own account,
before it executes. Where an approval policy covers the run, it is held until a person releases it.

There is deliberately no approve tool, so an agent cannot release its own work however it is prompted,
and no credential, account, token, grant, or policy tool, so it cannot widen its own reach. Give the
agent an operator-bound token, minted with "switchtender token new --user", and it holds exactly one
credential whose only door is this gate. The command refuses to start on an admin token.

The token is read from ` + mcpTokenEnv + `, falling back to ` + mcpFallbackTokenEnv + `. Give the
agent its own token rather than reusing an operator's: the trail names whoever the token belongs to.
An environment variable is preferred to the flag because a flag value is
visible in the host's process list.

    export ` + mcpTokenEnv + `=st_...
    switchtender mcp --server https://switchtender.internal`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runMCP,
}

// init registers the mcp command and its flags.
func init() {
	mcpCmd.Flags().StringVar(&mcpServer, "server", "", "SwitchTender API base URL. Required.")
	mcpCmd.Flags().StringVar(&mcpToken, "token", "",
		"API token. Prefer "+mcpTokenEnv+", since a flag is visible in the process list.")
	mcpCmd.Flags().DurationVar(&mcpTimeout, "timeout", 60*time.Second, "Bounds one API call.")
	mcpCmd.Flags().BoolVar(&mcpAllowAdhoc, "allow-adhoc", false,
		"Also expose the ad-hoc run tool, letting the agent compose a run rather than launch a "+
			"template an operator defined. Approval policy still applies.")
	mcpCmd.Flags().BoolVar(&mcpAllowAdminToken, "allow-admin-token", false,
		"Start even when the token has admin rights. An agent should hold an operator-bound token; "+
			"this exists for a local trial, not for production.")
	rootCmd.AddCommand(mcpCmd)
}

// runMCP serves the protocol on standard input and output until the client disconnects.
func runMCP(cmd *cobra.Command, _ []string) error {
	token := mcpToken
	if token == "" {
		token = os.Getenv(mcpTokenEnv)
		if token == "" {
			token = os.Getenv(mcpFallbackTokenEnv)
		}
	}
	client, err := mcp.NewClient(mcpServer, token, mcpTimeout)
	if err != nil {
		return err
	}

	// The signal context is established before the authority check so an interrupt during a slow or
	// unreachable server still exits rather than hanging on the probe.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// An agent holding an admin token could approve its own runs and rewrite the policies meant to
	// gate it, which would make the whole arrangement theater. The check is a real request, so it
	// also proves the server is reachable and the token is accepted before any agent connects.
	if err := client.RefuseAdminToken(ctx); err != nil {
		if errors.Is(err, mcp.ErrAdminToken) && !mcpAllowAdminToken {
			return fmt.Errorf("refusing to serve an agent on an admin token: mint an operator-bound " +
				"token with \"switchtender token new --user <account>\", or pass --allow-admin-token " +
				"for a local trial")
		}
		if !errors.Is(err, mcp.ErrAdminToken) {
			return err
		}
		fmt.Fprintln(os.Stderr, "mcp: warning: serving an agent on an admin token, which can approve "+
			"its own runs; use an operator-bound token instead")
	}

	tools := mcp.Tools(client, mcp.Options{AllowAdhoc: mcpAllowAdhoc})
	// Progress goes to standard error: standard output carries the protocol, so a stray line there
	// would corrupt the stream the client is parsing.
	fmt.Fprintf(os.Stderr, "mcp: serving %d tool(s) against %s\n", len(tools), mcpServer)
	srv := mcp.NewServer("switchtender", Version, tools)
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}
