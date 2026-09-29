package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/spanbeat"
)

// setDuration points a package-level duration flag variable at value and restores it afterward. The
// command flags are package globals, so a test that drives a command body has to set them the way
// cobra does.
func setDuration(t *testing.T, p *time.Duration, value time.Duration) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = value
}

// tempDB returns a fresh SQLite path inside the test's own directory, so a command that opens a
// store never touches a real install or the repository.
func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "switchtender.db")
}

// TestTokenNewRefusesAnAgentTokenWithNoAccount proves an agent token cannot be minted unbound.
//
// The agent identity exists so the chain records which human an agent acted for. An unbound token is
// also an admin token, so minting one with --agent would produce an administrator acting on behalf
// of nobody, which is exactly the accountability the flag claims to add. The refusal has to leave
// nothing behind: a token saved before the check would be an admin credential the operator believes
// is a capped agent one.
func TestTokenNewRefusesAnAgentTokenWithNoAccount(t *testing.T) {
	// Not parallel: the token commands read package-level flag variables.
	db := tempDB(t)
	setString(t, &tokenDB, db)
	setString(t, &tokenName, "agent")
	setString(t, &tokenUser, "")
	setBool(t, &tokenAgent, true)
	setDuration(t, &tokenTTL, 0)

	err := runTokenNew(testCommand(), nil)
	if err == nil {
		t.Fatal("runTokenNew() with --agent and no --user = nil error; an agent token was minted " +
			"unbound, which is an admin token acting for nobody")
	}
	if !strings.Contains(err.Error(), "--user") {
		t.Errorf("runTokenNew() error = %v, want it to name the flag that fixes it", err)
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	list, lerr := bundle.Tokens().List(context.Background())
	if lerr != nil {
		t.Fatalf("List() error = %v", lerr)
	}
	if len(list) != 0 {
		t.Errorf("the refused mint left %d token(s) in the store; the refusal has to fail closed",
			len(list))
	}
}

// TestDemoRefusesContradictoryAndUnusableFlags pins the demo's own flag validation. --no-seed and
// --seed-only are opposites, and a beat cadence the emitter cannot honor has to be refused rather
// than rounded, since the cadence is committed into every beat entry and a beat claiming a cadence
// nothing produced is a false statement in a signed record.
func TestDemoRefusesContradictoryAndUnusableFlags(t *testing.T) {
	tests := []struct {
		Name     string
		NoSeed   bool
		SeedOnly bool
		Cadence  time.Duration
		WantWord string
	}{{ // Test 0: Seeding and not seeding cannot both be asked for.
		Name: "opposites", NoSeed: true, SeedOnly: true, WantWord: "opposites",
	}, { // Test 1: A cadence under a second is refused.
		Name: "sub second", NoSeed: true, Cadence: 500 * time.Millisecond, WantWord: "span-cadence",
	}, { // Test 2: A cadence that is not a whole number of seconds is refused.
		Name: "fractional", NoSeed: true, Cadence: 1500 * time.Millisecond, WantWord: "span-cadence",
	}, { // Test 3: A negative cadence is refused rather than read as beats off.
		Name: "negative", NoSeed: true, Cadence: -time.Second, WantWord: "span-cadence",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the demo command reads package-level flag variables.
			setString(t, &demoDB, tempDB(t))
			setBool(t, &demoNoSeed, test.NoSeed)
			setBool(t, &demoSeedOnly, test.SeedOnly)
			setDuration(t, &demoSpanCadence, test.Cadence)
			setString(t, &demoAnchorTSA, "")

			err := runDemo(testCommand(), nil)
			if err == nil {
				t.Fatalf("%s: runDemo() = nil error, want the misconfiguration refused", test.Name)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("%s: runDemo() error = %v, want it to name %q", test.Name, err, test.WantWord)
			}
		})
	}
}

// TestChangeRegisterRefusesAnInvertedPeriod pins the period validation on the change register. The
// register is the evidence a SOC 2 or ISO review samples from, so a period whose end precedes its
// start has to be refused: silently swapping the bounds or reporting nothing would both hand an
// auditor a document that looks complete and covers the wrong window.
func TestChangeRegisterRefusesAnInvertedPeriod(t *testing.T) {
	tests := []struct {
		Name     string
		From     string
		To       string
		WantWord string
	}{{ // Test 0: The end precedes the start.
		Name: "inverted", From: "2026-02-01", To: "2026-01-01", WantWord: "does not precede",
	}, { // Test 1: A zero-width period covers nothing.
		Name: "same instant", From: "2026-01-01", To: "2026-01-01", WantWord: "does not precede",
	}, { // Test 2: An unreadable start is an error, not a silent widening to the epoch.
		Name: "bad from", From: "yesterday", To: "2026-01-01", WantWord: "parse --from",
	}, { // Test 3: An unreadable end is an error, not a silent widening to now.
		Name: "bad to", From: "2026-01-01", To: "tomorrow", WantWord: "parse --to",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the report command reads package-level flag variables.
			setString(t, &auditReportDB, tempDB(t))
			setString(t, &auditReportFrom, test.From)
			setString(t, &auditReportTo, test.To)
			setString(t, &auditReportOut, "")

			err := runChangeRegister(testCommand())
			if err == nil {
				t.Fatalf("%s: runChangeRegister() = nil error, want the period refused", test.Name)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("%s: runChangeRegister() error = %v, want it to name %q",
					test.Name, err, test.WantWord)
			}
		})
	}
}

