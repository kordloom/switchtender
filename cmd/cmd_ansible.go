package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/jsonutil"
)

var (
	// ansibleDB holds the ansible --db flag, which locates the data directory the runtime lives in.
	ansibleDB string
	// ansibleDir holds the ansible --dir flag, the managed runtime's directory.
	ansibleDir string
	// ansibleVersion holds the --version flag of ansible install, remove, and lock.
	ansibleVersion string
	// ansiblePython holds the ansible install --python flag.
	ansiblePython string
	// ansibleWheels holds the ansible install --wheels flag.
	ansibleWheels string
	// ansibleIndexURL holds the ansible install --index-url flag.
	ansibleIndexURL string
	// ansiblePretty holds the ansible --pretty flag.
	ansiblePretty bool
)

// ansibleCmd groups the managed Ansible runtime's commands.
var ansibleCmd = &cobra.Command{
	Use:   "ansible",
	Short: "Install, list, and remove the managed Ansible runtime runs use.",
	Long: "Manage the managed Ansible runtime: a Python virtual environment under the server's data " +
		"directory\nholding one pinned ansible-core release, installed with pip in hash-checking " +
		"mode from a\nlock this binary carries. Runs, inventory resolution, and doctor use it when " +
		"no --ansible-bin\nis configured. A python3 the release supports must be installed.",
	Args: cobra.NoArgs,
	RunE: runGroupHelp,
}

// ansibleInstallCmd installs a managed runtime and makes it the one in use.
var ansibleInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install a pinned ansible-core into the managed runtime and make it the one runs use.",
	Long: "Install a pinned ansible-core into the managed runtime and make it the one runs use.\n\n" +
		"With no --version it installs the newest supported release. Every file pip installs must " +
		"match\na SHA-256 in the release's lock, and a mismatch fails the install with nothing " +
		"changed. Running\nit again for an installed release verifies it and makes it the one in use, " +
		"reinstalling nothing.\n--wheels installs " +
		"offline from a directory of wheels, checked against the same hashes.",
	Args: cobra.NoArgs,
	RunE: runAnsibleInstall,
}

// ansibleListCmd prints the installed managed runtimes.
var ansibleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the installed managed runtimes and which one runs use.",
	Args:  cobra.NoArgs,
	RunE:  runAnsibleList,
}

// ansibleRemoveCmd removes managed runtimes.
var ansibleRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove a managed runtime, or all of them when no --version is given.",
	Args:  cobra.NoArgs,
	RunE:  runAnsibleRemove,
}

// ansibleLockCmd prints the lock a release installs from, for preparing an offline install.
var ansibleLockCmd = &cobra.Command{
	Use:   "lock",
	Short: "Print the hash-pinned requirements lock a release installs from.",
	Long: "Print the hash-pinned requirements lock a release installs from.\n\n" +
		"On a machine with network access, pip download --require-hashes --only-binary=:all: -r " +
		"<lock>\nfills a wheel directory that ansible install --wheels then installs from offline.",
	Args: cobra.NoArgs,
	RunE: runAnsibleLock,
}

// init registers the ansible commands and their flags.
func init() {
	ansibleCmd.PersistentFlags().StringVar(&ansibleDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, of the install whose data directory holds the "+
			"runtime. "+dbEnvVar+" sets it when this flag is absent.")
	ansibleCmd.PersistentFlags().StringVar(&ansibleDir, "dir", "",
		"Managed runtime directory. Defaults to "+ansibleruntime.RootEnv+", then ansible/ in "+
			"the data directory.")
	ansibleCmd.PersistentFlags().BoolVar(&ansiblePretty, "pretty", false,
		"Indent the JSON output.")
	for _, c := range []*cobra.Command{ansibleInstallCmd, ansibleRemoveCmd, ansibleLockCmd} {
		c.Flags().StringVar(&ansibleVersion, "version", "",
			"The ansible-core release, such as 2.21.4, or minor version, such as 2.21. Empty means "+
				"the newest supported release for install and lock, and every release for remove.")
	}
	ansibleInstallCmd.Flags().StringVar(&ansiblePython, "python", "",
		"Python interpreter to build the runtime with. Defaults to python3, then python3.N, "+
			"on PATH, within the release's supported range.")
	ansibleInstallCmd.Flags().StringVar(&ansibleWheels, "wheels", "",
		"Directory of wheels to install from instead of the package index, for an offline install.")
	ansibleInstallCmd.Flags().StringVar(&ansibleIndexURL, "index-url", "",
		"Package index to download from instead of PyPI, such as an internal mirror. pip's own "+
			"environment variables and configuration files are not read.")
	ansibleCmd.AddCommand(ansibleInstallCmd, ansibleListCmd, ansibleRemoveCmd, ansibleLockCmd)
	rootCmd.AddCommand(ansibleCmd)
}

// ansibleDataDir returns the server's data directory for the database db: the directory a SQLite
// file is in, and for a PostgreSQL DSN, which has no directory, switchtender in this account's
// configuration directory.
func ansibleDataDir(db string) (string, error) {
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		base, err := os.UserConfigDir()
		if err != nil || base == "" {
			return "", fmt.Errorf("this account has no configuration directory to keep the "+
				"Ansible runtime in: set %s or --dir to a durable path", ansibleruntime.RootEnv)
		}
		return filepath.Join(base, "switchtender"), nil
	}
	return filepath.Dir(db), nil
}

