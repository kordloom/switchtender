package tfscan

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// rtDeepNestChildEnv marks the child process that runs the parse, so a stack overflow is contained
// there rather than taking down the whole package test binary.
const rtDeepNestChildEnv = "RT_TFSCAN_DEEPNEST_CHILD"

// TestScanDeeplyNestedHCLDoesNotCrashTheProcess pins that the plan gate's HCL reader bounds parse
// nesting, so a single configuration file cannot crash the process that scans it.
//
// tfscan parses each file with hclsyntax.ParseConfig or hcljson.Parse, each recursive descent with no
// depth guard, under a per-file byte ceiling but with no bound on nesting. About a million open
// brackets, well under a megabyte, drive either parser past Go's 1 GB goroutine-stack limit and abort
// the process with "fatal error: stack overflow", which no recover and no net/http per-request
// recover catches. The scan runs synchronously on the review webhook path and on the plan gate for
// any Terraform or OpenTofu run, so one crafted file takes the server down. A cheap pre-pass now
// bounds bracket nesting before the parser runs and leaves a file past the bound unread.
//
// Both HCL syntaxes are covered. The scan runs in a child process because the crash is a fatal
// runtime error: the parent asserts the child exits cleanly.
func TestScanDeeplyNestedHCLDoesNotCrashTheProcess(t *testing.T) {
	if os.Getenv(rtDeepNestChildEnv) == "1" {
		deep := strings.Repeat("[", 1_000_000)
		files := map[string]string{
			"main.tf":      "a = " + deep,
			"main.tf.json": `{"x": ` + deep,
		}
		for name, body := range files {
			// A bounded reader returns a result and the child exits 0. An unbounded one overflows the
			// stack and the child dies with a non-zero status.
			_ = Scan(mapFS(map[string]string{name: body}), ".",
				Options{Tool: run.ToolTerraform, Place: "the project"})
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestScanDeeplyNestedHCLDoesNotCrashTheProcess$")
	cmd.Env = append(os.Environ(), rtDeepNestChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scanning a deeply nested HCL file crashed the scanning process, so a plan gate or "+
			"review webhook reading one is a denial of service: %v\nlast output:\n%s",
			err, lastLines(string(out), 12))
	}
}

// TestScanReadsOrdinaryNesting is the negative control: the nesting bound must not refuse a file that
// nests a handful of levels, which every real configuration does. The external data source inside the
// nested blocks is still found, so the file is parsed, not left unread.
func TestScanReadsOrdinaryNesting(t *testing.T) {
	t.Parallel()
	body := "locals {\n  m = {\n    a = [{ b = [1, 2, 3] }]\n  }\n}\n" +
		"data \"external\" \"lookup\" {\n  program = [\"sh\", \"-c\", \"echo {}\"]\n}\n"
	got := Scan(mapFS(map[string]string{"main.tf": body}), ".",
		Options{Tool: run.ToolTerraform, Place: "the project"})
	if got.Classification != run.DryRunNotChangeFree {
		t.Errorf("classification = %q, want not_change_free; the nesting bound must not leave an "+
			"ordinary file unread (findings %q, unread %q)", got.Classification, got.Findings, got.Unread)
	}
}

// lastLines returns the final n lines of s, for a crash message that is otherwise a whole stack dump.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
