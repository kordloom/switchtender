package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/jsonutil"
	"github.com/kordloom/switchtender/internal/user"
)

// tokenDB holds the value of the token --db flag.
var tokenDB string

// tokenName holds the value of the token new --name flag.
var tokenName string

// tokenPretty holds the value of the token --pretty flag.
var tokenPretty bool

// tokenTTL holds the value of the token new --ttl flag.
var tokenTTL time.Duration

// tokenUser holds the value of the token new --user flag.
var tokenUser string

// tokenAgent holds the value of the token new --agent flag.
var tokenAgent bool

// tokenCmd groups API token management.
var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage API tokens. Creating the first token turns authentication on.",
	Args:  cobra.NoArgs,
	RunE:  runGroupHelp,
}

// tokenNewCmd mints a token and prints it once. Without --user the token is unscoped and acts as
// admin; bound to an account it carries that account's role, which is what an automation or an AI
// agent should hold.
//
// The account is named by --user and never by a positional argument, so a bare word is refused
// rather than dropped. "token new alice" reads as binding the token to alice; with no validator the
// word was discarded and the token minted unscoped, so the operator handed out administrator on the
// control plane believing they had handed out an operator-bound credential, and nothing in the
// output said otherwise.
var tokenNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create an API token and print it. The value is shown only this once.",
	Long: "Create an API token and print it. The value is shown only this once.\n\n" +
		"Without --user the token is unscoped and acts as admin. With --user it is bound to that\n" +
		"account and carries the account's role, so an automation or an AI agent given an\n" +
		"operator-bound token can submit runs but cannot approve them or change configuration.",
	Args: cobra.NoArgs,
	RunE: runTokenNew,
}

// tokenListCmd lists tokens without their secrets.
var tokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List API tokens.",
	Args:  cobra.NoArgs,
	RunE:  runTokenList,
}

// tokenRevokeCmd deletes a token by id.
var tokenRevokeCmd = &cobra.Command{
	Use:   "revoke <token-id>",
	Short: "Revoke an API token.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTokenRevoke,
}