// ansibleRoot returns the managed runtime's absolute directory for a command: --dir, then the
// environment, then ansible/ in the data directory of the database the command is pointed at.
func ansibleRoot(dirFlag, db string) (string, error) {
	data := ""
	if strings.TrimSpace(dirFlag) == "" && strings.TrimSpace(os.Getenv(ansibleruntime.RootEnv)) == "" {
		var err error
		if data, err = ansibleDataDir(db); err != nil {
			return "", err
		}
	}
	return filepath.Abs(ansibleruntime.DataDirRoot(dirFlag, data))
}

// ansibleInstallOutput is what ansible install prints on success.
type ansibleInstallOutput struct {
	// Runtime is the installed runtime.
	*ansibleruntime.Runtime
	// AlreadyInstalled reports that the release was installed already and only verified.
	AlreadyInstalled bool `json:"already_installed"`
}

// runAnsibleInstall installs the requested release and prints the result.
func runAnsibleInstall(cmd *cobra.Command, _ []string) error {
	root, err := ansibleRoot(ansibleDir, dbFromEnv(cmd, ansibleDB))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res, err := ansibleruntime.Install(ctx, ansibleruntime.InstallOptions{
		Root: root, Version: ansibleVersion, Python: ansiblePython, Wheels: ansibleWheels,
		IndexURL: ansibleIndexURL, Out: os.Stderr,
	})
	if err != nil {
		return err
	}
	verb := "Installed"
	if res.AlreadyInstalled {
		verb = "Already installed and verified:"
	}
	fmt.Fprintf(os.Stderr, "%s ansible-core %s in %s.\nServers and workers whose runtime "+
		"directory is %s run it unless --ansible-bin names another Ansible.\n", verb,
		res.Runtime.Release, res.Runtime.Dir, root)
	return printAnsibleJSON(ansibleInstallOutput{Runtime: res.Runtime,
		AlreadyInstalled: res.AlreadyInstalled})
}

// ansibleListOutput is what ansible list prints.
type ansibleListOutput struct {
	// RuntimeDir is the managed runtime's directory.
	RuntimeDir string `json:"runtime_dir"`
	// Runtimes are the installed runtimes, newest first.
	Runtimes []*ansibleruntime.Runtime `json:"runtimes"`
}

// runAnsibleList prints the installed runtimes.
func runAnsibleList(cmd *cobra.Command, _ []string) error {
	root, err := ansibleRoot(ansibleDir, dbFromEnv(cmd, ansibleDB))
	if err != nil {
		return err
	}
	list, err := ansibleruntime.List(root)
	if err != nil {
		return err
	}
	if list == nil {
		list = []*ansibleruntime.Runtime{}
	}
	return printAnsibleJSON(ansibleListOutput{RuntimeDir: root, Runtimes: list})
}

// runAnsibleRemove removes the requested runtimes and prints what it removed.
func runAnsibleRemove(cmd *cobra.Command, _ []string) error {
	root, err := ansibleRoot(ansibleDir, dbFromEnv(cmd, ansibleDB))
	if err != nil {
		return err
	}
	removed, err := ansibleruntime.Remove(root, ansibleVersion)
	if err != nil {
		return err
	}
	if removed == nil {
		removed = []string{}
	}
	return printAnsibleJSON(map[string][]string{"removed": removed})
}

// runAnsibleLock prints the lock for the requested release.
func runAnsibleLock(cmd *cobra.Command, _ []string) error {
	lock, err := ansibleruntime.LockFor(ansibleVersion)
	if err != nil {
		return err
	}
	_, err = cmd.OutOrStdout().Write(lock.Content)
	return err
}

// printAnsibleJSON writes v to standard output as JSON, indented with --pretty.
func printAnsibleJSON(v any) error {
	data, err := jsonutil.Marshal(v, ansiblePretty)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// ansibleBinFlag holds the --ansible-bin flag shared by serve and worker.
var ansibleBinFlag string

// ansibleRuntimeDirFlag holds the --ansible-runtime-dir flag shared by serve and worker.
var ansibleRuntimeDirFlag string

// registerAnsibleFlags adds the flags that choose the Ansible a server or worker runs, shared by
// serve and worker so both choose it the same way.
func registerAnsibleFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&ansibleBinFlag, "ansible-bin", os.Getenv(ansibleruntime.BinEnv),
		"Directory holding ansible-playbook and ansible-inventory to run, or \"system\" for the ones "+
			"on PATH. Empty uses the managed runtime when one is installed, then PATH. "+
			ansibleruntime.BinEnv+" sets it.")
	cmd.Flags().StringVar(&ansibleRuntimeDirFlag, "ansible-runtime-dir", "",
		"Managed Ansible runtime directory. Defaults to "+ansibleruntime.RootEnv+", then ansible/ "+
			"in the data directory.")
}

// ansibleLocator returns the locator for the flags registerAnsibleFlags added, with the managed
// runtime under the data directory of db. A data directory that cannot be worked out leaves the
// managed runtime out, so the configured directory and PATH still apply.
func ansibleLocator(db string) *ansibleruntime.Locator {
	root, err := ansibleRoot(ansibleRuntimeDirFlag, db)
	if err != nil {
		root = ""
	}
	return ansibleruntime.NewLocator(ansibleBinFlag, root)
}
