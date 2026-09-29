package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestAnsibleRunnerRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name            string
		Binary          string
		Spec            Spec
		WantExit        int
		Want            error
		WantOutContains string
	}{
		{ // Test 0: Process exits zero.
			Name: "success", Binary: "true", Spec: Spec{Playbook: "ignored"}, WantExit: 0,
		},
		{ // Test 1: Process exits non-zero; run completed, not an executor error.
			Name: "playbook failed", Binary: "false", Spec: Spec{Playbook: "ignored"}, WantExit: 1,
		},
		{ // Test 2: Binary cannot be launched.
			Name: "missing binary", Binary: "switchtender-no-such-binary",
			Spec: Spec{Playbook: "ignored"}, WantExit: -1, Want: ErrLaunch,
		},
		{ // Test 3: Spec without a playbook is rejected before launch.
			Name: "no playbook", Binary: "true", Spec: Spec{}, WantExit: -1, Want: ErrNoPlaybook,
		},
		{ // Test 4: Combined output is streamed to the writer.
			Name: "streams output", Binary: "echo",
			Spec: Spec{Playbook: "hello-from-yard"}, WantExit: 0, WantOutContains: "hello-from-yard",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runner := NewAnsibleRunner(WithBinary(test.Binary))
			var buf bytes.Buffer

			res, err := runner.Run(context.Background(), test.Spec, &buf)

			if test.Want != nil {
				if !errors.Is(err, test.Want) {
					t.Fatalf("Run() error = %v, want %v", err, test.Want)
				}
			} else if err != nil {
				t.Fatalf("Run() unexpected error = %v", err)
			}
			if res.ExitCode != test.WantExit {
				t.Errorf("ExitCode = %d, want %d", res.ExitCode, test.WantExit)
			}
			if test.WantOutContains != "" && !strings.Contains(buf.String(), test.WantOutContains) {
				t.Errorf("output %q does not contain %q", buf.String(), test.WantOutContains)
			}
		})
	}
}

func TestAnsibleRunnerCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner := NewAnsibleRunner(WithBinary("sleep"))
	var buf bytes.Buffer
	res, err := runner.Run(ctx, Spec{Playbook: "5"}, &buf)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v, want context.Canceled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestAnsibleRunnerArgs(t *testing.T) {
	t.Parallel()
	got, err := playbookArgs(Spec{Playbook: "site.yml", Inventory: "hosts.ini"})
	if err != nil {
		t.Fatalf("playbookArgs() error = %v", err)
	}
	want := []string{"-i", "hosts.ini", "--", "site.yml"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}

	gotLimit, err := playbookArgs(Spec{Playbook: "site.yml", Inventory: "hosts.ini", Limit: "web01,web02"})
	if err != nil {
		t.Fatalf("playbookArgs() error = %v", err)
	}
	wantLimit := []string{"-i", "hosts.ini", "--limit", "web01,web02", "--", "site.yml"}
	if strings.Join(gotLimit, " ") != strings.Join(wantLimit, " ") {
		t.Errorf("args with limit = %v, want %v", gotLimit, wantLimit)
	}
}

// TestArgsExtraVarsGoInAFileNotOnArgv checks a run's own extra vars reach ansible-playbook through a
// private file rather than on the command line.
//
// Every credential-derived value already took that route, deliberately, so the password stays off
// argv. The run's own vars did not, and a template survey collects them with no password field type,
// so a secret is collected as ordinary text. ps auxww showed them to any local account on the
// executor for the life of the run, and for a container run the same argv became the docker command
// line and stayed in the container's Config.Cmd for docker inspect to read afterward.
func TestArgsExtraVarsGoInAFileNotOnArgv(t *testing.T) {
	t.Parallel()
	spec := Spec{Playbook: "p.yml", ExtraVars: map[string]any{"db_password": "hunter2"}}

	cleanup, err := materializeExtraVars(&spec)
	if err != nil {
		t.Fatalf("materializeExtraVars() error = %v", err)
	}
	defer cleanup()

	args, err := playbookArgs(spec)
	if err != nil {
		t.Fatalf("playbookArgs() error = %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "hunter2") {
			t.Fatalf("the value is on argv, where ps shows it to any local account: %v", args)
		}
	}
	if len(spec.ExtraVarsFiles) != 1 {
		t.Fatalf("extra vars files = %v, want the one just written", spec.ExtraVarsFiles)
	}
	path := spec.ExtraVarsFiles[0]
	want := []string{"--extra-vars", "@" + path, "--", "p.yml"}
	if diff := cmp.Diff(want, args); diff != "" {
		t.Errorf("args mismatch (-want +got):\n%s", diff)
	}

	// The vars still reach the playbook, and the file is not readable by other accounts.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != `{"db_password":"hunter2"}` {
		t.Errorf("vars file holds %q, want the extra vars", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("vars file mode = %o, want 600", perm)
	}

	// Cleanup removes it, so a run does not leave its variables on the executor's disk.
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the vars file outlived the run: %v", err)
	}
}

