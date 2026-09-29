package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestImportWithoutApplyCreatesNothing pins the dry run.
//
// The import commands read somebody else's automation and turn it into templates, schedules, and
// credentials in this install. Without --apply the command is documented as reporting only what it
// would create, and an operator reviews that report before letting it run. A dry run that wrote
// would be the worst possible failure of that promise: the review happens after the change.
func TestImportWithoutApplyCreatesNothing(t *testing.T) {
	// Not parallel: the import commands read package-level flag variables.
	dir := t.TempDir()
	db := filepath.Join(dir, "switchtender.db")
	crontab := filepath.Join(dir, "crontab")
	const lines = "# a comment\n0 3 * * * /usr/local/bin/nightly-backup\n" +
		"*/15 * * * * /usr/local/bin/collect-metrics\n"
	if err := os.WriteFile(crontab, []byte(lines), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	setString(t, &importDB, db)
	setBool(t, &importApply, false)
	setString(t, &importCronInventory, "prod")
	setBool(t, &importCronSystem, false)

	if err := runImport(testCommand(), crontab,
		importer.FromCron(importCronInventory, importCronSystem)); err != nil {
		t.Fatalf("runImport() dry run error = %v", err)
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	ctx := context.Background()
	schedules, err := bundle.Schedules().List(ctx)
	if err != nil {
		t.Fatalf("Schedules().List() error = %v", err)
	}
	templates, err := bundle.Templates().List(ctx)
	if err != nil {
		t.Fatalf("Templates().List() error = %v", err)
	}
	chain, err := bundle.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(schedules) != 0 || len(templates) != 0 {
		t.Errorf("a dry run created %d schedule(s) and %d template(s); without --apply it must "+
			"only report", len(schedules), len(templates))
	}
	if len(chain) != 0 {
		t.Errorf("a dry run appended %d audit entries; it changed nothing, so it records nothing",
			len(chain))
	}
}

// TestImportWithApplyCreatesAndRecordsOnce pins what --apply does. The objects have to arrive, and
// the chain has to carry exactly one entry for the import as a whole: an import creates many objects
// at once from one export, and recording each separately would bury the fact that they arrived
// together, which is the thing a reviewer needs to see.
func TestImportWithApplyCreatesAndRecordsOnce(t *testing.T) {
	// Not parallel: the import commands read package-level flag variables.
	dir := t.TempDir()
	db := filepath.Join(dir, "switchtender.db")
	crontab := filepath.Join(dir, "crontab")
	const lines = "0 3 * * * /usr/local/bin/nightly-backup\n" +
		"*/15 * * * * /usr/local/bin/collect-metrics\n"
	if err := os.WriteFile(crontab, []byte(lines), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	setString(t, &importDB, db)
	setBool(t, &importApply, true)
	setString(t, &importCronInventory, "prod")
	setBool(t, &importCronSystem, false)

	if err := runImport(testCommand(), crontab,
		importer.FromCron(importCronInventory, importCronSystem)); err != nil {
		t.Fatalf("runImport() with --apply error = %v", err)
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	ctx := context.Background()
	schedules, err := bundle.Schedules().List(ctx)
	if err != nil {
		t.Fatalf("Schedules().List() error = %v", err)
	}
	if len(schedules) != 2 {
		t.Errorf("--apply created %d schedules, want the two crontab job lines", len(schedules))
	}
	chain, err := bundle.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var imports int
	for _, e := range chain {
		if e.Path == "/cli/import/apply" {
			imports++
		}
	}
	if imports != 1 {
		t.Errorf("the chain holds %d import entries, want exactly one for the import as a whole; "+
			"an import that leaves no entry is a change an auditor cannot see, and one that leaves "+
			"many buries that they arrived together", imports)
	}
}

// TestImportRefusesADocumentItDoesNotRecognize pins the empty-plan refusal at the command layer. An
// export from the wrong endpoint parses as valid and yields nothing, and reporting that as a plan of
// zeros with exit status zero tells the operator the import worked.
func TestImportRefusesADocumentItDoesNotRecognize(t *testing.T) {
	// Not parallel: the import commands read package-level flag variables.
	dir := t.TempDir()
	empty := filepath.Join(dir, "crontab")
	if err := os.WriteFile(empty, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	setString(t, &importDB, filepath.Join(dir, "switchtender.db"))
	setBool(t, &importApply, false)
	setString(t, &importCronInventory, "prod")
	setBool(t, &importCronSystem, false)

	err := runImport(testCommand(), empty,
		importer.FromCron(importCronInventory, importCronSystem))
	if !errors.Is(err, importer.ErrNothingRecognized) {
		t.Errorf("runImport() on a document with no jobs = %v, want %v: a plan of zeros with a "+
			"zero exit tells the operator the import worked", err, importer.ErrNothingRecognized)
	}
}

// TestImportRefusesAFileItCannotRead pins the read error. Every importer routes through the same
// read, so a path that is not there, or is a directory, has to fail before anything is planned.
func TestImportRefusesAFileItCannotRead(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		Name string
		Path string
	}{{ // Test 0: A file that is not there.
		Name: "missing", Path: filepath.Join(dir, "absent.json"),
	}, { // Test 1: A directory is not an export.
		Name: "directory", Path: dir,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the import commands read package-level flag variables.
			setString(t, &importDB, filepath.Join(t.TempDir(), "switchtender.db"))
			setBool(t, &importApply, false)
			err := runImport(testCommand(), test.Path, importer.FromAWX)
			if err == nil {
				t.Fatalf("%s: runImport(%q) = nil error", test.Name, test.Path)
			}
			if !strings.Contains(err.Error(), "read export") {
				t.Errorf("%s: runImport() error = %v, want it to name the read that failed",
					test.Name, err)
			}
		})
	}
}

// TestExamplesIsSafeToRunTwice pins the starter template seeding at the command layer. The command
// documents itself as safe to run twice, and an operator reruns it after adding their own templates,
// so a second run that duplicated the starters would put four confusing copies in the list they were
// told to delete from.
func TestExamplesIsSafeToRunTwice(t *testing.T) {
	// Not parallel: the examples command reads a package-level flag variable.
	db := tempDB(t)
	setString(t, &examplesDB, db)

	if err := runExamples(testCommand(), nil); err != nil {
		t.Fatalf("runExamples() first run error = %v", err)
	}
	if err := runExamples(testCommand(), nil); err != nil {
		t.Fatalf("runExamples() second run error = %v", err)
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	ctx := context.Background()
	list, err := bundle.Templates().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := len(starterTemplates(time.Now()))
	if len(list) != want {
		t.Errorf("two runs left %d templates, want the %d starters exactly once", len(list), want)
	}

	// The seeding is an operator action against the store the server serves, so the first run is on
	// the chain and the second, which added nothing, is not.
	chain, err := bundle.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var seeds int
	for _, e := range chain {
		if e.Path == "/cli/examples" {
			seeds++
		}
	}
	if seeds != 1 {
		t.Errorf("the chain holds %d seeding entries, want one: the first run changed something "+
			"and the second did not", seeds)
	}
}

// TestOpenBundleFailsTowardCommunityOnABadLicense pins the licensing failure direction.
//
// The license loads inside openBundle, which every command goes through, so a file that does not
// verify decides whether the whole CLI still works. It has to be loud and non-fatal: broken
// licensing fails toward Community, never toward a command that will not run. A corrupt or truncated
// file must not be able to take an install offline.
func TestOpenBundleFailsTowardCommunityOnABadLicense(t *testing.T) {
	// Not parallel: t.Setenv points the license path at the fixture.
	dir := t.TempDir()
	bad := filepath.Join(dir, "switchtender-license.json")
	if err := os.WriteFile(bad, []byte(`{"claims":{"tier":"team"},"sig":"not-a-signature"}`),
		0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("SWITCHTENDER_LICENSE", bad)

	bundle, err := openBundle(filepath.Join(dir, "switchtender.db"))
	if err != nil {
		t.Fatalf("openBundle() error = %v; a license file that does not verify must not stop a "+
			"command from running", err)
	}
	defer func() { _ = bundle.Close() }()

	if lic := license.Current(); lic != nil {
		t.Errorf("a license that does not verify was installed as %q; a bad file has to fail "+
			"toward Community, not toward a tier nobody paid for", lic.Claims.Tier)
	}
	if err := license.Allow(license.FeatureRegister); err == nil {
		t.Error("a Team feature is allowed after a license that does not verify; the failure " +
			"direction is inverted")
	}
}

// TestReceiptRedemptionAnswersTheThreeQuestionsAReceiptAsks pins the redemption command.
//
// A receipt is what somebody outside the install holds. Redeeming it has to distinguish three
// answers, because they mean different things to the holder: the chain holds this exact link at this
// exact position, the chain holds a different entry there, or the chain has nothing there at all.
// Collapsing the last two would tell a holder whose entry was removed that their receipt merely does
// not match, which sends them looking for a typo instead of at a record that lost an entry.
func TestReceiptRedemptionAnswersTheThreeQuestionsAReceiptAsks(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	creation := seedReceiptableRun(t, db, "run_redeemed")
	good := audit.Receipt(creation)

	tests := []struct {
		Name     string
		Receipt  string
		WantWord string
		WantErr  bool
	}{{ // Test 0: The receipt the server issued redeems.
		Name: "issued receipt", Receipt: good, WantErr: false,
	}, { // Test 1: A different link at the same position is a different history.
		Name: "wrong link",
		Receipt: fmt.Sprintf("%d:%s", creation.Seq,
			strings.Repeat("0", len(strings.SplitN(good, ":", 2)[1]))),
		WantWord: "different entry", WantErr: true,
	}, { // Test 2: A position the chain never reached says the record is missing something.
		Name: "absent position", Receipt: "99999:" + strings.SplitN(good, ":", 2)[1],
		WantWord: "no entry at sequence", WantErr: true,
	}, { // Test 3: A malformed receipt is refused before the chain is read at all.
		Name: "malformed", Receipt: "not-a-receipt", WantWord: "seq:link", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the redemption command reads package-level flag variables.
			setString(t, &receiptDB, db)
			setBool(t, &receiptPretty, false)
			err := runAuditReceipt(testCommand(), []string{test.Receipt})
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: runAuditReceipt(%q) error = %v, want error %v",
					test.Name, test.Receipt, err, test.WantErr)
			}
			if test.WantErr && !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("%s: runAuditReceipt() error = %v, want it to say %q",
					test.Name, err, test.WantWord)
			}
		})
	}
}

// TestReadPasswordNeverTakesAPasswordFromAnArgument pins how the account password is obtained. It
// comes from the environment or an interactive prompt and never from the command line, so it stays
// out of shell history and out of the host's process list. With no terminal and no environment
// variable the command refuses and says which variable to set, rather than creating an account with
// an empty password.
func TestReadPasswordNeverTakesAPasswordFromAnArgument(t *testing.T) {
	tests := []struct {
		Name     string
		Env      string
		WantPass string
		WantErr  bool
	}{{ // Test 0: The environment supplies the password.
		Name: "from the environment", Env: "a-known-password", WantPass: "a-known-password",
	}, { // Test 1: A password of only spaces is still what the operator set; the environment is
		// taken literally, since trimming it would sign in with a password nobody typed.
		Name: "spaces are literal", Env: "  ", WantPass: "  ",
	}, { // Test 2: With nothing set and no terminal, the command refuses and names the variable.
		Name: "no terminal", Env: "", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv mutates the process environment.
			t.Setenv("SWITCHTENDER_PASSWORD", test.Env)
			got, err := readPassword()
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: readPassword() error = %v, want error %v", test.Name, err, test.WantErr)
			}
			if test.WantErr {
				if !strings.Contains(err.Error(), "SWITCHTENDER_PASSWORD") {
					t.Errorf("%s: readPassword() error = %v, want it to name the variable to set",
						test.Name, err)
				}
				return
			}
			if diff := cmp.Diff(test.WantPass, got); diff != "" {
				t.Errorf("%s: readPassword() mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestUserNewRefusesARoleTheServerDoesNotEnforce pins the role validation on account creation. A
// role the authorization code has never heard of is not a harmless label: it would be stored on the
// account and compared against by every permission check, so an account created with one would have
// whatever the fallthrough of those comparisons happens to be.
func TestUserNewRefusesARoleTheServerDoesNotEnforce(t *testing.T) {
	tests := []struct {
		Name    string
		Role    string
		WantErr bool
	}{{ // Test 0: admin is a role.
		Name: "admin", Role: "admin",
	}, { // Test 1: operator is a role.
		Name: "operator", Role: "operator",
	}, { // Test 2: viewer is a role.
		Name: "viewer", Role: "viewer",
	}, { // Test 3: An invented role is refused.
		Name: "invented", Role: "superuser", WantErr: true,
	}, { // Test 4: The comparison is case sensitive, so a different spelling is refused.
		Name: "wrong case", Role: "Admin", WantErr: true,
	}, { // Test 5: An empty role is refused rather than defaulted.
		Name: "empty", Role: "", WantErr: true,
	}, { // Test 6: Surrounding whitespace is not trimmed, so a padded role is refused.
		Name: "padded", Role: " admin ", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the user commands read package-level flag variables.
			db := tempDB(t)
			setString(t, &userDB, db)
			setString(t, &userRole, test.Role)
			t.Setenv("SWITCHTENDER_PASSWORD", "a-known-password")

			err := runUserNew(testCommand(), []string{"someone"})
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: runUserNew() error = %v, want error %v", test.Name, err, test.WantErr)
			}

			bundle, oerr := openBundle(db)
			if oerr != nil {
				t.Fatalf("%s: openBundle() error = %v", test.Name, oerr)
			}
			defer func() { _ = bundle.Close() }()
			list, lerr := bundle.Users().List(context.Background())
			if lerr != nil {
				t.Fatalf("%s: List() error = %v", test.Name, lerr)
			}
			if test.WantErr {
				if len(list) != 0 {
					t.Errorf("%s: a refused role still created %d account(s)", test.Name, len(list))
				}
				return
			}
			if len(list) != 1 {
				t.Fatalf("%s: created %d accounts, want one", test.Name, len(list))
			}
			if string(list[0].Role) != test.Role {
				t.Errorf("%s: account role = %q, want %q", test.Name, list[0].Role, test.Role)
			}
			// A locally created account is marked local so a directory identity of the same name
			// cannot later be handed this account.
			if list[0].Source != "local" {
				t.Errorf("%s: account Source = %q, want local: an unmarked account can be claimed "+
					"by a directory identity of the same name", test.Name, list[0].Source)
			}
		})
	}
}

// TestUserNewRefusesAUsernameThatIsTaken pins the duplicate check. Two accounts answering to one
// username would make every sign-in and every audit line ambiguous about which one acted.
func TestUserNewRefusesAUsernameThatIsTaken(t *testing.T) {
	// Not parallel: the user commands read package-level flag variables.
	db := tempDB(t)
	setString(t, &userDB, db)
	setString(t, &userRole, "operator")
	t.Setenv("SWITCHTENDER_PASSWORD", "a-known-password")

	if err := runUserNew(testCommand(), []string{"taken"}); err != nil {
		t.Fatalf("runUserNew() first error = %v", err)
	}
	err := runUserNew(testCommand(), []string{"taken"})
	if err == nil {
		t.Fatal("runUserNew() created a second account with the same username")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("runUserNew() error = %v, want it to say the username is taken", err)
	}

	bundle, oerr := openBundle(db)
	if oerr != nil {
		t.Fatalf("openBundle() error = %v", oerr)
	}
	defer func() { _ = bundle.Close() }()
	list, lerr := bundle.Users().List(context.Background())
	if lerr != nil {
		t.Fatalf("List() error = %v", lerr)
	}
	if len(list) != 1 {
		t.Errorf("the store holds %d accounts named taken, want one", len(list))
	}
}

// TestPrintOutcomeRendersWhatTheRunActuallyDid pins the receipt printout for a verified outcome.
//
// Three of these lines exist because their absence made a receipt say the wrong thing. A dry run
// with no mode line read exactly like the real change. A coordinator with no children read "nothing
// happened" over a fan-out that ran on real machines and then said VERIFIED. An install with no
// approval rules had nothing distinguishing "no rule stopped this" from "no rule was recorded".
func TestPrintOutcomeRendersWhatTheRunActuallyDid(t *testing.T) {
	t.Parallel()
	exit0, exit2, shard := 0, 2, 1
	tests := []struct {
		Name      string
		Record    outcome.Record
		WantLines []string
		DenyLines []string
	}{{ // Test 0: An ordinary run names itself, its status, and its exit code.
		Name: "ordinary run",
		Record: outcome.Record{
			RunID: "run_a", Status: "succeeded", ExitCode: &exit0, LogSHA256: "abc",
			Hosts: []outcome.RecordHost{{Host: "web01", Worst: "changed", OK: 5, Changed: 1}},
		},
		WantLines: []string{"run run_a succeeded (exit 0)", "host web01", "ok=5 changed=1",
			"log sha256     abc"},
		DenyLines: []string{"check mode"},
	}, { // Test 1: A check-mode run says so, or a preview reads as the change itself.
		Name: "check mode",
		Record: outcome.Record{
			RunID: "run_b", Status: "succeeded", ExitCode: &exit0, LogSHA256: "abc", DryRun: true,
		},
		WantLines: []string{"check mode, so nothing was changed"},
	}, { // Test 2: A run that never produced an exit code says none rather than zero, which would
		// read as success.
		Name:      "no exit code",
		Record:    outcome.Record{RunID: "run_c", Status: "failed", LogSHA256: "abc"},
		WantLines: []string{"run run_c failed (exit none)"},
		DenyLines: []string{"exit 0"},
	}, { // Test 3: An install with no rules recorded says so in as many words.
		Name: "no rules in force",
		Record: outcome.Record{
			RunID: "run_d", Status: "succeeded", ExitCode: &exit0, LogSHA256: "abc",
			PolicySet: &run.PolicySet{Digest: "0123456789abcdefdeadbeef", Count: 0},
		},
		WantLines: []string{"none recorded at submit", "0123456789ab"},
	}, { // Test 4: The rules that were in force are named, not just counted.
		Name: "rules in force",
		Record: outcome.Record{
			RunID: "run_e", Status: "succeeded", ExitCode: &exit0, LogSHA256: "abc",
			PolicySet: &run.PolicySet{
				Digest: "fedcba9876543210cafebabe", Count: 2,
				Rules: []string{"hold production", "hold terraform destroy"},
			},
		},
		WantLines: []string{"rules in force 2", "- hold production", "- hold terraform destroy"},
	}, { // Test 5: A coordinator renders its children, or the receipt reports nothing happening
		// over work that ran on real machines.
		Name: "coordinator",
		Record: outcome.Record{
			RunID: "run_f", Status: "failed", ExitCode: &exit2, LogSHA256: "abc",
			Children: []outcome.RecordChild{
				{RunID: "run_f1", Name: "migrate", Status: "succeeded", ExitCode: &exit0},
				{RunID: "run_f2", Index: &shard, Status: "failed", ExitCode: &exit2, Attempt: 2},
			},
		},
		WantLines: []string{"child migrate", "run run_f2", "shard 1", "attempt 2"},
	}, { // Test 6: The commit and the spec digest name the content that actually ran.
		Name: "pinned content",
		Record: outcome.Record{
			RunID: "run_g", Status: "succeeded", ExitCode: &exit0, LogSHA256: "abc",
			CommitSHA: "deadbeefcafe", SpecDigest: "sha256:1234", Image: "creator-ee@sha256:aa",
		},
		WantLines: []string{"commit         deadbeefcafe", "spec digest    sha256:1234",
			"image          creator-ee@sha256:aa"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(test.Record)
			if err != nil {
				t.Fatalf("%s: Marshal() error = %v", test.Name, err)
			}
			var out strings.Builder
			printOutcome(&out, body)
			got := out.String()
			for _, want := range test.WantLines {
				if !strings.Contains(got, want) {
					t.Errorf("%s: the printed outcome is missing %q:\n%s", test.Name, want, got)
				}
			}
			for _, deny := range test.DenyLines {
				if strings.Contains(got, deny) {
					t.Errorf("%s: the printed outcome claims %q when it must not:\n%s",
						test.Name, deny, got)
				}
			}
		})
	}
}

// TestPrintOutcomeSaysNothingAboutABodyItCannotRead pins the parse failure. The body is only printed
// once its digest has been held against the chain, so a body that will not parse is a shape nothing
// should be asserted about: printing a partial reading of it would put unverified claims under a
// VERIFIED heading.
func TestPrintOutcomeSaysNothingAboutABodyItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body []byte
	}{{ // Test 0: Bytes that are not JSON.
		Name: "not json", Body: []byte("what happened is anybody's guess"),
	}, { // Test 1: JSON that stops partway.
		Name: "truncated", Body: []byte(`{"run_id":"run_a"`),
	}, { // Test 2: An empty body.
		Name: "empty", Body: nil,
	}, { // Test 3: A JSON value of the wrong shape.
		Name: "wrong shape", Body: []byte(`["run_a"]`),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			printOutcome(&out, test.Body)
			if got := out.String(); got != "" {
				t.Errorf("%s: printOutcome wrote %q for a body it could not read; an unreadable "+
					"outcome must produce no claims", test.Name, got)
			}
		})
	}
}

