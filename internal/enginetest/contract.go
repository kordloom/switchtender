// Package enginetest holds the contract between an execution engine's runner and the third-party
// tool that runner invokes.
//
// Every engine's dry run is a different feature of a different tool. Bash parses with -n, Python
// compiles with py_compile, Go vets instead of running, PowerShell builds a script block without
// invoking it, and Terraform plans with -detailed-exitcode. The runner's correctness therefore rests
// on facts about tools this project does not own, and those facts are not obviously true: they are
// assumptions about somebody else's exit codes and flags.
//
// Unit tests point the runners at stand-in binaries so they can run anywhere, which is right and is
// how argv construction gets tested at all. What a stand-in cannot do is tell you whether the
// assumption it models is true. The terraform stub asserts that a pending change exits 2. If that
// were ever false, drift detection would report no drift, the suite would stay green, and the failure
// would be a silent negative on a paid feature rather than an error anybody sees.
//
// So the assumptions are written down once, here, as data that can be run against any binary. A unit
// test runs them against the stand-in and learns whether the stand-in is faithful. CI runs them
// against the real tool and learns whether the assumption is true. Neither can drift from the other
// without one of them failing.
//
// This follows the same shape as the store contracts beside it: one behavior, several
// implementations, held to the same answers so they cannot diverge quietly.
package enginetest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// contractTimeout bounds one assumption. A tool that hangs is a failing assumption rather than a
// stuck suite, and terraform init on a module with no providers is the slowest thing here by far.
const contractTimeout = 90 * time.Second

// Assumption is one fact about a tool that an engine runner's correctness rests on.
type Assumption struct {
	// Name states the fact, in the form the runner depends on it being true.
	Name string
	// Why says what breaks when the fact is false, so a failure here is actionable without reading
	// the runner.
	Why string
	// Files are written into a scratch directory before the tool runs, keyed by relative path. The
	// scratch directory is the working directory and is removed afterward.
	Files map[string]string
	// Args builds the argument list after the binary, given the scratch directory. It is a function
	// because several engines pass an absolute script path.
	Args func(dir string) []string
	// WantExit is the exit status the tool must produce. This is the assumption itself for the
	// engines whose dry run is an exit-code contract.
	WantExit int
	// MustNotExist is a path, relative to the scratch directory, that must be absent after the run.
	// It is how "the dry run did not execute" is checked rather than assumed: the script writes that
	// file when it actually runs, so its presence is proof the dry run executed the body.
	MustNotExist string
}

// Assumptions returns the facts the named engine's runner relies on its tool providing.
//
// An engine with no entry returns nil, which the guard in the roundhouse tests treats as a gap to
// close rather than as nothing to check.
func Assumptions(engine string) []Assumption {
	switch engine {
	case "ansible":
		return []Assumption{{
			Name: "--check reports what a play would do without doing it",
			Why: "This is the dry run an operator reads before approving a change, and it is the one " +
				"engine where the tool decides per module whether to honor it. A play whose tasks run " +
				"anyway under --check would perform the change the operator was still deciding about.",
			Files: map[string]string{
				"play.yml": ansibleProbe,
				"hosts":    "localhost ansible_connection=local\n",
			},
			Args: func(dir string) []string {
				return []string{"-i", filepath.Join(dir, "hosts"), "--check",
					filepath.Join(dir, "play.yml")}
			},
			WantExit:     0,
			MustNotExist: "ran",
		}, {
			Name: "a play that cannot be parsed fails rather than running what it could read",
			Why: "A dry run that passes an unparseable play tells an operator the change is safe to " +
				"approve when it would fail on the first task.",
			Files: map[string]string{
				"bad.yml": "- hosts: all\n  tasks:\n    - name: unterminated\n      copy:\n        content: \"\n",
				"hosts":   "localhost ansible_connection=local\n",
			},
			Args: func(dir string) []string {
				return []string{"-i", filepath.Join(dir, "hosts"), "--syntax-check",
					filepath.Join(dir, "bad.yml")}
			},
			WantExit: 4,
		}}
	case "bash":
		return []Assumption{{
			Name: "bash -n parses a script without running it",
			Why: "A dry run that executes the body is worse than no dry run, because an operator " +
				"checking a destructive script before approving it would cause the very change they " +
				"were checking.",
			Files:        map[string]string{"s.sh": "touch ran\n"},
			Args:         func(dir string) []string { return []string{"-n", filepath.Join(dir, "s.sh")} },
			WantExit:     0,
			MustNotExist: "ran",
		}, {
			Name:     "bash -n reports a syntax error as a nonzero exit",
			Why:      "A dry run that passes a script the tool cannot parse tells an operator the run is safe to approve when it would fail immediately.",
			Files:    map[string]string{"bad.sh": "if true; then\n"},
			Args:     func(dir string) []string { return []string{"-n", filepath.Join(dir, "bad.sh")} },
			WantExit: 2,
		}}
	case "python":
		return []Assumption{{
			Name: "python -m py_compile compiles a script without running it",
			Why:  "Same as the shell: a dry run must not perform the change it is checking.",
			Files: map[string]string{
				"s.py": "open('ran', 'w').close()\n",
			},
			Args: func(dir string) []string {
				return []string{"-m", "py_compile", filepath.Join(dir, "s.py")}
			},
			WantExit:     0,
			MustNotExist: "ran",
		}}
	case "go":
		return []Assumption{{
			Name: "go vet reports a clean file without running it",
			Why:  "Same as the shell: a dry run must not perform the change it is checking.",
			// The module file is part of the fixture because the toolchain refuses to vet a file
			// outside a module, and a failure for that reason would look like the assumption
			// breaking when it is the fixture that is wrong.
			Files: map[string]string{
				"s.go":   goProbe,
				"go.mod": "module switchtenderenginecontract\n\ngo 1.22\n",
			},
			Args:         func(string) []string { return []string{"vet", "./..."} },
			WantExit:     0,
			MustNotExist: "ran",
		}}
	case "powershell":
		return []Assumption{{
			Name: "pwsh builds a script block from a file without invoking it",
			Why: "This is the only dry run in the product that is neither a parse flag nor a plan " +
				"mode, so nothing else in the tool's own interface protects it. If Create ever " +
				"evaluated instead of parsing, a dry run would execute.",
			Files: map[string]string{"s.ps1": "New-Item -ItemType File -Path ran | Out-Null\n"},
			Args: func(dir string) []string {
				return []string{"-NoProfile", "-NonInteractive", "-Command",
					"[void][scriptblock]::Create((Get-Content -Raw '" + filepath.Join(dir, "s.ps1") + "'))"}
			},
			WantExit:     0,
			MustNotExist: "ran",
		}}
	case "terraform", "opentofu":
		return []Assumption{{
			Name: "plan -detailed-exitcode exits 2 when a change is pending",
			Why: "Drift detection reads exactly this. The runner turns exit 2 into Drift true and " +
				"anything else into no drift, so if this stopped being 2 the product would report " +
				"a drifted estate as clean. That is a silent false negative on a paid feature, and " +
				"no stand-in can tell you it happened.",
			Files:    map[string]string{"main.tf": terraformProbe},
			Args:     func(string) []string { return []string{"plan", "-input=false", "-no-color", "-detailed-exitcode"} },
			WantExit: 2,
		}, {
			Name: "plan -detailed-exitcode exits 0 when nothing is pending",
			Why: "The other half of the same fact. If an unchanged estate also exited 2, every run " +
				"would report drift and the signal would be worthless.",
			Files:    map[string]string{"main.tf": terraformEmpty},
			Args:     func(string) []string { return []string{"plan", "-input=false", "-no-color", "-detailed-exitcode"} },
			WantExit: 0,
		}}
	default:
		return nil
	}
}

