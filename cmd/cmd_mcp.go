package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/mcp"
	"github.com/kordloom/switchtender/internal/util"
)

var (
	// mcpServer is the SwitchTender API base URL the agent's calls are made against.
	mcpServer string
	// mcpToken is the agent's API token, preferred from the environment.
	mcpToken string
	// mcpTimeout bounds one API call.
	mcpTimeout time.Duration
	// mcpAllowAdhoc exposes the ad-hoc run tool alongside template launches.
	mcpAllowAdhoc bool
)

// mcpTokenEnv is the environment variable the token is read from when the flag is absent. An
// environment variable is preferred to a flag because a flag value is visible in the process list to
// every other user on the host.
//
// An agent is given its own variable rather than the operator's. The two are different principals:
// an agent acts on somebody's behalf under a token whose grants and audit identity are its own, and
// the token an operator exports in a shell is a person's, which this command refuses. The shared
// variable is still read when the agent-specific one is unset, for a host that holds only the
// agent's token.
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
before it executes, and it waits until a person releases it unless a written policy exempts it.

There is deliberately no approve tool, so an agent cannot release its own work however it is prompted,
and no credential, account, token, grant, or policy tool, so it cannot widen its own reach. Give the
agent an operator-bound token, minted with "switchtender token new --user --agent", and it holds one
credential whose only door is this gate.

The command asks the server who the token is before anything is served, and it starts only for an
agent token, one minted with "switchtender token new --user <account> --agent". Any other token is
refused, an admin's and an operator's alike, because the hold on an agent's runs applies to an agent
token and to no other. A server with no accounts is refused too: nothing can be held there, so turn
accounts on first. No flag turns the check off. The refusal covers this command, and the server
cannot tell a person from a program holding that person's token.

The token is read from ` + mcpTokenEnv + `, falling back to ` + mcpFallbackTokenEnv + `. Give the
agent its own token rather than reusing an operator's: the trail names whoever the token belongs to.
An environment variable is preferred to the flag because a flag value is
visible in the host's process list.

    export ` + mcpTokenEnv + `=swt_...
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
	rootCmd.AddCommand(mcpCmd)
}

// runMCP serves the protocol on standard input and output until the client disconnects.
func runMCP(cmd *cobra.Command, _ []string) error {
	// Progress goes to standard error: standard output carries the protocol, so a stray line there
	// would corrupt the stream the client is parsing.
	return serveMCP(cmd.Context(), os.Stdin, os.Stdout, os.Stderr)
}

// serveMCP checks the token, then serves the protocol read from in and written to out until in
// ends. Progress lines go to progress and never to out.
func serveMCP(parent context.Context, in io.Reader, out, progress io.Writer) error {
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

	// The signal context is established before the token check so an interrupt during a slow or
	// unreachable server still exits rather than hanging on the probe.
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The hold on an agent's runs applies to a token the server records as an agent's, so an agent
	// served on any other token would run under a person's name with no hold. The check is a real
	// request, so it also proves the server is reachable and the token is accepted before any agent
	// connects.
	if err := agentTokenRefusal(client.RequireAgentToken(ctx)); err != nil {
		return err
	}

	tools := mcp.Tools(client, mcp.Options{AllowAdhoc: mcpAllowAdhoc})
	fmt.Fprintf(progress, "mcp: serving %d %s against %s\n", len(tools),
		util.Plural(len(tools), "tool", "tools"), mcpServer)
	srv := mcp.NewServer("switchtender", Version, tools)
	return srv.Serve(ctx, in, out)
}

// agentTokenRefusal turns the startup check's answer into the command's decision: nil for an agent
// token, one line saying what to do for a token that is not an agent's or a server with no
// accounts, and the check's own error when the check could not be made.
func agentTokenRefusal(err error) error {
	switch {
	case errors.Is(err, mcp.ErrNotAgentToken):
		return fmt.Errorf("refusing to serve: %w: mint one with "+
			"\"switchtender token new --user <account> --agent\"", err)
	case errors.Is(err, mcp.ErrNoAccounts):
		return fmt.Errorf("refusing to serve: %w, so nothing an agent asks for can be held "+
			"there: turn accounts on first with \"switchtender user new\"", err)
	}
	return err
}
