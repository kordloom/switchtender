package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
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