func TestArgsExtraVarsMarshalError(t *testing.T) {
	t.Parallel()
	// A channel cannot be JSON encoded. Materializing must fail rather than silently run the
	// playbook without the extra vars, which could drop a variable gating a destructive task.
	spec := Spec{Playbook: "p.yml", ExtraVars: map[string]any{"bad": make(chan int)}}
	if _, err := materializeExtraVars(&spec); err == nil {
		t.Fatal("materializeExtraVars() with unmarshalable extra vars = nil error, want failure")
	}
}

func TestArgsVaultPasswords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Vaults are the passwords on the spec.
		Vaults []VaultPassword
		// WantArgs are the vault flags expected, in order, before the playbook separator.
		WantArgs []string
	}{{ // Test 0: An unlabeled password uses the classic flag.
		Name:     "unlabeled",
		Vaults:   []VaultPassword{{Path: "/t/vault-default"}},
		WantArgs: []string{"--vault-password-file", "/t/vault-default"},
	}, { // Test 1: A labeled password uses --vault-id so its label selects its secrets.
		Name:     "labeled",
		Vaults:   []VaultPassword{{Label: "prod", Path: "/t/vault-prod"}},
		WantArgs: []string{"--vault-id", "prod@/t/vault-prod"},
	}, { // Test 2: Several passwords on one run each pass their own flag, in order.
		Name: "several",
		Vaults: []VaultPassword{
			{Label: "prod", Path: "/t/prod"},
			{Path: "/t/plain"},
			{Label: "dev", Path: "/t/dev"},
		},
		WantArgs: []string{
			"--vault-id", "prod@/t/prod",
			"--vault-password-file", "/t/plain",
			"--vault-id", "dev@/t/dev",
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			args, err := playbookArgs(Spec{Playbook: "p.yml", VaultPasswords: test.Vaults})
			if err != nil {
				t.Fatalf("playbookArgs() error = %v", err)
			}
			want := append(append([]string{}, test.WantArgs...), "--", "p.yml")
			if diff := cmp.Diff(want, args); diff != "" {
				t.Errorf("args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestArgsTagsVerbosityForksDiff pins the AWX-parity run controls onto the ansible-playbook command
// line: tags and skip-tags as single comma-joined values, forks and diff, and verbosity as one -v
// per level capped at four. They apply only to the Ansible builder, so no other tool sees them.
func TestArgsTagsVerbosityForksDiff(t *testing.T) {
	t.Parallel()
	args, err := playbookArgs(Spec{
		Playbook: "site.yml", Tags: []string{"deploy", "config"}, SkipTags: []string{"slow"},
		Forks: 12, DiffMode: true, Verbosity: 3,
	})
	if err != nil {
		t.Fatalf("playbookArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--tags deploy,config", "--skip-tags slow", "--forks 12", "--diff", "-vvv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}

	// Verbosity above four is clamped to four v's, the most ansible-playbook accepts.
	capped, _ := playbookArgs(Spec{Playbook: "p.yml", Verbosity: 9})
	if !strings.Contains(strings.Join(capped, " "), "-vvvv") || strings.Contains(strings.Join(capped, " "), "-vvvvv") {
		t.Errorf("verbosity 9 rendered %v, want it clamped to -vvvv", capped)
	}

	// Zero and empty values emit nothing, so an ordinary run is unchanged.
	plain, _ := playbookArgs(Spec{Playbook: "p.yml"})
	for _, absent := range []string{"--tags", "--skip-tags", "--forks", "--diff", "-v"} {
		if strings.Contains(strings.Join(plain, " "), absent) {
			t.Errorf("plain run emitted %q: %v", absent, plain)
		}
	}
}

// TestContainerPlanKeepsExtraVarsOffTheDockerCommandLine covers the wiring rather than the helper.
//
// For a container run the playbook argv becomes the docker run command line, and the container keeps
// it in Config.Cmd, so docker inspect shows it after the run has finished. A survey answer collected
// as ordinary text, which is the only kind there is, was therefore readable on the executor long
// after the run ended.
func TestContainerPlanKeepsExtraVarsOffTheDockerCommandLine(t *testing.T) {
	t.Parallel()
	spec := Spec{
		Tool: "ansible", Playbook: "p.yml", Dir: t.TempDir(),
		ExtraVars: map[string]any{"db_password": "hunter2"},
	}

	plan, cleanup, err := toolContainerPlan(spec)
	if err != nil {
		t.Fatalf("toolContainerPlan() error = %v", err)
	}
	defer cleanup()

	for _, a := range plan.argv {
		if strings.Contains(a, "hunter2") {
			t.Fatalf("the value is in the container command line, which docker inspect keeps: %v",
				plan.argv)
		}
	}

	// It still reaches the playbook: the file is referenced and mounted into the container.
	var ref string
	for i, a := range plan.argv {
		if a == "--extra-vars" && i+1 < len(plan.argv) && strings.HasPrefix(plan.argv[i+1], "@") {
			ref = strings.TrimPrefix(plan.argv[i+1], "@")
		}
	}
	if ref == "" {
		t.Fatalf("no extra vars file is referenced, so the variables were dropped: %v", plan.argv)
	}
	var mounted bool
	for _, m := range plan.mounts {
		if m.path == ref {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("the vars file %q is not mounted, so the playbook cannot read it", ref)
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "hunter2") {
		t.Errorf("the vars file lost the value: %s", data)
	}
}