// TestDesktopDataDirIsPrivateToTheUser pins the mode on the desktop data directory. It holds the
// database, which carries hashed tokens, sealed credentials, and the audit chain, so any other
// account on the machine being able to read or enter it would put the whole install in reach of
// anyone with a login.
func TestDesktopDataDirIsPrivateToTheUser(t *testing.T) {
	// Not parallel: t.Setenv redirects the user configuration directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("AppData", filepath.Join(home, "config"))

	dir, err := desktopDataDir()
	if err != nil {
		t.Fatalf("desktopDataDir() error = %v", err)
	}
	if !strings.HasPrefix(dir, home) {
		t.Fatalf("desktopDataDir() = %q, which is outside the redirected home %q", dir, home)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the desktop data directory is mode %04o, want 0700: it holds hashed tokens, "+
			"sealed credentials, and the audit chain", perm)
	}

	// A rerun over a directory somebody widened restores the private mode rather than accepting it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	if _, err := desktopDataDir(); err != nil {
		t.Fatalf("desktopDataDir() second call error = %v", err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("a widened data directory stayed at mode %04o; the desktop app has to restrict it "+
			"on every launch", perm)
	}
}

// TestOpenWhenReadyRespectsTheHeadlessSwitch pins the escape hatch for a headless or remote run.
// The desktop command launches a browser, and on a machine with no display that would either hang
// or spawn a process nobody sees, so the documented variable has to turn it off before anything is
// dialed or launched.
func TestOpenWhenReadyRespectsTheHeadlessSwitch(t *testing.T) {
	// Not parallel: t.Setenv mutates the process environment.
	t.Setenv("SWITCHTENDER_DESKTOP_NO_BROWSER", "1")
	done := make(chan struct{})
	go func() {
		// An address nothing listens on. Without the switch this would dial for ten seconds.
		openWhenReady("127.0.0.1:1", "http://127.0.0.1:1/ui/")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("openWhenReady kept trying with the headless switch set, so a headless run stalls " +
			"on a browser it was told not to open")
	}
}

