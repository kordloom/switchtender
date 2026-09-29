package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/jsonutil"
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

// tokenCmd groups API token management.
var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage API tokens. Creating the first token turns authentication on.",
}

// tokenNewCmd mints a token and prints it once. Without --user the token is unscoped and acts as
// admin; bound to an account it carries that account's role, which is what an automation or an AI
// agent should hold.
var tokenNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create an API token and print it. The value is shown only this once.",
	Long: "Create an API token and print it. The value is shown only this once.\n\n" +
		"Without --user the token is unscoped and acts as admin. With --user it is bound to that\n" +
		"account and carries the account's role, so an automation or an AI agent given an\n" +
		"operator-bound token can submit runs but cannot approve them or change configuration.",
	RunE: runTokenNew,
}

// tokenListCmd lists tokens without their secrets.
var tokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List API tokens.",
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
	tokenNewCmd.Flags().StringVar(&tokenUser, "user", "",
		"Bind the token to this account by username. The token carries the account's role "+
			"instead of acting as admin.")
	tokenCmd.AddCommand(tokenNewCmd, tokenListCmd, tokenRevokeCmd)
}

// openTokens opens the token store for the --db value.
func openTokens(db string) (auth.Store, audit.Store, func() error, error) {
	bundle, err := openBundle(db)
	if err != nil {
		return nil, nil, nil, err
	}
	return bundle.Tokens(), bundle.Audits(), bundle.Close, nil
}

// printJSON writes v as JSON to stdout, indented when --pretty is set.
func printJSON(v any) error {
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
	bundle, err := openBundle(tokenDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	if err := recordCLI(cmd.Context(), bundle.Audits(), "/cli/token/new"); err != nil {
		return err
	}
	plain, tok, err := auth.New(tokenName)
	if err != nil {
		return fmt.Errorf("mint token: %w", err)
	}
	out := map[string]string{"id": tok.ID, "name": tok.Name}
	if tokenUser != "" {
		u, err := bundle.Users().FindByUsername(cmd.Context(), tokenUser)
		if err != nil {
			return fmt.Errorf("bind token: no account named %q: %w", tokenUser, err)
		}
		tok.UserID = u.ID
		out["user"] = u.Username
		out["role"] = string(u.Role)
	}
	if tokenTTL > 0 {
		expires := time.Now().Add(tokenTTL)
		tok.ExpiresAt = &expires
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

	if err := recordCLI(cmd.Context(), audits, "/cli/token/revoke"); err != nil {
		return err
	}
	if err := tokens.Delete(cmd.Context(), args[0]); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return printJSON(map[string]string{"revoked": args[0]})
}