// terraformProbe is a module with a pending change and no providers, so init needs no network.
// terraform_data is built into Terraform and OpenTofu rather than supplied by a provider, which is
// what makes this assumption checkable on a machine with no registry access and no cloud account.
const terraformProbe = `
terraform {
  required_version = ">= 1.4.0"
}

resource "terraform_data" "probe" {
  input = "switchtender-contract"
}
`

// terraformEmpty is a module with nothing to do, for the other half of the exit-code fact.
//
// Genuinely nothing: no resources and no outputs. An earlier version of this declared an output, and
// the tool correctly reported exit 2, because recording an output for the first time IS a pending
// change. That was the fixture being wrong rather than the assumption, and it is worth leaving the
// note here: this file is about not confusing the two.
const terraformEmpty = `
terraform {
  required_version = ">= 1.4.0"
}
`

// ansibleProbe is a play whose execution is observable, so a check run proves nothing ran. The task
// is a file creation rather than a command, because Ansible honors check mode per module and the file
// module is one that does.
const ansibleProbe = `
- hosts: all
  gather_facts: false
  tasks:
    - name: create the marker this contract looks for
      ansible.builtin.file:
        path: ran
        state: touch
`

// goProbe is a program whose execution is observable, so vetting it proves nothing ran.
const goProbe = `package main

import "os"

func main() {
	f, err := os.Create("ran")
	if err != nil {
		panic(err)
	}
	_ = f.Close()
}
`

// Contract runs every assumption the named engine declares against binary.
//
// It reports each failure separately and names the fact and its consequence, because a failure here
// is not a bug in this project: it means a third-party tool does not behave the way a runner believes
// it does, and whoever reads the failure has to decide which side changes.
func Contract(t *testing.T, engine, binary string) {
	t.Helper()
	assumptions := Assumptions(engine)
	if len(assumptions) == 0 {
		t.Fatalf("no assumptions are recorded for the %s engine, so running its contract proves "+
			"nothing about the tool its runner invokes", engine)
	}
	for _, a := range assumptions {
		t.Run(a.Name, func(t *testing.T) {
			verify(t, engine, binary, a)
		})
	}
}

// verify runs one assumption in a scratch directory and reports what it found.
func verify(t *testing.T, engine, binary string, a Assumption) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range a.Files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Terraform and OpenTofu will not plan an uninitialized directory, and init with no providers
	// declared reaches no network. A failure here is reported as its own thing rather than as the
	// assumption failing, since the two have different remedies.
	if engine == "terraform" || engine == "opentofu" {
		ctx, cancel := context.WithTimeout(context.Background(), contractTimeout)
		defer cancel()
		init := exec.CommandContext(ctx, binary, "init", "-input=false", "-no-color")
		init.Dir = dir
		if out, err := init.CombinedOutput(); err != nil {
			t.Fatalf("%s init failed before the assumption could be checked: %v\n%s",
				binary, err, out)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), contractTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, a.Args(dir)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := cmd.ProcessState.ExitCode()
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %s, so the assumption could not be checked",
			binary, contractTimeout)
	}
	if code != a.WantExit {
		t.Errorf("%s exited %d, want %d.\n  the fact: %s\n  what breaks when it is false: %s\n  output: %s",
			binary, code, a.WantExit, a.Name, a.Why, out)
	}
	if a.MustNotExist == "" {
		return
	}
	if _, statErr := os.Stat(filepath.Join(dir, a.MustNotExist)); statErr == nil {
		t.Errorf("%s created %s, so this mode executed the script rather than only checking it.\n"+
			"  the fact: %s\n  what breaks when it is false: %s\n  error from the run: %v\n  output: %s",
			binary, a.MustNotExist, a.Name, a.Why, err, out)
	}
}
