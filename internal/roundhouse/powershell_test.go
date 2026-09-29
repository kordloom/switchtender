package roundhouse

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
)

// TestPwshRunner exercises the PowerShell runner against a real pwsh: a passing script, a failing
// exit code, and a dry run that parses without executing. It skips where pwsh is not installed;
// CI's runners carry it.
func TestPwshRunner(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not installed")
	}
	tests := []struct {
		Name       string
		Command    string
		DryRun     bool
		WantExit   int
		WantOutput string
	}{{ // Test 0: A script runs and its output streams back.
		Name: "runs", Command: `Write-Output "hello from pwsh"`, WantExit: 0, WantOutput: "hello from pwsh",
	}, { // Test 1: A non-zero exit propagates.
		Name: "fails", Command: "exit 3", WantExit: 3,
	}, { // Test 2: A dry run parses without executing.
		Name: "dry run parses", Command: `Write-Output "must not print"`, DryRun: true, WantExit: 0,
	}, { // Test 3: A dry run reports a syntax error.
		Name: "dry run catches syntax", Command: "if (", DryRun: true, WantExit: 1,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			res, err := newPwshRunner(nil).Run(context.Background(),
				Spec{Command: test.Command, DryRun: test.DryRun}, &buf)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if res.ExitCode != test.WantExit {
				t.Errorf("exit = %d, want %d (out %q)", res.ExitCode, test.WantExit, buf.String())
			}
			if test.WantOutput != "" && !bytes.Contains(buf.Bytes(), []byte(test.WantOutput)) {
				t.Errorf("output %q missing %q", buf.String(), test.WantOutput)
			}
			if test.DryRun && test.WantExit == 0 && bytes.Contains(buf.Bytes(), []byte("must not print")) {
				t.Error("dry run executed the script")
			}
		})
	}
}
