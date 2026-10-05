// Package migration runs end-to-end migration scenarios: an AWX export carrying every object kind
// the importer moves is imported through the real importer, real servers serve it on SQLite or
// PostgreSQL, real ansible-playbook executes what it launches, and after every scenario the audit
// chain is exported and verified offline by the LoomSeal reference verifier.
//
// Each feature these scenarios exercise has unit and integration tests of its own. What those
// cannot show is that the features hold together on an imported install, under failure, with the
// evidence still verifying afterward, which is the claim a team switching from AWX is relying on.
//
// Every server and command runs as a child process: this test binary re-executes itself as the
// switchtender command, so a server can be stopped, killed mid-wait, and restarted, and two can
// share one PostgreSQL database, exactly as two deployed replicas do. The child trusts a license
// key minted for the run, the one thing a release binary cannot be given, so the Team features a
// scenario needs are licensed rather than reached around.
package migration

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/kordloom/switchtender/cmd"
	"github.com/kordloom/switchtender/internal/license"
)

const (
	// childEnv marks a process started by this suite as the switchtender command rather than as
	// the test binary.
	childEnv = "SWITCHTENDER_SCENARIO_CHILD"
	// licenseKeyEnv carries the hex public key the child trusts for the license this suite mints.
	licenseKeyEnv = "SWITCHTENDER_SCENARIO_LICENSE_KEY"
	// licenseKid is the key id the suite's license names.
	licenseKid = "scenario"
	// requireFullEnv turns a missing tool from a skip into a failure, the switch every other
	// suite in the repository reads for the same purpose.
	requireFullEnv = "SWITCHTENDER_REQUIRE_FULL_SUITE"
)

// TestMain runs the suite, or, in a child this suite started, the switchtender command.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		runChild()
		return
	}
	os.Exit(m.Run())
}

// runChild trusts the suite's license key and runs the switchtender command with this process's
// arguments. It never returns: the command exits the process with its own code.
func runChild() {
	if raw := os.Getenv(licenseKeyEnv); raw != "" {
		pub, err := hex.DecodeString(raw)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			fmt.Fprintln(os.Stderr, "scenario child: the license key is not an ed25519 public key")
			os.Exit(2)
		}
		license.RegisterKey(licenseKid, ed25519.PublicKey(pub))
	}
	os.Args = append([]string{"switchtender"}, os.Args[1:]...)
	cmd.Execute(nil)
}