// TestWitnessRefusesAServerItCannotWatch pins the witness command's own flag validation. The witness
// exists to watch a server from outside it, so a missing or unusable target is a misconfiguration
// that has to be refused: a witness that starts and watches nothing is the failure mode where the
// process is up, the operator believes they are covered, and a truncation goes unobserved.
func TestWitnessRefusesAServerItCannotWatch(t *testing.T) {
	tests := []struct {
		Name   string
		Server string
	}{{ // Test 0: No server at all.
		Name: "empty",
	}, { // Test 1: A scheme the witness cannot fetch over.
		Name: "ftp", Server: "ftp://st.example",
	}, { // Test 2: A file URL has no host to watch.
		Name: "file", Server: "file:///tmp/x",
	}, { // Test 3: A bare hostname names no scheme.
		Name: "no scheme", Server: "st.example",
	}, { // Test 4: A scheme with no host names nothing.
		Name: "no host", Server: "https://",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the witness command reads package-level flag variables.
			setString(t, &witnessServer, test.Server)
			setString(t, &witnessKeyDir, t.TempDir())
			setString(t, &witnessState, filepath.Join(t.TempDir(), "checkpoint.json"))
			if err := runWitness(testCommand(), nil); err == nil {
				t.Errorf("%s: runWitness() with --server %q = nil error; a witness watching nothing "+
					"reports nothing while the operator believes they are covered",
					test.Name, test.Server)
			}
		})
	}
}

// TestMCPRefusesAServerItCannotAddress pins the MCP command's client construction. The agent's every
// call goes to this URL, so an unusable one has to be refused at startup rather than after an agent
// has connected and started proposing runs that go nowhere.
func TestMCPRefusesAServerItCannotAddress(t *testing.T) {
	tests := []struct {
		Name   string
		Server string
		Token  string
	}{{ // Test 0: No server at all.
		Name: "empty server", Token: "st_token",
	}, { // Test 1: A scheme the client cannot speak.
		Name: "bad scheme", Server: "ftp://st.example", Token: "st_token",
	}, { // Test 2: A server with no token cannot authenticate any call it makes.
		Name: "no token", Server: "https://st.example",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the mcp command reads package-level flag variables and the environment.
			setString(t, &mcpServer, test.Server)
			setString(t, &mcpToken, test.Token)
			setDuration(t, &mcpTimeout, time.Second)
			t.Setenv(mcpTokenEnv, "")
			t.Setenv(mcpFallbackTokenEnv, "")
			if err := runMCP(testCommand(), nil); err == nil {
				t.Errorf("%s: runMCP() = nil error; the agent would connect to a gate that cannot "+
					"reach the server", test.Name)
			}
		})
	}
}
