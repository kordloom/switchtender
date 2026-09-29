package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/user"
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

// initCmd sets up a fresh SwitchTender server: an encryption key and salt, a first admin account, and a
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
	initCmd.Flags().StringVar(&initConfig, "config", "switchtender.env", "Environment file to write.")
	initCmd.Flags().StringVar(&initAddr, "addr", defaultServeAddr,
		"Address the generated systemd unit starts the server on. Loopback by default, matching "+
			"serve. Set 0.0.0.0:8080 to expose it on the network, behind a proxy that terminates TLS.")
	initCmd.Flags().StringVar(&initAdmin, "admin", "admin", "Username for the first admin account.")
	initCmd.Flags().StringVar(&initSystemd, "systemd", "", "Path to write a systemd unit to, empty to skip.")
	initCmd.Flags().BoolVar(&initForce, "force", false,
		"Rewrite an existing config file, keeping the encryption key and salt it already holds so "+
			"the credentials sealed under them still open. Delete the file instead to start over "+
			"with new keys, which orphans every stored secret and every earlier backup.")
}

// runInit performs the one-time setup.
func runInit(cmd *cobra.Command, _ []string) error {
	existing, haveConfig := readInitConfig(initConfig)
	if haveConfig && !initForce {
		return fmt.Errorf("%s already exists, use --force to overwrite", initConfig)
	}

	// A --force rerun keeps the key and salt the install is already using.
	//
	// Minting new ones looked like regenerating a config file and was not: every credential, content
	// source secret, and trigger secret in the database is sealed under the old key, and so is every
	// backup taken before now. Nothing can open them again, there is no rotation path, and the only
	// warning was a flag help line reading "Overwrite an existing config file". An operator who reran
	// init to regenerate a systemd unit, which is the ordinary reason to pass --force, destroyed
	// access to every secret in a live install. Deleting the file is still the way to ask for new
	// keys, and it is a deliberate act rather than a side effect of a flag.
	key, salt := existing.key, existing.salt
	kept := key != "" && salt != ""
	if !kept {
		var err error
		if key, err = randomHex(32); err != nil {
			return fmt.Errorf("generate key: %w", err)
		}
		if salt, err = randomHex(16); err != nil {
			return fmt.Errorf("generate salt: %w", err)
		}
	}
	var err error
	password := os.Getenv("SWITCHTENDER_ADMIN_PASSWORD")
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
	// The first administrator is local, and is exactly the account a misconfigured username claim would otherwise be handed.
	admin.Source = "local"
	if err := recordCLI(cmd.Context(), bundle.Audits(), "/cli/init/admin"); err != nil {
		return err
	}
	if err := bundle.Users().Save(cmd.Context(), admin); err != nil {
		return fmt.Errorf("save admin: %w", err)
	}

	env := "SWITCHTENDER_ENCRYPTION_KEY=" + key + "\nSWITCHTENDER_ENCRYPTION_SALT=" + salt + "\n"
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

	if kept {
		fmt.Fprintf(os.Stderr, "Kept the encryption key and salt already in %s, so the credentials "+
			"sealed under them still open. Delete that file first if you meant to start over.\n",
			initConfig)
	}
	printInitSummary(generated, password)
	return nil
}

// initConfigValues are the secrets an existing config file already holds.
type initConfigValues struct {
	// key is the stored encryption key, empty when the file has none.
	key string
	// salt is the stored encryption salt, empty when the file has none.
	salt string
}

// readInitConfig reads the key and salt out of an existing environment file, reporting whether the
// file was there at all. A file that cannot be read is treated as present but empty, so a rerun
// refuses rather than quietly minting keys over one it could not inspect.
func readInitConfig(path string) (initConfigValues, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return initConfigValues{}, !os.IsNotExist(err)
	}
	var out initConfigValues
	for _, line := range strings.Split(string(raw), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch name {
		case "SWITCHTENDER_ENCRYPTION_KEY":
			out.key = value
		case "SWITCHTENDER_ENCRYPTION_SALT":
			out.salt = value
		}
	}
	return out, true
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
Description=SwitchTender
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=%s
ExecStart=/usr/local/bin/switchtender serve --db %s --addr %s
Restart=on-failure
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, configPath, db, addr)
}

// printInitSummary writes the setup result and the next steps to stderr, keeping stdout clean. The
// admin password is shown once, since it is not stored in the clear.
func printInitSummary(generated bool, password string) {
	fmt.Fprintln(os.Stderr, "SwitchTender is set up.")
	fmt.Fprintln(os.Stderr, "  admin account: "+initAdmin)
	if generated {
		fmt.Fprintln(os.Stderr, "  admin password: "+password+"  (shown once, save it now)")
	}
	fmt.Fprintln(os.Stderr, "  config file:   "+initConfig+"  (holds the encryption key, keep it safe)")
	fmt.Fprintln(os.Stderr, "  database:      "+initDB)
	if initSystemd != "" {
		fmt.Fprintln(os.Stderr, "  systemd unit:  "+initSystemd)
		fmt.Fprintln(os.Stderr, "\nStart with systemd:")
		fmt.Fprintln(os.Stderr, "  sudo cp "+initSystemd+" /etc/systemd/system/switchtender.service")
		fmt.Fprintln(os.Stderr, "  sudo systemctl enable --now switchtender")
		return
	}
	fmt.Fprintln(os.Stderr, "\nStart the server:")
	fmt.Fprintln(os.Stderr, "  set -a; . ./"+initConfig+"; set +a")
	fmt.Fprintln(os.Stderr, "  switchtender serve --db "+initDB+" --addr "+initAddr)
}
