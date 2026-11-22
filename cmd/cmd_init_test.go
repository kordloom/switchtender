package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/util"
)

// TestInitForceKeepsTheKeyTheInstallIsUsing pins that rerunning init over a live install does not
// destroy access to its secrets.
//
// A --force rerun minted a fresh key and salt. That reads as regenerating a config file and is not:
// every credential, content source secret, and trigger secret in the database is sealed under the old
// key, and so is every backup taken before the rerun. Nothing can open them again and there is no
// rotation path. Regenerating a systemd unit is the ordinary reason to pass --force, so the flag's
// most common use quietly destroyed the install.
func TestInitForceKeepsTheKeyTheInstallIsUsing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchtender.env")
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const salt = "fedcba9876543210fedcba9876543210"
	body := "SWITCHTENDER_ENCRYPTION_KEY=" + key + "\nSWITCHTENDER_ENCRYPTION_SALT=" + salt + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// Drive the real command, which is the path that mints. Reading the parser alone would pass
	// whether or not runInit actually keeps what it read.
	prevCfg, prevDB, prevForce, prevAdmin, prevSystemd := initConfig, initDB, initForce, initAdmin, initSystemd
	t.Cleanup(func() {
		initConfig, initDB, initForce, initAdmin, initSystemd = prevCfg, prevDB, prevForce, prevAdmin, prevSystemd
	})
	initConfig, initDB, initForce = cfg, filepath.Join(dir, "st.db"), true
	initAdmin, initSystemd = "admin", ""
	t.Setenv("SWITCHTENDER_ADMIN_PASSWORD", "a-known-password")

	cmd := initCmd
	cmd.SetContext(context.Background())
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("runInit(--force) error = %v", err)
	}

	after, present := readInitConfig(cfg)
	if !present {
		t.Fatal("the config vanished")
	}
	if after.key != key {
		t.Errorf("after --force the key is %q, want the original: every credential, content source "+
			"secret, and trigger secret sealed under the old key is now unopenable, and so is every "+
			"backup taken before this run", after.key)
	}
	if after.salt != salt {
		t.Errorf("after --force the salt is %q, want the original", after.salt)
	}

	// A directory with no config is the fresh install, where minting is correct.
	if _, present := readInitConfig(filepath.Join(dir, "absent.env")); present {
		t.Error("a missing config was reported present, which would refuse a fresh install")
	}
}

// TestInitRerunsOverAnInstallItAlreadyMade pins that init can run again over its own install. It
// created the admin account every time, and the second attempt failed on the unique username, so the
// two reruns its own help describes were impossible: --force to regenerate a systemd unit, and
// deleting the config to start over with new keys. A rerun leaves the account as it is, password
// included, since changing it would lock out whoever holds the first one.
func TestInitRerunsOverAnInstallItAlreadyMade(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchtender.env")
	db := filepath.Join(dir, "st.db")
	setString(t, &initConfig, cfg)
	setString(t, &initDB, db)
	setString(t, &initAdmin, "admin")
	setString(t, &initSystemd, "")
	setBool(t, &initForce, false)
	cmd := initCmd
	cmd.SetContext(context.Background())

	t.Setenv("SWITCHTENDER_ADMIN_PASSWORD", "the-first-password")
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("first runInit() error = %v", err)
	}

	// Rerun to regenerate what the config holds, which is what --force is for.
	t.Setenv("SWITCHTENDER_ADMIN_PASSWORD", "a-second-password")
	setBool(t, &initForce, true)
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("runInit(--force) over its own install error = %v", err)
	}

	// Rerun after deleting the config, which the --force help says is how to start over.
	if err := os.Remove(cfg); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	setBool(t, &initForce, false)
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("runInit() after deleting the config error = %v", err)
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	accounts, err := bundle.Users().List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("accounts after three runs = %d, want the one the first run made", len(accounts))
	}
	if _, err := user.Authenticate(context.Background(), bundle.Users(), "admin",
		"the-first-password"); err != nil {
		t.Errorf("the admin's first password no longer works after a rerun: %v", err)
	}
}

// TestInitSystemdAddrMatchesServeDefault pins that the generated unit does not bind every interface.
//
// serve defaults to loopback and says so; init defaulted to ":8080", so the unit it wrote for a first
// install listened on every interface. First start then mints an admin token and logs it, and on a
// unit started by systemd that lands in the journal, where anyone in adm or systemd-journal can read
// a plaintext non-expiring admin bearer token off a control plane that is already reachable.
func TestInitSystemdAddrMatchesServeDefault(t *testing.T) {
	flag := initCmd.Flags().Lookup("addr")
	if flag == nil {
		t.Fatal("init has no --addr flag")
	}
	if flag.DefValue != defaultServeAddr {
		t.Errorf("init --addr default = %q, want serve's %q: the generated unit otherwise binds "+
			"every interface on a fresh install", flag.DefValue, defaultServeAddr)
	}
}

