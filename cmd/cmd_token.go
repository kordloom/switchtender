package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dcadolph/railwarden/internal/auth"
	"github.com/dcadolph/railwarden/internal/jsonutil"
)

// tokenDB holds the value of the token --db flag.
var tokenDB string

// tokenName holds the value of the token new --name flag.
var tokenName string

// tokenPretty holds the value of the token --pretty flag.
var tokenPretty bool

// tokenTTL holds the value of the token new --ttl flag.
var tokenTTL time.Duration

// tokenCmd groups API token management.
var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage API tokens. Creating the first token turns authentication on.",
}

// tokenNewCmd mints a token and prints it once.
var tokenNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create an API token and print it. The value is shown only this once.",
	RunE:  runTokenNew,
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
		"Lifetime, for example 720h. Zero means the token never expires.")
	tokenCmd.AddCommand(tokenNewCmd, tokenListCmd, tokenRevokeCmd)
}

// openTokens opens the token store for the --db value.
func openTokens(db string) (auth.Store, func() error, error) {
	bundle, err := openBundle(db)
	if err != nil {
		return nil, nil, err
	}
	return bundle.Tokens(), bundle.Close, nil
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

// runTokenNew mints and stores a token, printing the plaintext exactly once.
func runTokenNew(cmd *cobra.Command, _ []string) error {
	tokens, closeStores, err := openTokens(tokenDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = closeStores() }()

	plain, tok, err := auth.New(tokenName)
	if err != nil {
		return fmt.Errorf("mint token: %w", err)
	}
	if tokenTTL > 0 {
		expires := time.Now().Add(tokenTTL)
		tok.ExpiresAt = &expires
	}
	if err := tokens.Save(cmd.Context(), tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	out := map[string]string{"id": tok.ID, "name": tok.Name, "token": plain}
	return printJSON(out)
}

// runTokenList prints all tokens without secret material.
func runTokenList(cmd *cobra.Command, _ []string) error {
	tokens, closeStores, err := openTokens(tokenDB)
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
	tokens, closeStores, err := openTokens(tokenDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = closeStores() }()

	if err := tokens.Delete(cmd.Context(), args[0]); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return printJSON(map[string]string{"revoked": args[0]})
}