// init registers the token commands and flags.
func init() {
	tokenCmd.PersistentFlags().StringVar(&tokenDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend.")
	tokenCmd.PersistentFlags().BoolVar(&tokenPretty, "pretty", false, "Indent JSON output.")
	tokenNewCmd.Flags().StringVar(&tokenName, "name", "", "Label for the token, for example ci.")
	tokenNewCmd.Flags().DurationVar(&tokenTTL, "ttl", 0,
		"Lifetime, for example 720h. Zero means the token never expires. A negative value is refused.")
	tokenNewCmd.Flags().BoolVar(&tokenAgent, "agent", false,
		"Mint the token for an AI agent. Its actions are recorded under the agent identity in the "+
			"chain, and it is capped so it cannot manage identity, access, or secrets, or approve its "+
			"own held runs. Requires --user so the human it acts for is recorded.")
	tokenNewCmd.Flags().StringVar(&tokenUser, "user", "",
		"Bind the token to this account by username. The token carries the account's role "+
			"instead of acting as admin.")
	tokenCmd.AddCommand(tokenNewCmd, tokenListCmd, tokenRevokeCmd)
}

// openTokens opens the token store for the --db value.
func openTokens(db string) (auth.Store, audit.Store, func() error, error) {
	bundle, err := openExisting(db)
	if err != nil {
		return nil, nil, nil, err
	}
	return bundle.Tokens(), bundle.Audits(), bundle.Close, nil
}

// tokenChange is what a token entry's content digest commits to: which token was minted or revoked,
// for whom, and what it can do, never its secret.
type tokenChange struct {
	// ID is the token's id.
	ID string `json:"id"`
	// Name is the token's label, set when one is minted.
	Name string `json:"name,omitempty"`
	// Kind is agent for an agent token, empty for a person's.
	Kind string `json:"kind,omitempty"`
	// UserID is the account the token is bound to, empty for an unbound admin token.
	UserID string `json:"user_id,omitempty"`
	// Role is the bound account's role at the time the token was minted.
	Role string `json:"role,omitempty"`
	// ExpiresAt is when the token stops working, nil for one that never expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// recordTokenChange records a token change from the command line with its summary committed.
func recordTokenChange(ctx context.Context, audits audit.Store, path string, change tokenChange) error {
	body, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("encode the token change: %w", err)
	}
	return recordCLIChange(ctx, audits, tokenDB, path, body)
}

// printJSON writes v as JSON to stdout, indented when --pretty is set. An empty list prints as [],
// not null: a store with no rows returns a nil slice, and a script reading the output with jq or a
// JSON parser met null where it expected an array.
func printJSON(v any) error {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		v = []any{}
	}
	data, err := jsonutil.Marshal(v, tokenPretty)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// runTokenNew mints and stores a token, printing the plaintext exactly once. With --user the
// token is bound to that account and carries its role; without it the token is unscoped admin.
//
// A negative --ttl is refused before anything is minted. Only a positive lifetime set an expiry, so
// a mistyped duration produced the opposite of what was asked for: an operator who meant to hand
// out a short-lived credential handed out one that never expires, and nothing in the output said so.
func runTokenNew(cmd *cobra.Command, _ []string) error {
	if tokenTTL < 0 {
		return fmt.Errorf("%w: --ttl %s is negative: pass a positive lifetime, "+
			"or zero for a token that never expires", ErrUsage, tokenTTL)
	}
	bundle, err := openExisting(tokenDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	// An agent token must name the human it acts for, or the chain records an action by an agent on
	// behalf of nobody, which is exactly the accountability the agent identity exists to provide. An
	// unbound token is also an admin token, and an admin agent is a contradiction.
	if tokenAgent && tokenUser == "" {
		return fmt.Errorf("an agent token must be bound to an account with --user, so the chain " +
			"records who it acts for")
	}
	plain, tok, err := auth.New(tokenName)
	if err != nil {
		return fmt.Errorf("mint token: %w", err)
	}
	if tokenAgent {
		tok.Kind = auth.KindAgent
	}
	// The host account behind the command line minted it, which is the issuer an agent-initiated
	// run's evidence names as having provisioned the agent.
	tok.CreatedBy, tok.CreatedByType = cliActor(), actorTypeCLI
	out := map[string]string{"id": tok.ID, "name": tok.Name}
	if tokenAgent {
		out["kind"] = auth.KindAgent
	}
	if tokenUser != "" {
		u, err := bundle.Users().FindByUsername(cmd.Context(), tokenUser)
		if err != nil {
			return fmt.Errorf("bind token: no account named %q: %w", tokenUser, err)
		}
		tok.UserID = u.ID
		out["user"] = u.Username
		// The role printed and recorded is the one the token acts with. An agent token runs capped
		// below admin whatever its account holds, and printing the account's admin role told the
		// operator the agent could approve, mint tokens, and manage users, all of which it cannot.
		role := u.Role
		if tokenAgent {
			role = user.AgentRole(role)
		}
		out["role"] = string(role)
	}
	if tokenTTL > 0 {
		expires := time.Now().Add(tokenTTL)
		tok.ExpiresAt = &expires
	}
	// Recorded once the token exists in memory and before it is saved, so the entry commits to which
	// token was minted, for whom, with what role and lifetime. It used to be recorded before any of
	// that was known, and the chain showed that a token was minted without saying which.
	if err := recordTokenChange(cmd.Context(), bundle.Audits(), "/cli/token/new", tokenChange{
		ID: tok.ID, Name: tok.Name, Kind: tok.Kind, UserID: tok.UserID, Role: out["role"],
		ExpiresAt: tok.ExpiresAt,
	}); err != nil {
		return err
	}
	if err := bundle.Tokens().Save(cmd.Context(), tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	out["token"] = plain
	return printJSON(out)
}

// runTokenList prints all tokens without secret material.
func runTokenList(cmd *cobra.Command, _ []string) error {
	tokens, _, closeStores, err := openTokens(tokenDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = closeStores() }()

	list, err := tokens.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	return printJSON(list)
}

// runTokenRevoke deletes the token with the given id.
func runTokenRevoke(cmd *cobra.Command, args []string) error {
	tokens, audits, closeStores, err := openTokens(tokenDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = closeStores() }()

	if err := recordTokenChange(cmd.Context(), audits, "/cli/token/revoke",
		tokenChange{ID: args[0]}); err != nil {
		return err
	}
	if err := tokens.Delete(cmd.Context(), args[0]); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return printJSON(map[string]string{"revoked": args[0]})
}