// TestInitSummaryMovesTheKeyWhereRunsCannotReadIt pins the steps init prints with a unit: install
// the key file root-owned at 0600 where the unit loads it, in the same sudo step that copies the
// unit, and then remove the copy this account can read. A run with no execution image runs as this
// account, so the copy init wrote is one cat away from any such run.
func TestInitSummaryMovesTheKeyWhereRunsCannotReadIt(t *testing.T) {
	// Not parallel: the summary reads package-level flag variables.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchtender.env")
	unit := filepath.Join(dir, "switchtender.service")
	setString(t, &initConfig, cfg)
	setString(t, &initDB, filepath.Join(dir, "st.db"))
	setString(t, &initAdmin, "admin")
	setString(t, &initAddr, defaultServeAddr)
	tests := []struct {
		// Systemd is the --systemd value.
		Systemd string
		// WantLines are the lines the summary prints, in this order.
		WantLines []string
		// WantAbsent are lines the summary must not print.
		WantAbsent []string
	}{{ // Test 0: With a unit, the key moves to root before the unit starts, then the copy goes.
		Systemd: unit,
		WantLines: []string{
			"  sudo install -d -m 0755 -o root -g root /etc/switchtender",
			"  sudo install -m 0600 -o root -g root " + util.ShellQuote(cfg) + " " + systemEnvFile,
			"  sudo cp " + util.ShellQuote(unit) + " /etc/systemd/system/switchtender.service",
			"  sudo systemctl enable --now switchtender",
			"  rm " + util.ShellQuote(cfg),
		},
	}, { // Test 1: Without a unit, the person sources the file by hand and nothing moves.
		Systemd:    "",
		WantLines:  []string{"  set -a; . " + util.ShellQuote(cfg) + "; set +a"},
		WantAbsent: []string{"  rm " + util.ShellQuote(cfg)},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			setString(t, &initSystemd, test.Systemd)
			var buf bytes.Buffer
			printInitSummary(&buf, false, "")
			lines := strings.Split(buf.String(), "\n")
			at := 0
			for _, want := range test.WantLines {
				i := slices.Index(lines[at:], want)
				if i < 0 {
					t.Fatalf("summary lacks %q after line %d:\n%s", want, at, buf.String())
				}
				at += i + 1
			}
			for _, absent := range test.WantAbsent {
				if slices.Contains(lines, absent) {
					t.Errorf("summary prints %q:\n%s", absent, buf.String())
				}
			}
		})
	}
}

// TestInitRefusesToMintOverTheRootKeyFile pins that regenerating a unit after the person removed
// their own copy of the key, as the summary tells them to, does not mint new keys over the ones in
// the root-owned file the unit loads. That would orphan every sealed secret and every backup.
func TestInitRefusesToMintOverTheRootKeyFile(t *testing.T) {
	// Not parallel: init reads package-level flag variables.
	dir := t.TempDir()
	rootFile := filepath.Join(dir, "root.env")
	if err := os.WriteFile(rootFile, []byte("SWITCHTENDER_ENCRYPTION_KEY=x\n"), 0o600); err != nil {
		t.Fatalf("seed the root key file: %v", err)
	}
	prev := systemEnvFile
	systemEnvFile = rootFile
	t.Cleanup(func() { systemEnvFile = prev })
	setString(t, &initConfig, filepath.Join(dir, "switchtender.env"))
	setString(t, &initDB, filepath.Join(dir, "st.db"))
	setString(t, &initAdmin, "admin")
	setString(t, &initAddr, defaultServeAddr)
	setBool(t, &initForce, true)
	t.Setenv("SWITCHTENDER_ADMIN_PASSWORD", "a-known-password")
	tests := []struct {
		// Systemd is the --systemd value.
		Systemd string
		// Want is the error init returns.
		Want error
	}{{ // Test 0: Regenerating a unit with no readable config and a root key file is refused.
		Systemd: filepath.Join(dir, "switchtender.service"), Want: ErrUsage,
	}, { // Test 1: Without a unit nothing reads the root file, so init sets up as before.
		Systemd: "", Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			setString(t, &initSystemd, test.Systemd)
			err := runInit(testCommand(), nil)
			if !errors.Is(err, test.Want) {
				t.Fatalf("runInit() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil && !strings.Contains(err.Error(), rootFile) {
				t.Errorf("runInit() error = %v, want it to name %s", err, rootFile)
			}
		})
	}
}
