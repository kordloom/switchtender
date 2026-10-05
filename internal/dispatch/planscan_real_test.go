package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// probeScript is the program an external data source names in the real plan tests. It leaves a
// file beside itself, which is the evidence that a plan ran it, and answers the empty object the
// external protocol expects.
const probeScript = "#!/bin/sh\ntouch \"$(dirname \"$0\")/ran\"\nprintf '{}'\n"

// probeConfig declares an external data source running probeScript.
const probeConfig = "data \"external\" \"probe\" {\n" +
	"  program = [\"sh\", \"${path.module}/probe.sh\"]\n}\n"

// standDownOrFail skips on a machine allowed to lack something, and fails where the suite was
// demanded whole.
func standDownOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
		t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and "+format, args...)
	}
	t.Skipf(format, args...)
}

// requireExternalProvider stands the test down unless binary is installed and can install the
// external provider, which a plan of probeConfig needs: from its registry, or from the provider
// mirror a CLI configuration file names, the way an install without network access provides one.
func requireExternalProvider(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		standDownOrFail(t, "%s is not on PATH, so the real plan cannot run", binary)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(probeConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cmd := exec.Command(binary, "init", "-input=false", "-no-color")
	cmd.Dir = dir
	// The version check would call the vendor's service, so it is off, as the runner turns it off.
	cmd.Env = append(os.Environ(), "CHECKPOINT_DISABLE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		standDownOrFail(t, "%s could not install the external provider, from its registry or a "+
			"mirror named by TF_CLI_CONFIG_FILE, so the real plan cannot run: %v\n%s", binary, err, out)
	}
}

// TestARealPlanRunsTheProgramTheGateHeldItFor proves the premise the scan rests on, with the real
// tools: a plan runs the program an external data source names. Held by the gate, nothing runs and
// the program leaves no trace. Released by a person, the plan runs it, which is exactly what the
// exemption would have let happen with nobody asked. A configuration that runs nothing plans
// straight through, so the gate costs a change-free plan nothing.
//
//nolint:funlen // Test function.
func TestARealPlanRunsTheProgramTheGateHeldItFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Tool is the tool the run names.
		Tool string
		// Binary is the executable that tool runs.
		Binary string
	}{{ // Test 0: Terraform.
		Tool: run.ToolTerraform, Binary: "terraform",
	}, { // Test 1: OpenTofu.
		Tool: run.ToolOpenTofu, Binary: "tofu",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			requireExternalProvider(t, test.Binary)
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, roundhouse.NewAnsibleRunner(), zap.NewNop(),
				WithPolicies(rulesHolding(t, excludePlans(test.Tool))), WithNoJanitor())
			t.Cleanup(d.Close)

			clean := t.TempDir()
			writeFiles(t, clean, map[string]string{"main.tf": cleanConfig})
			planned, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(clean),
				run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit(clean) error = %v", err)
			}
			if planned.Status == run.StatusPendingApproval {
				t.Fatalf("a change-free plan was held: %q", planned.DryRunFindings())
			}
			if done := waitTerminal(t, store, planned.ID); done.Status != run.StatusSucceeded {
				t.Fatalf("the change-free plan ended %q: %s", done.Status, done.Error)
			}

			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": probeConfig, "probe.sh": probeScript})
			marker := filepath.Join(dir, "ran")
			held, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(dir),
				run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit(external) error = %v", err)
			}
			if held.Status != run.StatusPendingApproval {
				t.Fatalf("a plan running a program was not held: status %q", held.Status)
			}
			if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the program ran while the plan was held: %v", err)
			}
			if _, err := d.Approve(ctx, held.ID, decider("approver", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			done := waitTerminal(t, store, held.ID)
			if done.Status != run.StatusSucceeded {
				t.Fatalf("the approved plan ended %q: %s", done.Status, done.Error)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("the approved plan did not run the program, so the premise the gate "+
					"holds on is wrong: %v", err)
			}
		})
	}
}