// TestTeamOnlyCommandsRefuseOnCommunity pins the two license gates that live in this package. They
// are not security boundaries, but they are refusals, and a gate that fails open is a gate that is
// not there. The error has to name the tier and where to go, since that line is the whole of what an
// operator sees when they hit one.
func TestTeamOnlyCommandsRefuseOnCommunity(t *testing.T) {
	if license.Current() != nil {
		t.Skip("this process already carries a license, so the Community gate cannot be observed")
	}
	tests := []struct {
		Name    string
		Feature license.Feature
	}{{ // Test 0: The period change register is the compliance packaging.
		Name: "register", Feature: license.FeatureRegister,
	}, { // Test 1: Distributed workers are Team.
		Name: "workers", Feature: license.FeatureWorkers,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := license.Allow(test.Feature)
			if err == nil {
				t.Fatalf("%s: license.Allow() = nil on Community, so the gate is open", test.Name)
			}
			if !strings.Contains(err.Error(), "switchtender.com/pricing") {
				t.Errorf("%s: the gate message does not say where to go: %v", test.Name, err)
			}
		})
	}
}

// TestAuditReportRefusesOnCommunity drives the command body so the gate is proved where it is
// actually installed, not only in the license package. A gate wired to the wrong feature, or wired
// after the work, passes every rule-level test and still ships the register for free.
func TestAuditReportRefusesOnCommunity(t *testing.T) {
	if license.Current() != nil {
		t.Skip("this process already carries a license, so the Community gate cannot be observed")
	}
	// Not parallel: the report command reads package-level flag variables.
	dir := t.TempDir()
	setString(t, &auditReportDB, filepath.Join(dir, "switchtender.db"))
	setString(t, &auditReportFrom, "")
	setString(t, &auditReportTo, "")
	setString(t, &auditReportOut, filepath.Join(dir, "register.html"))

	err := runAuditReport(testCommand(), nil)
	if err == nil {
		t.Fatal("runAuditReport() = nil error on Community; the register is gated and the gate opened")
	}
	if !strings.Contains(err.Error(), "change register") {
		t.Errorf("runAuditReport() error = %v, want it to name the gated feature", err)
	}
	if _, serr := os.Stat(auditReportOut); serr == nil {
		t.Error("the refused command still wrote a register file")
	}
}

// TestEmptyChainCommandsRefuseRatherThanPublishNothing pins what the evidence commands do with a
// database that has no chain in it. Emitting an empty signed bundle, or an anchor over no entries,
// would produce a document that verifies and attests to nothing, which is worse than an error
// because a relying party cannot tell it apart from a real one.
func TestEmptyChainCommandsRefuseRatherThanPublishNothing(t *testing.T) {
	tests := []struct {
		Name     string
		WantWord string
		Run      func(t *testing.T, db string) error
	}{{ // Test 0: A bundle over no entries is refused.
		Name: "bundle", WantWord: "empty",
		Run: func(t *testing.T, db string) error {
			setString(t, &bundleDB, db)
			setString(t, &bundleOut, filepath.Join(t.TempDir(), "bundle.json"))
			setInt(t, &bundleLimit, 0)
			setString(t, &bundleKeyDir, t.TempDir())
			return runAuditBundle(testCommand(), nil)
		},
	}, { // Test 1: An anchor over no entries is refused.
		Name: "anchor", WantWord: "nothing to anchor",
		Run: func(t *testing.T, db string) error {
			setString(t, &anchorDB, db)
			setString(t, &anchorType, audit.AnchorRFC3161)
			setString(t, &anchorRef, "")
			setBool(t, &anchorTree, false)
			return runAuditAnchor(testCommand(), nil)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the audit commands read package-level flag variables.
			err := test.Run(t, tempDB(t))
			if err == nil {
				t.Fatalf("%s: the command succeeded on an empty chain, so it published a document "+
					"that attests to nothing", test.Name)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("%s: error = %v, want it to name %q", test.Name, err, test.WantWord)
			}
		})
	}
}

