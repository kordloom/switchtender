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
			Name: "missing binary", Binary: "yardmaster-no-such-binary",
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
	got := playbookArgs(Spec{Playbook: "site.yml", Inventory: "hosts.ini"})
	want := []string{"-i", "hosts.ini", "site.yml"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}

	gotLimit := playbookArgs(Spec{Playbook: "site.yml", Inventory: "hosts.ini", Limit: "web01,web02"})
	wantLimit := []string{"-i", "hosts.ini", "--limit", "web01,web02", "site.yml"}
	if strings.Join(gotLimit, " ") != strings.Join(wantLimit, " ") {
		t.Errorf("args with limit = %v, want %v", gotLimit, wantLimit)
	}
}

func TestArgsExtraVarsJSON(t *testing.T) {
	t.Parallel()
	args := playbookArgs(Spec{Playbook: "p.yml", ExtraVars: map[string]any{"version": "1.2.3"}})
	want := []string{"--extra-vars", `{"version":"1.2.3"}`, "p.yml"}
	if diff := cmp.Diff(want, args); diff != "" {
		t.Errorf("args mismatch (-want +got):\n%s", diff)
	}
}
