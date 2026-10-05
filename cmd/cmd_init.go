package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/util"
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
	Args: cobra.NoArgs,
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
	bundle, err := openBundle(initDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	// A rerun finds the admin account the first run made. Creating it again failed on the unique
	// username, so init could not be rerun at all: not with --force to regenerate a systemd unit,
	// which is what the flag is for, and not after deleting the config to start over with new keys,
	// which is what its help says to do. The account is left as it is, password included.
	adminExists, err := accountExists(cmd.Context(), bundle.Users(), initAdmin)
	if err != nil {
		return err
	}
	password := os.Getenv("SWITCHTENDER_ADMIN_PASSWORD")
	generated := password == "" && !adminExists
	if generated {
		if password, err = randomHex(12); err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
	}
	if !adminExists {
		admin, err := user.New(initAdmin, password, user.RoleAdmin)
		if err != nil {
			return fmt.Errorf("build admin: %w", err)
		}
		// The first administrator is local, and is exactly the account a misconfigured username claim would otherwise be handed.
		admin.Source = "local"
		if err := recordCLI(cmd.Context(), bundle.Audits(), initDB, "/cli/init/admin"); err != nil {
			return err
		}
		if err := bundle.Users().Save(cmd.Context(), admin); err != nil {
			return fmt.Errorf("save admin: %w", err)
		}
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
		dbPath, err := filepath.Abs(initDB)
		if err != nil {
			return fmt.Errorf("resolve database path: %w", err)
		}
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve this binary's path for the unit: %w", err)
		}
		account, group, err := unitAccount()
		if err != nil {
			return err
		}
		unit := systemdUnit(unitSpec{DB: dbPath, Addr: initAddr, Config: cfgPath, Exe: exe,
			WorkDir: filepath.Dir(dbPath), User: account, Group: group})
		if err := os.WriteFile(initSystemd, []byte(unit), 0o644); err != nil {
			return fmt.Errorf("write systemd unit: %w", err)
		}
		if account == "root" {
			fmt.Fprintln(os.Stderr, rootUnitNote)
		}
	}

	if kept {
		fmt.Fprintf(os.Stderr, "Kept the encryption key and salt already in %s, so the credentials "+
			"sealed under them still open. Delete that file first if you meant to start over.\n",
			initConfig)
	}
	if adminExists {
		fmt.Fprintf(os.Stderr, "The admin account %q already exists, so it was left as it is, password "+
			"included.\n", initAdmin)
	}
	printInitSummary(generated, password)
	return nil
}

// accountExists reports whether the store already holds an account with the given username.
func accountExists(ctx context.Context, users user.Store, username string) (bool, error) {
	_, err := users.FindByUsername(ctx, username)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, user.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("look up the admin account: %w", err)
	}
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

// rootUnitNote is what init says when it writes a unit as root, since that unit then runs the
// server, and every run it executes, as root.
const rootUnitNote = `Note: init ran as root, so the unit runs the server as root, and every run it
executes runs as root too. To keep runs off root, create an account for it and run init as that
account instead:
  sudo useradd --system --create-home --home-dir /var/lib/switchtender switchtender
  sudo -u switchtender -H switchtender init --db /var/lib/switchtender/switchtender.db \
    --config /var/lib/switchtender/switchtender.env \
    --systemd /var/lib/switchtender/switchtender.service`

// unitSpec is what a generated systemd unit starts, and as which account.
type unitSpec struct {
	// DB is the database the server opens, as an absolute path.
	DB string
	// Addr is the address the server listens on.
	Addr string
	// Config is the environment file holding the encryption key and salt, as an absolute path.
	Config string
	// Exe is the binary the unit runs, as an absolute path.
	Exe string
	// WorkDir is the directory the service runs in.
	WorkDir string
	// User is the account the service runs as.
	User string
	// Group is the group the service runs as.
	Group string
}

// systemdUnit renders a systemd service that starts the server with the config file's secrets and
// the given database and address, as the account that owns them.
//
// Every path here is absolute and every one of them was a defect. ExecStart hardcoded
// /usr/local/bin/switchtender, which the published install script does not use when that directory
// is not writable, so the unit failed 203/EXEC for any non-root install. The database was passed
// through as written, and systemd runs a service with a working directory of /, so a relative --db
// resolved somewhere the operator never chose. Both together produced the worst version: a service
// that started against a database that did not exist, created it, and stood up a new empty chain
// with no admin account and authentication off, in place of the one the operator had just set up.
//
// The account is named for the same reason. A system unit without User= runs as root, and a run with
// no execution image runs as the server's own account, so every Bash, Python, and local Ansible run
// the server executed ran as root, and the server wrote root-owned files into the directory init had
// just set up as somebody else.
//
// The runtime directory is where runs stage their keys and tokens: memory-backed, private, and
// removed by systemd when the service stops for any reason, a crash included. LimitCORE=0 keeps a
// crash from writing process memory, credentials included, to disk, and KillMode=mixed lets the
// server end its runs' tools itself on a stop rather than having them killed under it.
func systemdUnit(u unitSpec) string {
	return fmt.Sprintf(`[Unit]
Description=SwitchTender
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
EnvironmentFile=%s
WorkingDirectory=%s
ExecStart=%s serve --db %s --addr %s
Restart=on-failure
NoNewPrivileges=true
PrivateTmp=true
RuntimeDirectory=switchtender
RuntimeDirectoryMode=0700
RuntimeDirectoryPreserve=no
LimitCORE=0
KillMode=mixed
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
`, u.User, u.Group, u.Config, u.WorkDir, u.Exe, u.DB, u.Addr)
}

// unitAccount returns the account and group running init, which own every file init writes and so
// are the ones the generated unit runs as.
func unitAccount() (name, group string, err error) {
	u, err := osuser.Current()
	if err != nil {
		return "", "", fmt.Errorf("resolve the account running init: %w", err)
	}
	g, err := osuser.LookupGroupId(u.Gid)
	if err != nil {
		return "", "", fmt.Errorf("resolve the group of %s: %w", u.Username, err)
	}
	return u.Username, g.Name, nil
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
	fmt.Fprintln(os.Stderr, "  set -a; . "+util.ShellQuote(sourceablePath(initConfig))+"; set +a")
	fmt.Fprintln(os.Stderr, "  switchtender serve --db "+util.ShellQuote(initDB)+" --addr "+initAddr)
}

// sourceablePath returns a path the shell's dot command reads as a file rather than searching PATH
// for. A bare name needs ./ in front of it. A path that already holds a slash does not, and an
// absolute one given ./ became .//etc/switchtender.env, which exists nowhere, so the printed start
// command failed and the server it started had no encryption key.
func sourceablePath(p string) string {
	if strings.Contains(p, "/") {
		return p
	}
	return "./" + p
}
