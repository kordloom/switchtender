package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestArgsExtraVarsJSON(t *testing.T) {
	t.Parallel()
	args, err := playbookArgs(Spec{Playbook: "p.yml", ExtraVars: map[string]any{"version": "1.2.3"}})
	if err != nil {
		t.Fatalf("playbookArgs() error = %v", err)
	}
	want := []string{"--extra-vars", `{"version":"1.2.3"}`, "--", "p.yml"}
	if diff := cmp.Diff(want, args); diff != "" {
		t.Errorf("args mismatch (-want +got):\n%s", diff)
	}
}

func TestArgsExtraVarsMarshalError(t *testing.T) {
	t.Parallel()
	// A channel cannot be JSON encoded. Building the args must fail rather than silently run the
	// playbook without the extra vars, which could drop a variable gating a destructive task.
	_, err := playbookArgs(Spec{Playbook: "p.yml", ExtraVars: map[string]any{"bad": make(chan int)}})
	if err == nil {
		t.Fatal("playbookArgs() with unmarshalable extra vars = nil error, want failure")
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
