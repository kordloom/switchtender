package roundhouse

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestShellExportSourcesBackExactly proves the env file this runner writes is safe to source in a
// POSIX shell: an adversarial secret value round-trips through sh byte for byte, and its shell
// metacharacters are never executed. This is the property the earlier --env-file delivery did not
// need but the sourced-file delivery must have, and getting it wrong would either corrupt a
// credential or run an injected command.
func TestShellExportSourcesBackExactly(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell to source against")
	}
	tests := []struct {
		Name  string
		Value string
	}{
		{Name: "plain", Value: "simple"},
		{Name: "spaces", Value: "two words"},
		{Name: "single quote", Value: "O'Brien's key"},
		{Name: "double quote", Value: `a "quoted" value`},
		{Name: "dollar and braces", Value: "$HOME ${PATH} plain"},
		{Name: "command substitution", Value: "before $(touch /tmp/switchtender-pwned) after"},
		{Name: "backticks", Value: "pre `id` post"},
		{Name: "backslash", Value: `a\tb\nc`},
		{Name: "newline", Value: "line1\nline2"},
		{Name: "semicolons and amps", Value: "a; rm -rf x && echo y | cat"},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			line := shellExport("SECRET=" + test.Value)
			dir := t.TempDir()
			path := filepath.Join(dir, "env")
			if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			// Source the file exactly as the container wrapper does, then print the recovered value
			// with no interpretation so the comparison is byte-exact.
			out, err := exec.Command(sh, "-c", `. "$1"; printf %s "$SECRET"`, "sh", path).Output()
			if err != nil {
				t.Fatalf("sourcing env line %q error = %v", line, err)
			}
			if diff := cmp.Diff(test.Value, string(out)); diff != "" {
				t.Errorf("test %d: sourced value mismatch (-want +got):\n%s", testNum, diff)
			}
			// A command-substitution payload must not have run.
			if _, statErr := os.Stat("/tmp/switchtender-pwned"); statErr == nil {
				_ = os.Remove("/tmp/switchtender-pwned")
				t.Errorf("test %d: the value was executed as a command", testNum)
			}
		})
	}
}

// TestShellExportSkipsNonAssignments checks an entry with no '=' or an empty key is dropped rather
// than emitted as a broken shell statement.
func TestShellExportSkipsNonAssignments(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "notanassignment", "=novalue"} {
		if got := shellExport(in); got != "" {
			t.Errorf("shellExport(%q) = %q, want empty", in, got)
		}
	}
}

// TestRunArgsMountsAndSourcesEnv checks the run mounts the env file read-only and sources it in a
// shell wrapper instead of passing --env-file, so a resolved secret never lands in the container's
// Config.Env where docker inspect would return it.
func TestRunArgsMountsAndSourcesEnv(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{Playbook: "/work/site.yml", Dir: "/work", Image: "alpine"}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()

	args, err := c.runArgs(spec, plan, "ym-test", "/tmp/switchtender-env-xyz")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--env-file") {
		t.Error("runArgs() passes --env-file, which exposes secrets to docker inspect")
	}
	if !strings.Contains(joined, "-v /tmp/switchtender-env-xyz:/tmp/switchtender-env-xyz:ro") {
		t.Errorf("env file not mounted read-only: %v", args)
	}
	// The command after the image must be the shell wrapper that sources the env path.
	img := slices.Index(args, "alpine")
	if img == -1 || img+4 >= len(args) {
		t.Fatalf("image and wrapper not present: %v", args)
	}
	wrapper := args[img+1 : img+6]
	want := []string{"sh", "-c", `. "$1"; shift; exec "$@"`, "sh", "/tmp/switchtender-env-xyz"}
	if diff := cmp.Diff(want, wrapper); diff != "" {
		t.Errorf("env-sourcing wrapper mismatch (-want +got):\n%s", diff)
	}
}

// TestRunArgsNoEnvNoWrapper checks a run with no environment runs the tool directly, with no shell
// wrapper and no env mount, so an image without a shell is only constrained when there is env to
// source.
func TestRunArgsNoEnvNoWrapper(t *testing.T) {
	t.Parallel()
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	spec := Spec{Playbook: "/work/site.yml", Dir: "/work", Image: "alpine"}
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()

	// An empty env file path is what writeEnvFile returns when nothing is injected.
	args, err := c.runArgs(spec, plan, "ym-test", "")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	img := slices.Index(args, "alpine")
	if img == -1 || img+1 >= len(args) || args[img+1] != "ansible-playbook" {
		t.Errorf("a no-env run should exec the tool directly after the image: %v", args)
	}
	if slices.Contains(args, "sh") {
		t.Errorf("a no-env run should carry no shell wrapper: %v", args)
	}
}

// TestShellExportRefusesAnUnsafeName pins that a variable name carrying shell metacharacters never
// reaches the env file.
//
// The value is single quoted, so no value can break out. A name cannot be quoted, because quoting it
// would stop the line being an assignment, so an unsafe name has to be refused instead. Names are not
// always the product's own: a custom credential type lets an operator choose the variable a secret
// injects into, and an import reads those definitions out of a file written by another system.
func TestShellExportRefusesAnUnsafeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Entry   string
		Emitted bool
	}{
		{Name: "ordinary name", Entry: "ANSIBLE_HOST_KEY_CHECKING=False", Emitted: true},
		{Name: "leading underscore", Entry: "_PRIVATE=x", Emitted: true},
		{Name: "digits after the first character", Entry: "AWS_S3_V4=1", Emitted: true},
		{Name: "command separator in the name", Entry: "FOO;touch /tmp/pwned=1", Emitted: false},
		{Name: "substitution in the name", Entry: "FOO$(id)=1", Emitted: false},
		{Name: "backtick in the name", Entry: "FOO`id`=1", Emitted: false},
		{Name: "space in the name", Entry: "FOO BAR=1", Emitted: false},
		{Name: "newline in the name", Entry: "FOO\nrm -rf /=1", Emitted: false},
		{Name: "leading digit is not a shell name", Entry: "9LIVES=1", Emitted: false},
		{Name: "empty name", Entry: "=novalue", Emitted: false},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got := shellExport(test.Entry)
			if emitted := got != ""; emitted != test.Emitted {
				t.Errorf("shellExport(%q) = %q, emitted %v, want %v", test.Entry, got, emitted,
					test.Emitted)
			}
		})
	}
}

// TestEnvFileSourcesSafelyWithAHostileName runs a real shell over an env file built from a hostile
// name beside an honest one, and requires the honest value to survive while the hostile line never
// executes. Reasoning about escaping is how escaping bugs survive review, so this asks sh.
func TestEnvFileSourcesSafelyWithAHostileName(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell to source against")
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	lines := []string{
		shellExport("GOOD=kept"),
		shellExport("EVIL;touch " + marker + "=1"),
	}
	var body strings.Builder
	for _, l := range lines {
		if l != "" {
			body.WriteString(l + "\n")
		}
	}
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out, err := exec.Command(sh, "-c", `. "$1"; printf %s "$GOOD"`, "sh", path).Output()
	if err != nil {
		t.Fatalf("sourcing the env file error = %v", err)
	}
	if string(out) != "kept" {
		t.Errorf("GOOD = %q, want kept; the honest value must survive", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the hostile name executed a command when the env file was sourced")
	}
}
