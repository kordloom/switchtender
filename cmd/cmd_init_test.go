package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