// TestAuditRunDossierSaysWhenTheRunIsNotThere pins the message for a run id that is not in the
// database. An auditor sampling a run id has to be able to tell "this install does not hold that
// run" apart from "the evidence could not be collected", because only the first one is answered by
// looking somewhere else.
func TestAuditRunDossierSaysWhenTheRunIsNotThere(t *testing.T) {
	// Not parallel: the dossier command reads package-level flag variables.
	setString(t, &dossierDB, tempDB(t))
	setString(t, &dossierOut, filepath.Join(t.TempDir(), "dossier.html"))

	err := runAuditRunDossier(testCommand(), []string{"run_missing"})
	if err == nil {
		t.Fatal("runAuditRunDossier() = nil error for a run that is not stored")
	}
	if !strings.Contains(err.Error(), "run_missing") ||
		!strings.Contains(err.Error(), "not in this database") {
		t.Errorf("runAuditRunDossier() error = %v, want it to name the run and say it is absent", err)
	}
	if _, serr := os.Stat(dossierOut); serr == nil {
		t.Error("the refused command still wrote a dossier file")
	}
}

// TestVerifyRefusesAReceiptItCannotRead pins the offline verifier's input handling. It is the
// command a relying party runs on a machine that has never seen this install, so every way its
// input can be wrong has to end in a refusal rather than a verdict.
func TestVerifyRefusesAReceiptItCannotRead(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte("not a receipt at all"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	truncated := filepath.Join(dir, "truncated.json")
	if err := os.WriteFile(truncated, []byte(`{"claims":[`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	notJSON := filepath.Join(dir, "binary.bin")
	if err := os.WriteFile(notJSON, []byte{0x00, 0xff, 0xfe, 0x01}, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tests := []struct {
		Name string
		Path string
	}{{ // Test 0: A file that is not there.
		Name: "missing", Path: filepath.Join(dir, "absent.json"),
	}, { // Test 1: A directory is not a receipt.
		Name: "directory", Path: dir,
	}, { // Test 2: An empty file carries no claims to check.
		Name: "empty", Path: empty,
	}, { // Test 3: Text that is not a receipt.
		Name: "garbage", Path: garbage,
	}, { // Test 4: JSON that stops partway.
		Name: "truncated", Path: truncated,
	}, { // Test 5: Bytes that are not text at all.
		Name: "binary", Path: notJSON,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: verify reads the package-level --pubkey variable.
			setString(t, &verifyPubkey, "")
			if err := runVerify(testCommand(), []string{test.Path}); err == nil {
				t.Errorf("%s: runVerify(%q) = nil error, so an unreadable receipt verified",
					test.Name, test.Path)
			}
		})
	}
}

// TestInitRefusesToOverwriteAConfigItWasNotAskedTo pins the guard on the environment file. That file
// holds the encryption key every stored credential is sealed under, so writing over it without
// --force would orphan every secret in a live install, and the refusal is the only thing standing
// between an operator rerunning init and losing them.
func TestInitRefusesToOverwriteAConfigItWasNotAskedTo(t *testing.T) {
	// Not parallel: init reads package-level flag variables and the admin password environment.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchtender.env")
	const body = "SWITCHTENDER_ENCRYPTION_KEY=aa\nSWITCHTENDER_ENCRYPTION_SALT=bb\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	setString(t, &initConfig, cfg)
	setString(t, &initDB, filepath.Join(dir, "st.db"))
	setString(t, &initAdmin, "admin")
	setString(t, &initSystemd, "")
	setBool(t, &initForce, false)
	t.Setenv("SWITCHTENDER_ADMIN_PASSWORD", "a-known-password")

	err := runInit(testCommand(), nil)
	if err == nil {
		t.Fatal("runInit() overwrote an existing config with no --force, orphaning every sealed secret")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("runInit() error = %v, want it to name the flag that would allow it", err)
	}

	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if diff := cmp.Diff(body, string(after)); diff != "" {
		t.Errorf("the refused init changed the config file (-want +got):\n%s", diff)
	}
	if _, serr := os.Stat(filepath.Join(dir, "st.db")); serr == nil {
		t.Error("the refused init created the database, so the refusal was not before the work")
	}
}

// TestInitWritesTheConfigWithNoGroupOrWorldAccess pins the mode on the file holding the encryption
// key. Any other account on the host that can read it can open every credential, content source
// secret, and trigger secret in the database, and every backup taken under that key.
func TestInitWritesTheConfigWithNoGroupOrWorldAccess(t *testing.T) {
	// Not parallel: init reads package-level flag variables and the admin password environment.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchtender.env")
	setString(t, &initConfig, cfg)
	setString(t, &initDB, filepath.Join(dir, "st.db"))
	setString(t, &initAdmin, "admin")
	setString(t, &initSystemd, filepath.Join(dir, "switchtender.service"))
	setString(t, &initAddr, defaultServeAddr)
	setBool(t, &initForce, false)
	t.Setenv("SWITCHTENDER_ADMIN_PASSWORD", "a-known-password")

	if err := runInit(testCommand(), nil); err != nil {
		t.Fatalf("runInit() error = %v", err)
	}
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the config holding the encryption key is mode %04o; any other account on the host "+
			"can read it and open every sealed secret", perm)
	}

	// The unit has to name the config it was told to write, or the server starts with no key.
	unit, err := os.ReadFile(initSystemd)
	if err != nil {
		t.Fatalf("ReadFile(unit) error = %v", err)
	}
	if !strings.Contains(string(unit), "EnvironmentFile="+cfg) {
		t.Errorf("the generated unit does not load %s, so the server it starts has no encryption "+
			"key:\n%s", cfg, unit)
	}
	if !strings.Contains(string(unit), "--addr "+defaultServeAddr) {
		t.Errorf("the generated unit does not bind %s:\n%s", defaultServeAddr, unit)
	}
}

// TestWorkerRelayModeRefusesWithoutItsToken pins that a relay worker will not dial a control node
// unauthenticated. Without a token every call it makes would be rejected, so the worker would run,
// log that it started, and lease nothing, which reads as an idle fleet rather than a broken one.
func TestWorkerRelayModeRefusesWithoutItsToken(t *testing.T) {
	// Not parallel: the worker reads package-level flag variables and the token environment.
	setString(t, &workerServer, "https://switchtender.example")
	setString(t, &workerDB, tempDB(t))
	setString(t, &workerPolicyFile, "")
	t.Setenv("SWITCHTENDER_WORKER_TOKEN", "")

	_, _, _, err := workerStore(zap.NewNop())
	if err == nil {
		t.Fatal("workerStore() = nil error with no worker token; the worker would dial the control " +
			"node unauthenticated and lease nothing")
	}
	if !strings.Contains(err.Error(), "SWITCHTENDER_WORKER_TOKEN") {
		t.Errorf("workerStore() error = %v, want it to name the variable that fixes it", err)
	}
}

// TestWorkerRefusesAMalformedPolicyFile pins that a worker will not start enforcing nothing.
//
// The plan-content gate is applied by whichever process claims the run. A worker that fell back to
// an empty policy set on a malformed file would apply a gated change with no plan and no approver
// whenever it won the claim, while the same run was held if the control node claimed it instead,
// which turns the product's central control into a coin flip.
func TestWorkerRefusesAMalformedPolicyFile(t *testing.T) {
	// Not parallel: the worker reads package-level flag variables.
	bad := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(bad, []byte("this: [is not: valid\n  yaml"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	setString(t, &workerServer, "")
	setString(t, &workerDB, tempDB(t))
	setString(t, &workerPolicyFile, bad)

	store, _, closeStore, err := workerStore(zap.NewNop())
	if closeStore != nil {
		closeStore()
	}
	if err == nil {
		t.Fatalf("workerStore() = nil error on a malformed policy file; the worker would start "+
			"enforcing nothing (store %v)", store)
	}
}

// TestContainerRuntimeFromFlagsCoercesToAKnownBinary pins the runtime selection. The value becomes
// the name of a binary the server execs, so anything other than the two supported CLIs has to
// collapse to docker rather than reach exec.
func TestContainerRuntimeFromFlagsCoercesToAKnownBinary(t *testing.T) {
	tests := []struct {
		Name        string
		Flag        string
		WantRuntime string
	}{{ // Test 0: docker selects docker.
		Name: "docker", Flag: "docker", WantRuntime: "docker",
	}, { // Test 1: podman selects podman.
		Name: "podman", Flag: "podman", WantRuntime: "podman",
	}, { // Test 2: An empty value falls back to docker.
		Name: "empty", Flag: "", WantRuntime: "docker",
	}, { // Test 3: An unknown CLI never reaches exec.
		Name: "unknown", Flag: "nerdctl", WantRuntime: "docker",
	}, { // Test 4: The comparison is exact, so a different spelling falls back.
		Name: "wrong case", Flag: "Podman", WantRuntime: "docker",
	}, { // Test 5: A value carrying a path never reaches exec.
		Name: "path", Flag: "/usr/bin/evil", WantRuntime: "docker",
	}, { // Test 6: A value carrying shell metacharacters never reaches exec.
		Name: "injection", Flag: "podman; rm -rf /", WantRuntime: "docker",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the container flags are package globals.
			setString(t, &containerRuntime, test.Flag)
			if diff := cmp.Diff(test.WantRuntime, containerRuntimeFromFlags()); diff != "" {
				t.Errorf("%s: containerRuntimeFromFlags() mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestContainerPullPolicyFromFlagsCoercesToAKnownPolicy pins the pull policy. It is passed straight
// to the container CLI, so an unrecognized value has to become the documented default rather than
// reach the command line, where it would fail every run instead of one flag.
func TestContainerPullPolicyFromFlagsCoercesToAKnownPolicy(t *testing.T) {
	tests := []struct {
		Name       string
		Flag       string
		WantPolicy string
	}{{ // Test 0: always is honored.
		Name: "always", Flag: "always", WantPolicy: "always",
	}, { // Test 1: never is honored.
		Name: "never", Flag: "never", WantPolicy: "never",
	}, { // Test 2: missing is the documented default.
		Name: "missing", Flag: "missing", WantPolicy: "missing",
	}, { // Test 3: An empty value falls back to missing.
		Name: "empty", Flag: "", WantPolicy: "missing",
	}, { // Test 4: An unknown policy falls back rather than reaching the CLI.
		Name: "unknown", Flag: "ifnotpresent", WantPolicy: "missing",
	}, { // Test 5: The comparison is exact.
		Name: "wrong case", Flag: "Always", WantPolicy: "missing",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the container flags are package globals.
			setString(t, &containerPullPolicy, test.Flag)
			if diff := cmp.Diff(test.WantPolicy, containerPullPolicyFromFlags()); diff != "" {
				t.Errorf("%s: containerPullPolicyFromFlags() mismatch (-want +got):\n%s",
					test.Name, diff)
			}
		})
	}
}

// TestContainerLimitsFromFlagsCarriesEveryCap pins that each container resource flag reaches the
// limits struct the runner applies. A cap that is registered as a flag and then dropped on the way
// to the runner is worse than no cap: the operator set it, the help says it exists, and the
// container runs unbounded.
func TestContainerLimitsFromFlagsCarriesEveryCap(t *testing.T) {
	// Not parallel: the container flags are package globals.
	setString(t, &containerMemory, "256m")
	setString(t, &containerCPUs, "0.5")
	setInt(t, &containerPidsLimit, 32)
	setString(t, &containerNetwork, "none")

	want := roundhouse.ContainerLimits{Memory: "256m", CPUs: "0.5", PidsLimit: 32, Network: "none"}
	if diff := cmp.Diff(want, containerLimitsFromFlags()); diff != "" {
		t.Errorf("containerLimitsFromFlags() mismatch (-want +got):\n%s", diff)
	}
}

// TestContainerFlagsDefaultToTheBoundedLimits pins that a run is capped when the operator sets
// nothing. The defaults come from roundhouse rather than from empty strings, so an install that
// never touched the container flags still runs containers with a memory, CPU, and process cap.
func TestContainerFlagsDefaultToTheBoundedLimits(t *testing.T) {
	t.Parallel()
	d := roundhouse.DefaultContainerLimits()
	tests := []struct {
		Name        string
		Flag        string
		WantDefault string
	}{{ // Test 0: The memory cap has a default.
		Name: "memory", Flag: "container-memory", WantDefault: d.Memory,
	}, { // Test 1: The CPU cap has a default.
		Name: "cpus", Flag: "container-cpus", WantDefault: d.CPUs,
	}, { // Test 2: The network mode has a default.
		Name: "network", Flag: "container-network", WantDefault: d.Network,
	}, { // Test 3: The runtime defaults to docker.
		Name: "runtime", Flag: "container-runtime", WantDefault: "docker",
	}, { // Test 4: The pull policy defaults to missing.
		Name: "pull policy", Flag: "container-pull-policy", WantDefault: "missing",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			for _, c := range []string{"switchtender serve", "switchtender worker"} {
				cmd := commands().ByPath[c]
				if cmd == nil {
					t.Fatalf("%s is not a command any more", c)
				}
				f := cmd.Flags().Lookup(test.Flag)
				if f == nil {
					t.Fatalf("%s has no --%s flag, so its runs are uncapped", c, test.Flag)
				}
				if f.DefValue != test.WantDefault {
					t.Errorf("%s --%s defaults to %q, want %q: an install that sets nothing must "+
						"still run capped", c, test.Flag, f.DefValue, test.WantDefault)
				}
			}
		})
	}
}

// TestBuildEmailerNeedsEveryPartOfAnAddress pins when notification email turns on. A half-configured
// emailer that constructed itself anyway would fail on every send, and the operator would learn that
// their failure notifications were never arriving from the absence of them.
func TestBuildEmailerNeedsEveryPartOfAnAddress(t *testing.T) {
	tests := []struct {
		Name          string
		Addr          string
		From          string
		To            []string
		NotifyOn      string
		WantConfig    bool
		WantOnFailure bool
	}{{ // Test 0: Nothing configured leaves email off.
		Name: "nothing", WantConfig: false, WantOnFailure: true,
	}, { // Test 1: A server with no sender cannot send.
		Name: "no sender", Addr: "smtp:25", To: []string{"a@b"}, WantConfig: false, WantOnFailure: true,
	}, { // Test 2: A sender with no server cannot send.
		Name: "no server", From: "a@b", To: []string{"a@b"}, WantConfig: false, WantOnFailure: true,
	}, { // Test 3: No recipient means nowhere to send.
		Name: "no recipient", Addr: "smtp:25", From: "a@b", WantConfig: false, WantOnFailure: true,
	}, { // Test 4: All three present turns email on, defaulting to failures only.
		Name: "complete", Addr: "smtp:25", From: "a@b", To: []string{"c@d"}, NotifyOn: "failure",
		WantConfig: true, WantOnFailure: true,
	}, { // Test 5: finish is the one value that widens it to every terminal run.
		Name: "finish", Addr: "smtp:25", From: "a@b", To: []string{"c@d"}, NotifyOn: "finish",
		WantConfig: true, WantOnFailure: false,
	}, { // Test 6: An unrecognized --notify-on falls back to failures only, which is the quieter
		// of the two and cannot flood an inbox from a typo.
		Name: "unknown notify-on", Addr: "smtp:25", From: "a@b", To: []string{"c@d"},
		NotifyOn: "everything", WantConfig: true, WantOnFailure: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the SMTP flags are package globals.
			setString(t, &smtpAddr, test.Addr)
			setString(t, &smtpFrom, test.From)
			setString(t, &smtpUsername, "")
			setString(t, &notifyOn, test.NotifyOn)
			old := smtpTo
			t.Cleanup(func() { smtpTo = old })
			smtpTo = test.To

			emailer, onFailureOnly := buildEmailer()
			if (emailer != nil) != test.WantConfig {
				t.Errorf("%s: buildEmailer() configured = %v, want %v",
					test.Name, emailer != nil, test.WantConfig)
			}
			if onFailureOnly != test.WantOnFailure {
				t.Errorf("%s: onFailureOnly = %v, want %v",
					test.Name, onFailureOnly, test.WantOnFailure)
			}
		})
	}
}

// TestSealerRefusesToWorkWithoutBothHalves pins when credential sealing turns on. The sealer needs
// the key and a stable salt, and a sealer that enabled itself on half of them would encrypt under a
// value the next start cannot reproduce, which is indistinguishable from losing the key.
func TestSealerRefusesToWorkWithoutBothHalves(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const salt = "fedcba9876543210fedcba9876543210"
	tests := []struct {
		Name        string
		Key         string
		Salt        string
		WantEnabled bool
	}{{ // Test 0: Neither half means credentials are off.
		Name: "neither", WantEnabled: false,
	}, { // Test 1: A key with no salt is not enough.
		Name: "key only", Key: key, WantEnabled: false,
	}, { // Test 2: A salt with no key is not enough.
		Name: "salt only", Salt: salt, WantEnabled: false,
	}, { // Test 3: Both halves enable sealing.
		Name: "both", Key: key, Salt: salt, WantEnabled: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv mutates the process environment.
			t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", test.Key)
			t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", test.Salt)
			sealer := newSealerFromEnv(zap.NewNop())
			if sealer == nil {
				t.Fatalf("%s: newSealerFromEnv() = nil, want a disabled sealer rather than nothing",
					test.Name)
			}
			if sealer.Enabled() != test.WantEnabled {
				t.Errorf("%s: sealer.Enabled() = %v, want %v",
					test.Name, sealer.Enabled(), test.WantEnabled)
			}
		})
	}
}

// TestPluginsDirPrefersTheFlagThenTheEnvironment pins where extension plugins are loaded from. The
// directory names binaries the server executes at startup, so which of the two sources wins is a
// decision about what code runs, and an empty answer has to mean no plugins rather than the working
// directory.
func TestPluginsDirPrefersTheFlagThenTheEnvironment(t *testing.T) {
	tests := []struct {
		Name    string
		Flag    string
		Env     string
		WantDir string
	}{{ // Test 0: Neither set loads no plugins.
		Name: "neither", WantDir: "",
	}, { // Test 1: The environment is used when the flag is unset.
		Name: "environment", Env: "/opt/plugins", WantDir: "/opt/plugins",
	}, { // Test 2: The flag is used when the environment is unset.
		Name: "flag", Flag: "/srv/plugins", WantDir: "/srv/plugins",
	}, { // Test 3: The flag wins over the environment, since it is the more explicit of the two.
		Name: "flag wins", Flag: "/srv/plugins", Env: "/opt/plugins", WantDir: "/srv/plugins",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv mutates the process environment.
			t.Setenv("SWITCHTENDER_PLUGINS_DIR", test.Env)
			if diff := cmp.Diff(test.WantDir, pluginsDir(test.Flag)); diff != "" {
				t.Errorf("%s: pluginsDir(%q) mismatch (-want +got):\n%s", test.Name, test.Flag, diff)
			}
		})
	}
}

// TestWorkerTokenPrefersTheFlagThenTheEnvironment pins how the mesh relay token is resolved. The
// token is what turns the relay endpoints on, so an empty answer has to leave them off, and the
// environment has to work on its own since that is the channel the help recommends for a secret a
// flag would publish to the process list.
func TestWorkerTokenPrefersTheFlagThenTheEnvironment(t *testing.T) {
	tests := []struct {
		Name      string
		Flag      string
		Env       string
		WantToken string
	}{{ // Test 0: Neither set leaves the relay endpoints off.
		Name: "neither", WantToken: "",
	}, { // Test 1: The environment alone is enough.
		Name: "environment", Env: "env-token", WantToken: "env-token",
	}, { // Test 2: The flag alone is enough.
		Name: "flag", Flag: "flag-token", WantToken: "flag-token",
	}, { // Test 3: The flag wins, since it is the more explicit of the two.
		Name: "flag wins", Flag: "flag-token", Env: "env-token", WantToken: "flag-token",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv mutates the process environment.
			t.Setenv("SWITCHTENDER_WORKER_TOKEN", test.Env)
			setString(t, &serveWorkerToken, test.Flag)
			if diff := cmp.Diff(test.WantToken, workerToken()); diff != "" {
				t.Errorf("%s: workerToken() mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestExternalAuthDrivesEnforcement pins the wiring between the sign-on flags and the option that
// keeps the API gate enforcing.
//
// An install whose way in is single sign-on has empty token and account tables until somebody signs
// in, and enforcement derived from those tables served the whole API to anonymous callers as admin
// in the meantime. Each provider flag has to switch enforcement on by itself, since an install
// configures whichever one it uses and none of the others.
func TestExternalAuthDrivesEnforcement(t *testing.T) {
	tests := []struct {
		Name         string
		OIDC         string
		LDAP         string
		SAML         string
		JWT          string
		WantExternal bool
	}{{ // Test 0: No provider configured leaves the token count in charge.
		Name: "none", WantExternal: false,
	}, { // Test 1: OIDC alone enforces.
		Name: "oidc", OIDC: "https://issuer.example", WantExternal: true,
	}, { // Test 2: LDAP alone enforces.
		Name: "ldap", LDAP: "ldaps://ldap.example:636", WantExternal: true,
	}, { // Test 3: SAML alone enforces.
		Name: "saml", SAML: "https://idp.example/metadata", WantExternal: true,
	}, { // Test 4: Bearer JWT alone enforces.
		Name: "jwt", JWT: "https://jwt.example/jwks", WantExternal: true,
	}, { // Test 5: Several at once still enforce.
		Name: "several", OIDC: "https://issuer.example", JWT: "https://jwt.example/jwks",
		WantExternal: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the sign-on flags are package globals.
			setString(t, &serveOIDCIssuer, test.OIDC)
			setString(t, &serveLDAPURL, test.LDAP)
			setString(t, &serveSAMLIDPMetadataURL, test.SAML)
			setString(t, &serveJWTJWKSURL, test.JWT)

			if got := externalAuthConfigured(); got != test.WantExternal {
				t.Fatalf("%s: externalAuthConfigured() = %v, want %v",
					test.Name, got, test.WantExternal)
			}
			// The option is what actually reaches the server, so the constructor is exercised
			// rather than only the predicate it reads.
			if enforcedAuthOption(externalAuthConfigured()) == nil {
				t.Errorf("%s: enforcedAuthOption() = nil, which would panic when applied", test.Name)
			}
		})
	}
}

// TestKeyDirPrefersTheOverrideThenTheInstall pins where the bundle command reads the producer
// signing key from. A bundle signed with a key other than the one serve publishes cannot be pinned
// by a relying party, so the override has to win when given and the install's own directory has to
// be the answer when it is not.
func TestKeyDirPrefersTheOverrideThenTheInstall(t *testing.T) {
	// Not parallel: the bundle flags are package globals.
	dir := t.TempDir()
	db := filepath.Join(dir, "switchtender.db")

	setString(t, &bundleDB, db)
	setString(t, &bundleKeyDir, "")
	got, err := keyDir()
	if err != nil {
		t.Fatalf("keyDir() error = %v", err)
	}
	if diff := cmp.Diff(dir, got); diff != "" {
		t.Errorf("keyDir() without an override mismatch (-want +got):\n%s", diff)
	}

	override := t.TempDir()
	setString(t, &bundleKeyDir, override)
	got, err = keyDir()
	if err != nil {
		t.Fatalf("keyDir() with an override error = %v", err)
	}
	if diff := cmp.Diff(override, got); diff != "" {
		t.Errorf("keyDir() with an override mismatch (-want +got):\n%s", diff)
	}
}

// TestAuditBeatStoreReportsWhatItAppended pins the adapter between the audit chain and the span beat
// emitter. The emitter logs and anchors the sequence, hash, and beat number this returns, so an
// adapter that dropped one of them would anchor a coordinate that names nothing, and the anchor
// would then fail every bundle built over the chain.
func TestAuditBeatStoreReportsWhatItAppended(t *testing.T) {
	t.Parallel()
	store := auditBeatStore{store: audit.NewMemStore()}
	ctx := context.Background()

	first, err := store.AppendSpanBeat(ctx, time.Now(), 60)
	if err != nil {
		t.Fatalf("AppendSpanBeat() error = %v", err)
	}
	if first.Beat != 1 {
		t.Errorf("first beat number = %d, want 1", first.Beat)
	}
	if first.Seq == 0 || first.Hash == "" || first.At.IsZero() {
		t.Errorf("AppendSpanBeat() = %+v, want a sequence, a hash, and a time the emitter can "+
			"anchor", first)
	}

	second, err := store.AppendSpanBeat(ctx, time.Now().Add(time.Minute), 60)
	if err != nil {
		t.Fatalf("second AppendSpanBeat() error = %v", err)
	}
	if second.Beat != first.Beat+1 {
		t.Errorf("second beat number = %d, want %d", second.Beat, first.Beat+1)
	}
	if second.Seq <= first.Seq {
		t.Errorf("second sequence = %d, want it past the first at %d", second.Seq, first.Seq)
	}
	if second.Hash == first.Hash {
		t.Error("two beats share a hash, so the chain link does not distinguish them")
	}

	// A clock that went backwards is refused rather than signed, since a beat's time is a claim.
	var behind spanbeat.AppendedBeat
	behind, err = store.AppendSpanBeat(ctx, first.At.Add(-time.Hour), 60)
	if err == nil {
		t.Errorf("AppendSpanBeat() with a clock behind the newest beat = %+v, want a refusal", behind)
	}
}

// TestRecordCLIFailsTheCommandItCannotRecord pins the fail-closed rule on command-line mutations. A
// mutation that cannot be recorded must not happen: creating an admin account and minting a token
// are the two most security-relevant operations in the product, and a record that omits them is
// worse than no record, because it invites the reader to trust it.
func TestRecordCLIFailsTheCommandItCannotRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Audits  audit.Store
		Body    []byte
		WantErr bool
	}{{ // Test 0: A store that refuses the append fails the command.
		Name: "refusing store", Audits: refusingAudits{}, WantErr: true,
	}, { // Test 1: The same holds when a content digest is committed alongside the call.
		Name: "refusing store with a body", Audits: refusingAudits{},
		Body: []byte(`{"users":1}`), WantErr: true,
	}, { // Test 2: A working store records and the command proceeds.
		Name: "working store", Audits: audit.NewMemStore(), WantErr: false,
	}, { // Test 3: An install with no audit store configured is not failed by the recorder.
		Name: "no store", Audits: nil, WantErr: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := recordCLIChange(context.Background(), test.Audits, "/cli/test", test.Body)
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: recordCLIChange() error = %v, want error %v",
					test.Name, err, test.WantErr)
			}
			if test.WantErr && !strings.Contains(err.Error(), "refused") {
				t.Errorf("%s: recordCLIChange() error = %v, want it to say the change was refused",
					test.Name, err)
			}
		})
	}
}

// refusingAudits is an audit store whose append always fails, standing in for a chain that cannot be
// written: a full disk, a revoked grant, or a database in recovery.
type refusingAudits struct {
	// Store answers every method this stub does not override.
	audit.Store
}

// errAuditRefused is what the refusing store answers with.
var errAuditRefused = errors.New("the chain is not writable")

// Append always refuses, so a caller's fail-closed handling is what the test observes.
func (refusingAudits) Append(context.Context, *audit.Entry) error { return errAuditRefused }

// TestCLIActorNamesTheHostAccount pins the actor recorded for a command-line mutation. No token and
// no session is involved here, so the host account is the only observed identity, and the prefix
// keeps it from being read as an API token label in the trail.
func TestCLIActorNamesTheHostAccount(t *testing.T) {
	t.Parallel()
	got := cliActor()
	if !strings.HasPrefix(got, "cli:") {
		t.Errorf("cliActor() = %q, want the cli: prefix so it is not read as a token label", got)
	}
	if got == "cli:" {
		t.Error("cliActor() named nobody; an entry with an empty actor records a change by no one")
	}
}
