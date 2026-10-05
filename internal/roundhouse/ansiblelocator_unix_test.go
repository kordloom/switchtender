//go:build !windows

package roundhouse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
)

// writeFakeAnsible writes stand-ins for the Ansible commands into dir: ansible-playbook prints the
// directory it was started from, and ansible-inventory answers --version with release.
func writeFakeAnsible(t *testing.T, dir, release string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scripts := map[string]string{
		"ansible":           "#!/bin/sh\necho 'ansible [core " + release + "]'\n",
		"ansible-playbook":  "#!/bin/sh\necho \"playbook from $(dirname \"$0\")\"\n",
		"ansible-inventory": "#!/bin/sh\necho 'ansible-inventory [core " + release + "]'\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// TestHostRunnerStartsTheLocatedAnsible pins that the host runner starts ansible-playbook and
// ansible-inventory from where its locator says, the managed runtime included, reports the version
// and the source from there, and never falls back to PATH when a configured directory lacks them.
func TestHostRunnerStartsTheLocatedAnsible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want        error
		WantSource  string
		WantVersion string
		Configured  bool
		Managed     bool
	}{{ // Test 0: A configured directory's commands run.
		Configured: true, WantSource: ansibleruntime.SourceConfigured, WantVersion: "2.20.9",
	}, { // Test 1: The managed runtime's commands run when nothing is configured.
		Managed: true, WantSource: ansibleruntime.SourceManaged, WantVersion: "2.21.4",
	}, { // Test 2: A configured directory without Ansible is missing Ansible, not PATH's.
		WantSource: ansibleruntime.SourceConfigured, Want: ErrAnsibleMissing,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			root := filepath.Join(base, "runtime")
			bin := filepath.Join(base, "configured")
			want := bin
			switch {
			case test.Configured:
				writeFakeAnsible(t, bin, "2.20.9")
			case test.Managed:
				bin = ""
				// The managed runtime's layout: an environment named for its release and the start of
				// its lock's digest, the marker a finished install writes, and current naming it.
				env := "2.21.4-" + strings.Repeat("0", 12)
				want = filepath.Join(root, env, "bin")
				writeFakeAnsible(t, want, "2.21.4")
				marker := `{"ansible_core":"2.21.4","python":"3.12.3","lock_sha256":"` +
					strings.Repeat("0", 64) + `"}`
				if err := os.WriteFile(filepath.Join(root, env, "switchtender-runtime.json"),
					[]byte(marker), 0o644); err != nil {
					t.Fatalf("write marker: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, "current"), []byte(env+"\n"),
					0o644); err != nil {
					t.Fatalf("write current: %v", err)
				}
			default:
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
			runner := NewAnsibleRunner(WithAnsibleLocator(ansibleruntime.NewLocator(bin, root)))
			cmds := runner.(AnsibleCommandsReporter).AnsibleCommands()
			if cmds.Source != test.WantSource {
				t.Errorf("AnsibleCommands().Source = %q, want %q", cmds.Source, test.WantSource)
			}
			version, err := runner.(AnsibleCoreReporter).AnsibleCoreVersion(context.Background())
			if !errors.Is(err, test.Want) {
				t.Fatalf("AnsibleCoreVersion() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantVersion, version); diff != "" {
				t.Errorf("AnsibleCoreVersion() mismatch (-want +got):\n%s", diff)
			}
			var out bytes.Buffer
			_, runErr := runner.Run(context.Background(), Spec{Playbook: "site.yml", Dir: base}, &out)
			if test.Want != nil {
				if runErr == nil {
					t.Errorf("Run() with no ansible-playbook in %s succeeded: %q", bin, out.String())
				}
				return
			}
			if runErr != nil || !strings.Contains(out.String(), "playbook from "+want) {
				t.Errorf("Run() = %v, output %q, want the playbook from %s", runErr, out.String(), want)
			}
		})
	}
}
