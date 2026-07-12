package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dcadolph/yardmaster/internal/user"
)

// init flag values.
var (
	// initDB is the SQLite database the server will use.
	initDB string
	// initConfig is the environment file init writes, holding the encryption key and salt.
	initConfig string
	// initAddr is the address the generated systemd unit starts the server on.
	initAddr string
	// initAdmin is the username of the first admin account.
	initAdmin string
	// initSystemd, when set, is the path to write a systemd unit to.
	initSystemd string
	// initForce overwrites an existing config file.
	initForce bool
)

// initCmd sets up a fresh Yardmaster server: an encryption key and salt, a first admin account, and a
// config file, so a new install is one command away from serving.
var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Set up a new server: encryption keys, an admin account, and a config file.",
	Long: "Init generates the encryption key and salt that seal credentials, creates the first admin " +
		"account, and writes an environment file the server reads. It can also write a systemd unit. " +
		"Run it once on a fresh install, then start the server.",
	RunE: runInit,
}

// init binds the init command's flags.
func init() {
	initCmd.Flags().StringVar(&initDB, "db", defaultDBPath, "SQLite database path.")
	initCmd.Flags().StringVar(&initConfig, "config", "yardmaster.env", "Environment file to write.")
	initCmd.Flags().StringVar(&initAddr, "addr", ":8080", "Address the server listens on.")
	initCmd.Flags().StringVar(&initAdmin, "admin", "admin", "Username for the first admin account.")
	initCmd.Flags().StringVar(&initSystemd, "systemd", "", "Path to write a systemd unit to, empty to skip.")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Overwrite an existing config file.")
}

// runInit performs the one-time setup.
func runInit(cmd *cobra.Command, _ []string) error {
	if _, err := os.Stat(initConfig); err == nil && !initForce {
		return fmt.Errorf("%s already exists, use --force to overwrite", initConfig)
	}

	key, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	salt, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	password := os.Getenv("YARDMASTER_ADMIN_PASSWORD")
	generated := password == ""
	if generated {
		if password, err = randomHex(12); err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
	}

	bundle, err := openBundle(initDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	admin, err := user.New(initAdmin, password, user.RoleAdmin)
	if err != nil {
		return fmt.Errorf("build admin: %w", err)
	}
	if err := bundle.Users().Save(cmd.Context(), admin); err != nil {
		return fmt.Errorf("save admin: %w", err)
	}

	env := "YARDMASTER_ENCRYPTION_KEY=" + key + "\nYARDMASTER_ENCRYPTION_SALT=" + salt + "\n"
	if err := os.WriteFile(initConfig, []byte(env), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	if initSystemd != "" {
		cfgPath, err := filepath.Abs(initConfig)
		if err != nil {
			return fmt.Errorf("resolve config path: %w", err)
		}
		if err := os.WriteFile(initSystemd, []byte(systemdUnit(initDB, initAddr, cfgPath)), 0o644); err != nil {
			return fmt.Errorf("write systemd unit: %w", err)
		}
	}

	printInitSummary(generated, password)
	return nil
}

// randomHex returns n random bytes as a hex string, so a key, salt, or password is unpredictable.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// systemdUnit renders a systemd service that starts the server with the config file's secrets and the
// given database and address.
func systemdUnit(db, addr, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Yardmaster
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=%s
ExecStart=/usr/local/bin/yardmaster serve --db %s --addr %s
Restart=on-failure
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, configPath, db, addr)
}

// printInitSummary writes the setup result and the next steps to stderr, keeping stdout clean. The
// admin password is shown once, since it is not stored in the clear.
func printInitSummary(generated bool, password string) {
	fmt.Fprintln(os.Stderr, "Yardmaster is set up.")
	fmt.Fprintln(os.Stderr, "  admin account: "+initAdmin)
	if generated {
		fmt.Fprintln(os.Stderr, "  admin password: "+password+"  (shown once, save it now)")
	}
	fmt.Fprintln(os.Stderr, "  config file:   "+initConfig+"  (holds the encryption key, keep it safe)")
	fmt.Fprintln(os.Stderr, "  database:      "+initDB)
	if initSystemd != "" {
		fmt.Fprintln(os.Stderr, "  systemd unit:  "+initSystemd)
		fmt.Fprintln(os.Stderr, "\nStart with systemd:")
		fmt.Fprintln(os.Stderr, "  sudo cp "+initSystemd+" /etc/systemd/system/yardmaster.service")
		fmt.Fprintln(os.Stderr, "  sudo systemctl enable --now yardmaster")
		return
	}
	fmt.Fprintln(os.Stderr, "\nStart the server:")
	fmt.Fprintln(os.Stderr, "  set -a; . ./"+initConfig+"; set +a")
	fmt.Fprintln(os.Stderr, "  yardmaster serve --db "+initDB+" --addr "+initAddr)
}
