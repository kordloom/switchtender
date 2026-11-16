package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestNameAnsibleInstallAddsTheInstallLineOnlyToAMissingHostAnsible pins which failures gain the
// line that installs Ansible: a host Ansible run whose command the operating system did not find,
// and nothing else.
func TestNameAnsibleInstallAddsTheInstallLineOnlyToAMissingHostAnsible(t *testing.T) {
	t.Parallel()
	notFound := fmt.Errorf("%w: %w", roundhouse.ErrLaunch,
		&exec.Error{Name: "ansible-playbook", Err: exec.ErrNotFound})
	tests := []struct {
		// Spec is the run the failure came from.
		Spec roundhouse.Spec
		// In is the failure the runner returned.
		In error
		// WantHint is whether the returned failure names the install line.
		WantHint bool
	}{{ // Test 0: A host Ansible run with no Ansible names the install line.
		Spec: roundhouse.Spec{Playbook: "site.yml"}, In: notFound, WantHint: true,
	}, { // Test 1: The tool named outright reads the same as the default.
		Spec: roundhouse.Spec{Tool: run.ToolAnsible, Playbook: "site.yml"}, In: notFound,
		WantHint: true,
	}, { // Test 2: A run inside an image has its Ansible in the image.
		Spec: roundhouse.Spec{Playbook: "site.yml", Image: "ghcr.io/example/ee:1"}, In: notFound,
	}, { // Test 3: Another tool's missing command is not Ansible's.
		Spec: roundhouse.Spec{Tool: run.ToolBash, Command: "true"}, In: notFound,
	}, { // Test 4: Another launch failure is returned as it came.
		Spec: roundhouse.Spec{Playbook: "site.yml"},
		In:   fmt.Errorf("%w: permission denied", roundhouse.ErrLaunch),
	}, { // Test 5: No failure stays none.
		Spec: roundhouse.Spec{Playbook: "site.yml"},
	}}
	d := New(run.NewMemStore(), dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := d.nameAnsibleInstall(test.Spec, test.In)
			if !errors.Is(got, test.In) {
				t.Fatalf("nameAnsibleInstall() = %v, which no longer wraps %v", got, test.In)
			}
			hinted := got != nil && strings.Contains(got.Error(), inventory.AnsibleInstallCommand)
			if hinted != test.WantHint {
				t.Errorf("nameAnsibleInstall() = %v, names the install line = %t, want %t",
					got, hinted, test.WantHint)
			}
			if !test.WantHint && got != test.In {
				t.Errorf("nameAnsibleInstall() = %v, want the failure unchanged: %v", got, test.In)
			}
		})
	}
}

// TestARunWithNoAnsibleFailsNamingTheInstallLine pins the wiring: a run the real host runner
// cannot start, because its Ansible command is on no PATH, ends with an error that names how to
// install Ansible rather than only the operating system's words.
func TestARunWithNoAnsibleFailsNamingTheInstallLine(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := roundhouse.NewAnsibleRunner(
		roundhouse.WithBinary("switchtender-test-ansible-playbook-that-is-not-installed"))
	d := New(store, runner, zap.NewNop(), WithNoJanitor())
	defer d.Close()

	r, err := d.Submit(context.Background(), "site.yml", "localhost,")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	var done *run.Run
	waitFor(t, func() bool {
		got, gerr := store.Get(context.Background(), r.ID)
		if gerr != nil || !got.Status.Terminal() {
			return false
		}
		done = got
		return true
	})
	if done.Status != run.StatusFailed {
		t.Fatalf("status = %s, want failed", done.Status)
	}
	for _, want := range []string{"executable file not found", inventory.AnsibleInstallCommand} {
		if !strings.Contains(done.Error, want) {
			t.Errorf("run error %q lacks %q", done.Error, want)
		}
	}
}
