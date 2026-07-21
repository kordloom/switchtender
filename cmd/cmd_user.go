package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kordloom/switchtender/internal/user"
)

// userDB holds the value of the user --db flag.
var userDB string

// userRole holds the value of the user new --role flag.
var userRole string

// userCmd groups account management.
var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage accounts. Sign in through the UI or POST /auth/login.",
}

// userNewCmd creates an account. The password comes from SWITCHTENDER_PASSWORD or an interactive
// prompt, never from arguments, so it stays out of shell history and process listings.
var userNewCmd = &cobra.Command{
	Use:   "new <username>",
	Short: "Create an account. Password from SWITCHTENDER_PASSWORD or a prompt.",
	Args:  cobra.ExactArgs(1),
	RunE:  runUserNew,
}

// userListCmd lists accounts.
var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List accounts.",
	RunE:  runUserList,
}

// userDeleteCmd removes an account by id.
var userDeleteCmd = &cobra.Command{
	Use:   "delete <user-id>",
	Short: "Delete an account. Its tokens stop working on next use.",
	Args:  cobra.ExactArgs(1),
	RunE:  runUserDelete,
}

// init registers the user commands and flags.
func init() {
	userCmd.PersistentFlags().StringVar(&userDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend.")
	userNewCmd.Flags().StringVar(&userRole, "role", string(user.RoleOperator),
		"Role: admin, operator, or viewer.")
	userCmd.AddCommand(userNewCmd, userListCmd, userDeleteCmd)
	rootCmd.AddCommand(userCmd)
}

// readPassword takes the password from the environment or prompts on the terminal.
func readPassword() (string, error) {
	if pw := os.Getenv("SWITCHTENDER_PASSWORD"); pw != "" {
		return pw, nil
	}
	if !term.IsTerminal(int(syscall.Stdin)) {
		return "", errors.New("no terminal: set SWITCHTENDER_PASSWORD")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	raw, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	pw := strings.TrimSpace(string(raw))
	if pw == "" {
		return "", errors.New("empty password")
	}
	return pw, nil
}

// runUserNew creates and stores an account.
func runUserNew(cmd *cobra.Command, args []string) error {
	bundle, err := openBundle(userDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	users := bundle.Users()

	if _, err := users.FindByUsername(cmd.Context(), args[0]); err == nil {
		return fmt.Errorf("username %q already exists", args[0])
	}
	password, err := readPassword()
	if err != nil {
		return err
	}
	u, err := user.New(args[0], password, user.Role(userRole))
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	if err := users.Save(cmd.Context(), u); err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	return printJSON(map[string]string{"id": u.ID, "username": u.Username, "role": string(u.Role)})
}

// runUserList prints all accounts without password material.
func runUserList(cmd *cobra.Command, _ []string) error {
	bundle, err := openBundle(userDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	list, err := bundle.Users().List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	return printJSON(list)
}

// runUserDelete removes the account with the given id.
func runUserDelete(cmd *cobra.Command, args []string) error {
	bundle, err := openBundle(userDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	if err := bundle.Users().Delete(cmd.Context(), args[0]); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return printJSON(map[string]string{"deleted": args[0]})
}
