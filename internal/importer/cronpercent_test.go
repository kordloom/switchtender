package importer

import (
	"fmt"
	"os/exec"
	"testing"
)

// TestACrontabCommandRunsAsCronWouldRunIt pins the crontab percent sign. Cron reads "\%" as a literal
// percent and the first unescaped "%" as the start of the command's standard input, with each later
// one a newline. The command was imported verbatim, so a backup dated with $(date +"\%Y\%m\%d") wrote
// a file with backslashes in its name. Each case runs the imported script in bash and compares the
// output to what cron produces for the same line.
func TestACrontabCommandRunsAsCronWouldRunIt(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	tests := []struct {
		Line string
		Want string
	}{
		{Line: `echo "load 100\%"`, Want: "load 100%\n"},                            // Test 0: An escaped percent is a percent.
		{Line: `echo "db_\%Y\%m\%d.sql"`, Want: "db_%Y%m%d.sql\n"},                  // Test 1: Escapes in a date format.
		{Line: `cat%line one%line two`, Want: "line one\nline two\n"},               // Test 2: Input after the first percent.
		{Line: `echo first; cat%fed`, Want: "first\nfed\n"},                         // Test 3: Input reaches the whole command.
		{Line: `cat%literal $HOME and \% sign`, Want: "literal $HOME and % sign\n"}, // Test 4: Input is not expanded.
		{Line: `echo plain`, Want: "plain\n"},                                       // Test 5: No percent at all.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command("bash", "-c", cronCommand(test.Line)).Output()
			if err != nil {
				t.Fatalf("bash -c %q error = %v", cronCommand(test.Line), err)
			}
			if string(out) != test.Want {
				t.Errorf("the crontab line %q printed %q, want %q", test.Line, out, test.Want)
			}
		})
	}
}
