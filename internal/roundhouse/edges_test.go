package roundhouse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// TestPlaybookArgsBoundaries pins the ansible-playbook command line at every edge of the Spec that
// shapes it. The separator before the playbook is the load-bearing part: without it a stored template
// whose name begins with a dash turns its own name into an option, so the separator has to survive
// every other combination of flags.
//
//nolint:funlen // Test function.
func TestPlaybookArgsBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Spec is the run whose arguments are built.
		Spec Spec
		// WantArgs is the exact argument list expected.
		WantArgs []string
	}{{ // Test 0: The bare minimum is the separator and the playbook.
		Name: "playbook only", Spec: Spec{Playbook: "site.yml"},
		WantArgs: []string{"--", "site.yml"},
	}, { // Test 1: A playbook named like a flag is still read as a file.
		Name: "playbook begins with a dash", Spec: Spec{Playbook: "--become"},
		WantArgs: []string{"--", "--become"},
	}, { // Test 2: An empty playbook still gets the separator, so nothing shifts position.
		Name: "empty playbook", Spec: Spec{}, WantArgs: []string{"--", ""},
	}, { // Test 3: Verbosity of one is a single -v.
		Name: "one v", Spec: Spec{Playbook: "p.yml", Verbosity: 1},
		WantArgs: []string{"-v", "--", "p.yml"},
	}, { // Test 4: Verbosity at the cap is four, the most ansible-playbook accepts.
		Name: "four v", Spec: Spec{Playbook: "p.yml", Verbosity: 4},
		WantArgs: []string{"-vvvv", "--", "p.yml"},
	}, { // Test 5: Verbosity above the cap is clamped rather than passed through.
		Name: "clamped verbosity", Spec: Spec{Playbook: "p.yml", Verbosity: 1000},
		WantArgs: []string{"-vvvv", "--", "p.yml"},
	}, { // Test 6: A negative verbosity emits nothing rather than a malformed flag.
		Name: "negative verbosity", Spec: Spec{Playbook: "p.yml", Verbosity: -3},
		WantArgs: []string{"--", "p.yml"},
	}, { // Test 7: A negative fork count is omitted, since it would be rejected downstream.
		Name: "negative forks", Spec: Spec{Playbook: "p.yml", Forks: -1},
		WantArgs: []string{"--", "p.yml"},
	}, { // Test 8: One fork is honored, since it is positive.
		Name: "one fork", Spec: Spec{Playbook: "p.yml", Forks: 1},
		WantArgs: []string{"--forks", "1", "--", "p.yml"},
	}, { // Test 9: Empty tag slices emit nothing, so an ordinary run is unchanged.
		Name:     "empty tag slices",
		Spec:     Spec{Playbook: "p.yml", Tags: []string{}, SkipTags: []string{}},
		WantArgs: []string{"--", "p.yml"},
	}, { // Test 10: A single tag is one comma-joined value, not a repeated flag.
		Name: "one tag", Spec: Spec{Playbook: "p.yml", Tags: []string{"deploy"}},
		WantArgs: []string{"--tags", "deploy", "--", "p.yml"},
	}, { // Test 11: An empty tag string is passed as written rather than dropped silently.
		Name: "empty tag string", Spec: Spec{Playbook: "p.yml", Tags: []string{""}},
		WantArgs: []string{"--tags", "", "--", "p.yml"},
	}, { // Test 12: Several extra vars files each get their own reference, in order.
		Name: "several vars files",
		Spec: Spec{Playbook: "p.yml", ExtraVarsFiles: []string{"/t/a.json", "/t/b.json"}},
		WantArgs: []string{"--extra-vars", "@/t/a.json", "--extra-vars", "@/t/b.json",
			"--", "p.yml"},
	}, { // Test 13: Every option appears in a fixed order ahead of the separator.
		Name: "everything at once",
		Spec: Spec{
			Playbook: "p.yml", Inventory: "h.ini", Limit: "web", Tags: []string{"a", "b"},
			SkipTags: []string{"c"}, Forks: 4, DiffMode: true, Verbosity: 2, DryRun: true,
			ExtraVarsFiles: []string{"/t/v.json"}, PrivateKeyPath: "/t/key",
			VaultPasswords: []VaultPassword{{Path: "/t/vp"}},
		},
		WantArgs: []string{
			"-i", "h.ini", "--limit", "web", "--tags", "a,b", "--skip-tags", "c",
			"--forks", "4", "--diff", "-vv", "--check", "--extra-vars", "@/t/v.json",
			"--private-key", "/t/key", "--vault-password-file", "/t/vp", "--", "p.yml",
		},
	}, { // Test 14: A unicode playbook name survives intact.
		Name: "unicode playbook", Spec: Spec{Playbook: "spillbøker/nettverk.yml"},
		WantArgs: []string{"--", "spillbøker/nettverk.yml"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := playbookArgs(test.Spec)
			if err != nil {
				t.Fatalf("%s: playbookArgs() error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.WantArgs, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: args mismatch (-want +got):\n%s", test.Name, diff)
			}
			// Wherever the separator lands, nothing may follow it but the playbook itself.
			sep := -1
			for i, a := range got {
				if a == "--" {
					sep = i
					break
				}
			}
			if sep == -1 || sep != len(got)-2 {
				t.Errorf("%s: the playbook is not the only argument after the separator: %v",
					test.Name, got)
			}
		})
	}
}

// TestCallbackEnv pins the three variables the structured event stream depends on. Ansible reads the
// plugin directory and the enabled callback name from the environment, and the plugin reads where to
// write from it, so a renamed variable costs a run its events with no error anywhere.
func TestCallbackEnv(t *testing.T) {
	t.Parallel()
	want := []string{
		"ANSIBLE_CALLBACK_PLUGINS=/t/plugins",
		"ANSIBLE_CALLBACKS_ENABLED=switchtender",
		"SWITCHTENDER_EVENTS_PATH=/t/events.ndjson",
	}
	if diff := cmp.Diff(want, callbackEnv("/t/plugins", "/t/events.ndjson")); diff != "" {
		t.Errorf("callback env mismatch (-want +got):\n%s", diff)
	}
	// The enabled callback name has to match the plugin file that is materialized beside it.
	if !strings.Contains(callbackPlugin, pluginName) {
		t.Errorf("the embedded plugin does not carry the name %q it is enabled under", pluginName)
	}
}

// TestAnsibleRunEnablesTheCallbackForTheChild proves the events wiring end to end for a host run:
// the plugin is materialized, its directory is handed to the child, and the sidecar path travels
// with it. Checking the helper alone would not catch a Run that built the environment and never
// attached it.
func TestAnsibleRunEnablesTheCallbackForTheChild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "ansible-playbook")
	writeStub(t, stub, "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\nenv\n")
	events := filepath.Join(dir, "events.ndjson")

	var buf strings.Builder
	res, err := newAnsibleRunner(WithBinary(stub)).Run(t.Context(),
		Spec{Playbook: "p.yml", EventsPath: events}, &buf)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run() exit=%d err=%v", res.ExitCode, err)
	}

	got := buf.String()
	for _, want := range []string{
		"ANSIBLE_CALLBACKS_ENABLED=" + pluginName, "SWITCHTENDER_EVENTS_PATH=" + events,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the child environment is missing %q:\n%s", want, got)
		}
	}
	pluginDir := ""
	for _, line := range strings.Split(got, "\n") {
		if v, ok := strings.CutPrefix(line, "ANSIBLE_CALLBACK_PLUGINS="); ok {
			pluginDir = v
		}
	}
	if pluginDir == "" {
		t.Fatalf("no callback plugin directory reached the child:\n%s", got)
	}
	t.Cleanup(func() { _ = os.RemoveAll(pluginDir) })
	body, err := os.ReadFile(filepath.Join(pluginDir, pluginName+".py"))
	if err != nil {
		t.Fatalf("the directory handed to Ansible holds no plugin: %v", err)
	}
	if string(body) != callbackPlugin {
		t.Error("the materialized plugin is not the embedded copy")
	}
}

// TestAnsibleRunWithoutEventsEnablesNoCallback pins the other half: a run that asked for no events
// must not enable the callback at all, so an ordinary run does not import a plugin it has no sidecar
// for and then fail on a path that was never set.
func TestAnsibleRunWithoutEventsEnablesNoCallback(t *testing.T) {
	t.Parallel()
	stub := filepath.Join(t.TempDir(), "ansible-playbook")
	writeStub(t, stub, "#!/bin/sh\n[ \"$1\" = stub-warmup ] && exit 0\nenv\n")

	var buf strings.Builder
	if _, err := newAnsibleRunner(WithBinary(stub)).Run(t.Context(),
		Spec{Playbook: "p.yml"}, &buf); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, absent := range []string{"ANSIBLE_CALLBACK_PLUGINS", "SWITCHTENDER_EVENTS_PATH"} {
		if strings.Contains(buf.String(), absent) {
			t.Errorf("a run with no events sidecar still set %q", absent)
		}
	}
}

// TestMaterializeExtraVarsBoundaries pins the movement of a run's own variables off the command
// line. A survey has no password field type, so a secret is collected as ordinary text, and argv is
// readable through ps by any local account for the life of the run.
func TestMaterializeExtraVarsBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Spec is the run whose variables are materialized.
		Spec Spec
		// WantFiles is how many extra vars files the spec must carry afterward.
		WantFiles int
		// WantJSON is the content of the file this call wrote, empty when it wrote none.
		WantJSON string
	}{{ // Test 0: No variables writes nothing and leaves the spec alone.
		Name: "nil vars", Spec: Spec{Playbook: "p.yml"}, WantFiles: 0,
	}, { // Test 1: An empty map is the same as none, so no file appears on disk.
		Name: "empty map", Spec: Spec{Playbook: "p.yml", ExtraVars: map[string]any{}}, WantFiles: 0,
	}, { // Test 2: A single variable is written as JSON so its type survives.
		Name: "one var", Spec: Spec{Playbook: "p.yml", ExtraVars: map[string]any{"n": 3}},
		WantFiles: 1, WantJSON: `{"n":3}`,
	}, { // Test 3: A file already on the spec is kept and the new one appended after it.
		Name: "appends to existing files",
		Spec: Spec{Playbook: "p.yml", ExtraVarsFiles: []string{"/t/first.json"},
			ExtraVars: map[string]any{"k": "v"}},
		WantFiles: 2, WantJSON: `{"k":"v"}`,
	}, { // Test 4: A non-ASCII value survives the round trip through the file.
		Name:      "unicode value",
		Spec:      Spec{Playbook: "p.yml", ExtraVars: map[string]any{"navn": "Øystein"}},
		WantFiles: 1, WantJSON: `{"navn":"Øystein"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spec := test.Spec
			cleanup, err := materializeExtraVars(&spec)
			defer cleanup()
			if err != nil {
				t.Fatalf("%s: materializeExtraVars() error = %v", test.Name, err)
			}
			if len(spec.ExtraVarsFiles) != test.WantFiles {
				t.Fatalf("%s: files = %v, want %d", test.Name, spec.ExtraVarsFiles, test.WantFiles)
			}
			if test.WantJSON == "" {
				return
			}
			// The inline copy is cleared, so the values cannot also travel on the command line.
			if spec.ExtraVars != nil {
				t.Errorf("%s: the inline vars survived: %v", test.Name, spec.ExtraVars)
			}
			path := spec.ExtraVarsFiles[len(spec.ExtraVarsFiles)-1]
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("%s: ReadFile() error = %v", test.Name, readErr)
			}
			if diff := cmp.Diff(test.WantJSON, string(body)); diff != "" {
				t.Errorf("%s: vars file mismatch (-want +got):\n%s", test.Name, diff)
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("%s: Stat() error = %v", test.Name, statErr)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s: vars file mode = %o, want 600", test.Name, perm)
			}
			cleanup()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s: the vars file outlived the run: %v", test.Name, err)
			}
			// Cleanup runs again through the defer, and must not report anything new.
		})
	}
}

// TestWriteScriptFileRoundTrip pins that a run's inline source reaches disk byte for byte and is
// removed afterward. The Python, Go, and PowerShell runners all execute what this writes, so a
// truncated or re-encoded script is a run executing something other than what was approved.
func TestWriteScriptFileRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Content is the source written.
		Content string
	}{
		{"empty source", ""},                                              // Test 0.
		{"single byte", "x"},                                              // Test 1.
		{"trailing newline", "print(1)\n"},                                // Test 2.
		{"embedded nulls are not a terminator", "a\x00b"},                 // Test 3.
		{"unicode", "print('Ømrådet er større')\n"},                       // Test 4.
		{"very long source", strings.Repeat("# padding line\n", 100_000)}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path, cleanup, err := writeScriptFile("switchtender-edge-*.txt", test.Content)
			defer cleanup()
			if err != nil {
				t.Fatalf("%s: writeScriptFile() error = %v", test.Name, err)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: ReadFile() error = %v", test.Name, err)
			}
			if diff := cmp.Diff(test.Content, string(body)); diff != "" {
				t.Errorf("%s: content mismatch (-want +got):\n%s", test.Name, diff)
			}
			cleanup()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s: the script outlived the run: %v", test.Name, err)
			}
		})
	}
}

// TestToolArgsForEveryScriptEngine pins the argument list each engine gets, and above all that a dry
// run cannot execute anything. A dry run is what an approver reads before an apply, so an engine
// whose no-change mode still ran the source would turn a preview into a change.
func TestToolArgsForEveryScriptEngine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which engine and mode is being pinned.
		Name string
		// Got is the argument list the engine builds.
		Got []string
		// WantArgs is the exact list expected.
		WantArgs []string
	}{
		{"bash run", bashArgs(Spec{Command: "echo hi"}), []string{"-c", "echo hi"}}, // Test 0.
		{"bash dry run", bashArgs(Spec{Command: "echo hi", DryRun: true}),
			[]string{"-n", "-c", "echo hi"}}, // Test 1.
		{"bash empty command", bashArgs(Spec{}), []string{"-c", ""}},      // Test 2.
		{"python run", pythonArgs("/t/s.py", false), []string{"/t/s.py"}}, // Test 3.
		{"python dry run", pythonArgs("/t/s.py", true),
			[]string{"-m", "py_compile", "/t/s.py"}}, // Test 4.
		{"go run", goArgs("/t/m.go", false), []string{"run", "/t/m.go"}},    // Test 5.
		{"go dry run", goArgs("/t/m.go", true), []string{"vet", "/t/m.go"}}, // Test 6.
		{"pwsh run", pwshArgs("/t/s.ps1", false),
			[]string{"-NoProfile", "-NonInteractive", "-File", "/t/s.ps1"}}, // Test 7.
		{"pwsh dry run", pwshArgs("/t/s.ps1", true),
			[]string{"-NoProfile", "-NonInteractive", "-Command",
				`[void][scriptblock]::Create((Get-Content -Raw '/t/s.ps1'))`}}, // Test 8.
		{"terraform init", terraformInitArgs(),
			[]string{"init", "-input=false", "-no-color"}}, // Test 9.
		{"terraform apply", terraformActionArgs(false),
			[]string{"apply", "-auto-approve", "-input=false", "-no-color"}}, // Test 10.
		{"terraform plan", terraformActionArgs(true),
			[]string{"plan", "-input=false", "-no-color", "-detailed-exitcode"}}, // Test 11.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantArgs, test.Got); diff != "" {
				t.Errorf("%s: args mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
	// Neither Terraform mode ever prompts, since an executor has no terminal to answer with.
	for _, args := range [][]string{terraformInitArgs(), terraformActionArgs(true),
		terraformActionArgs(false)} {
		if !strings.Contains(strings.Join(args, " "), "-input=false") {
			t.Errorf("args %v may prompt for input on an executor with no terminal", args)
		}
	}
}

// TestSourceEnvArgv pins the shell wrapper that carries a run's environment into the container. The
// positional convention is what keeps a tool argument from being read as the env path, and exec is
// what leaves the tool as the container's own process rather than a child of a shell that would
// swallow its signals.
func TestSourceEnvArgv(t *testing.T) {
	t.Parallel()
	got := sourceEnvArgv("/t/env", []string{"ansible-playbook", "--", "-weird.yml"})
	want := []string{"sh", "-c", `. "$1"; shift; exec "$@"`, "sh", "/t/env",
		"ansible-playbook", "--", "-weird.yml"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("wrapper mismatch (-want +got):\n%s", diff)
	}
	// An empty argv still produces a valid wrapper rather than a shell reading its own arguments.
	if diff := cmp.Diff([]string{"sh", "-c", `. "$1"; shift; exec "$@"`, "sh", "/t/env"},
		sourceEnvArgv("/t/env", nil)); diff != "" {
		t.Errorf("empty argv wrapper mismatch (-want +got):\n%s", diff)
	}
}

// TestVarsExtraAndVarsEnvBoundaries pins how a run's survey answers reach a script engine. They
// travel as one JSON object in the environment rather than on the command line, and the layering
// order decides which value wins when the same name arrives twice.
func TestVarsExtraAndVarsEnvBoundaries(t *testing.T) {
	t.Parallel()
	if got := varsExtra(Spec{}); got != nil {
		t.Errorf("varsExtra with no vars = %v, want nil", got)
	}
	if got := varsExtra(Spec{ExtraVars: map[string]any{}}); got != nil {
		t.Errorf("varsExtra with an empty map = %v, want nil", got)
	}
	want := []string{`SWITCHTENDER_VARS={"a":1,"b":"two"}`}
	got := varsExtra(Spec{ExtraVars: map[string]any{"a": 1, "b": "two"}})
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("varsExtra mismatch (-want +got):\n%s", diff)
	}

	// The Spec's own environment is layered over the base, and the vars entry comes last, so a base
	// environment cannot shadow the answers a run was submitted with.
	env := varsEnv([]string{"BASE=1", "SWITCHTENDER_VARS=stale"},
		Spec{Env: []string{"TOKEN=x"}, ExtraVars: map[string]any{"k": "v"}})
	wantEnv := []string{"BASE=1", "SWITCHTENDER_VARS=stale", "TOKEN=x",
		`SWITCHTENDER_VARS={"k":"v"}`}
	if diff := cmp.Diff(wantEnv, env); diff != "" {
		t.Errorf("varsEnv mismatch (-want +got):\n%s", diff)
	}

	// A base environment is never modified in place, so one run cannot alter what the next inherits.
	base := []string{"BASE=1"}
	_ = varsEnv(base, Spec{Env: []string{"A=1"}})
	_ = varsEnv(base, Spec{Env: []string{"B=2"}})
	if diff := cmp.Diff([]string{"BASE=1"}, base); diff != "" {
		t.Errorf("the base environment was mutated (-want +got):\n%s", diff)
	}
}

// TestToolClassifiers pins the two predicates the router and the drift reading turn on. A tool that
// stopped counting as built-in would silently run on the host with its image ignored, and one that
// wrongly counted as Terraform would have its exit code 2 read as drift instead of as a failure.
func TestToolClassifiers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name is the tool string under test.
		Name string
		// WantBuiltin is whether the container runner can execute it.
		WantBuiltin bool
		// WantTerraform is whether its dry run reports drift by exit code 2.
		WantTerraform bool
	}{
		{"", true, false},                 // Test 0: Empty means Ansible.
		{run.ToolAnsible, true, false},    // Test 1.
		{run.ToolBash, true, false},       // Test 2.
		{run.ToolTerraform, true, true},   // Test 3.
		{run.ToolOpenTofu, true, true},    // Test 4.
		{run.ToolPython, true, false},     // Test 5.
		{run.ToolPowerShell, true, false}, // Test 6.
		{run.ToolGo, true, false},         // Test 7.
		{"cobol", false, false},           // Test 8: An unregistered name.
		{"Terraform", false, false},       // Test 9: Tool names are matched exactly.
		{"terraform ", false, false},      // Test 10: Trailing space is a different name.
		{"opentofu\n", false, false},      // Test 11: A newline is a different name.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := isBuiltinTool(test.Name); got != test.WantBuiltin {
				t.Errorf("isBuiltinTool(%q) = %v, want %v", test.Name, got, test.WantBuiltin)
			}
			if got := isTerraformTool(test.Name); got != test.WantTerraform {
				t.Errorf("isTerraformTool(%q) = %v, want %v", test.Name, got, test.WantTerraform)
			}
		})
	}
}

// TestTerraformVarsBoundaries pins how survey answers become TF_VAR_ entries. A string passes
// through unquoted because Terraform reads it as the value itself, and everything else is JSON,
// which is how a list or map variable arrives. A value that cannot be encoded is dropped rather than
// corrupting the rest of the environment.
func TestTerraformVarsBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Vars are the extra vars offered.
		Vars map[string]any
		// WantEntries is the exact sorted environment expected.
		WantEntries []string
	}{
		{"nil map", nil, nil},                // Test 0.
		{"empty map", map[string]any{}, nil}, // Test 1.
		{"a string is not quoted", map[string]any{"r": "us-east-1"},
			[]string{"TF_VAR_r=us-east-1"}}, // Test 2.
		{"an empty string is kept", map[string]any{"r": ""}, []string{"TF_VAR_r="}}, // Test 3.
		{"a bool is JSON", map[string]any{"on": true}, []string{"TF_VAR_on=true"}},  // Test 4.
		{"a nil is JSON null", map[string]any{"x": nil}, []string{"TF_VAR_x=null"}}, // Test 5.
		{"a map is JSON", map[string]any{"t": map[string]any{"env": "prod"}},
			[]string{`TF_VAR_t={"env":"prod"}`}}, // Test 6.
		{"entries are sorted", map[string]any{"b": "2", "a": "1"},
			[]string{"TF_VAR_a=1", "TF_VAR_b=2"}}, // Test 7.
		{"an unencodable value is dropped, the rest survive",
			map[string]any{"good": "1", "bad": make(chan int)},
			[]string{"TF_VAR_good=1"}}, // Test 8.
		{"a unicode value survives", map[string]any{"n": "Ålesund"},
			[]string{"TF_VAR_n=Ålesund"}}, // Test 9.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := terraformVars(test.Vars)
			if diff := cmp.Diff(test.WantEntries, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: entries mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestHostileTerraformVariableNamesNeverReachAContainerShell pins the join between two layers. A
// survey key becomes the tail of a TF_VAR_ name, and inside a container that environment is written
// as shell assignments and sourced, where a name is not quotable. A key carrying a command separator
// therefore has to be refused at the export, which is what keeps a survey answer from running a
// command in the container.
func TestHostileTerraformVariableNamesNeverReachAContainerShell(t *testing.T) {
	t.Parallel()
	entries := terraformVars(map[string]any{
		"good":            "kept",
		";touch /tmp/pwn": "1",
		"$(id)":           "1",
		"a b":             "1",
	})
	var emitted []string
	for _, kv := range entries {
		if line := shellExport(kv); line != "" {
			emitted = append(emitted, line)
		}
	}
	if diff := cmp.Diff([]string{"export TF_VAR_good='kept'"}, emitted); diff != "" {
		t.Errorf("a hostile variable name reached the sourced env file (-want +got):\n%s", diff)
	}
}

// TestToolWorkDirBoundaries pins the containment for a Terraform working directory at the edges the
// symlink test does not cover. An absolute subdirectory is neutralized by the join rather than
// escaping, a base that does not exist has no links to follow, and a path through a regular file is
// an error rather than an allowance.
func TestToolWorkDirBoundaries(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	file := filepath.Join(base, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Base is the project checkout.
		Base string
		// Sub is the subdirectory named by the run.
		Sub string
		// WantDir is the resolved directory, when the case is allowed.
		WantDir string
		// Want is the refusal expected, or nil when the case is allowed.
		Want error
	}{
		{"empty sub is the checkout itself", base, "", base, nil}, // Test 0.
		{"dot is the checkout itself", base, ".", base, nil},      // Test 1.
		{"an absolute sub is joined, not obeyed", base, "/etc",
			filepath.Join(base, "etc"), nil}, // Test 2.
		{"a deep escape is refused", base, "a/b/../../../..", "", ErrBadWorkDir}, // Test 3.
		{"a path through a regular file is refused", base, "notadir/x", "",
			ErrBadWorkDir}, // Test 4.
		{"no checkout means the path as given", "", "/srv/infra", "/srv/infra", nil}, // Test 5.
		{"no checkout keeps a relative path", "", "infra", "infra", nil},             // Test 6.
		{"a base that does not exist has no links to follow",
			filepath.Join(base, "absent"), "infra",
			filepath.Join(base, "absent", "infra"), nil}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := toolWorkDir(test.Base, test.Sub)
			if test.Want != nil {
				if !errors.Is(err, test.Want) {
					t.Errorf("%s: toolWorkDir(%q, %q) = (%q, %v), want %v",
						test.Name, test.Base, test.Sub, got, err, test.Want)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: toolWorkDir(%q, %q) error = %v", test.Name, test.Base, test.Sub, err)
			}
			if diff := cmp.Diff(test.WantDir, got); diff != "" {
				t.Errorf("%s: directory mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestValidateImageBoundaries pins the reference filter at its edges. The first character rule is
// what stops a reference being read by the container CLI as a flag, the length cap bounds what is
// handed to it, and the dot-dot rule refuses a traversal spelling outright.
func TestValidateImageBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Image is the reference offered.
		Image string
		// Want is the refusal expected, or nil when the reference is accepted.
		Want error
	}{
		{"empty", "", ErrNoImage},                                   // Test 0.
		{"single character", "a", nil},                              // Test 1.
		{"digit start", "0img", nil},                                // Test 2.
		{"uppercase start", "Img", nil},                             // Test 3.
		{"dot start reads as a path", ".img", ErrBadImage},          // Test 4.
		{"underscore start", "_img", ErrBadImage},                   // Test 5.
		{"slash start is an absolute path", "/img", ErrBadImage},    // Test 6.
		{"traversal anywhere", "reg.io/a/../../etc", ErrBadImage},   // Test 7.
		{"a lone dot pair", "a..b", ErrBadImage},                    // Test 8.
		{"newline in a reference", "alpine\nrm -rf /", ErrBadImage}, // Test 9.
		{"tab in a reference", "alpine\t3", ErrBadImage},            // Test 10.
		{"unicode is refused", "älpine", ErrBadImage},               // Test 11.
		{"a null byte is refused", "alpine\x00", ErrBadImage},       // Test 12.
		{"at the length cap", "a" + strings.Repeat("b", 511), nil},  // Test 13.
		{"one past the length cap", "a" + strings.Repeat("b", 512),
			ErrBadImage}, // Test 14.
		{"a full digest reference",
			"ghcr.io/org/img@sha256:" + strings.Repeat("a", 64), nil}, // Test 15.
		{"a port and a tag", "localhost:5000/team/img:v1.2.3-rc1", nil}, // Test 16.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := validateImage(test.Image)
			if test.Want == nil {
				if err != nil {
					t.Errorf("%s: validateImage(%q) = %v, want it accepted", test.Name, test.Image, err)
				}
				return
			}
			if !errors.Is(err, test.Want) {
				t.Errorf("%s: validateImage(%q) = %v, want %v", test.Name, test.Image, err, test.Want)
			}
		})
	}
}

// TestContainerNameIsUniquePerRun pins that each run gets its own container name. The name is how a
// canceled run finds and removes its container, so a shared name would let one run's cancel stop
// another run's container.
func TestContainerNameIsUniquePerRun(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		name := containerName()
		if !strings.HasPrefix(name, "ym-") {
			t.Fatalf("containerName() = %q, want the ym- prefix a cancel looks for", name)
		}
		if seen[name] {
			t.Fatalf("containerName() repeated %q, so one run's cancel can remove another's "+
				"container", name)
		}
		seen[name] = true
	}
}

// TestWriteEnvFileBoundaries pins the file a container sources its environment from. It is the only
// place a resolved credential is written in the clear, so it must be private, must hold nothing when
// there is nothing to inject, and must be removed with the run.
//
//nolint:funlen // Test function.
func TestWriteEnvFileBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Spec is the run whose environment is written.
		Spec Spec
		// ExtraEnv is the tool-specific environment the plan added.
		ExtraEnv []string
		// WantLines is the exact file content expected, empty when no file is written.
		WantLines []string
	}{{ // Test 0: Nothing to inject writes no file, so an image without a shell still runs.
		Name: "no environment", Spec: Spec{}, WantLines: nil,
	}, { // Test 1: Entries that are not assignments write no file rather than a broken one.
		Name: "only unusable entries", Spec: Spec{Env: []string{"novalue", "=empty", "9BAD=1"}},
		WantLines: nil,
	}, { // Test 2: A credential is written as a quoted export.
		Name: "one credential", Spec: Spec{Env: []string{"TOKEN=abc"}},
		WantLines: []string{"export TOKEN='abc'"},
	}, { // Test 3: The tool's own environment follows the Spec's, so it can override it.
		Name: "extra env follows", Spec: Spec{Env: []string{"A=1"}},
		ExtraEnv:  []string{`SWITCHTENDER_VARS={"k":"v"}`},
		WantLines: []string{"export A='1'", `export SWITCHTENDER_VARS='{"k":"v"}'`},
	}, { // Test 4: An unsafe name is dropped while its honest neighbors survive.
		Name:      "hostile name dropped",
		Spec:      Spec{Env: []string{"GOOD=1", "EVIL;id=1", "ALSOGOOD=2"}},
		WantLines: []string{"export GOOD='1'", "export ALSOGOOD='2'"},
	}, { // Test 5: A value carrying a quote is escaped so it survives being sourced.
		Name: "quote in a value", Spec: Spec{Env: []string{`P=O'Brien`}},
		WantLines: []string{`export P='O'\''Brien'`},
	}, { // Test 6: A value spanning lines is written literally inside the quotes.
		Name: "multiline value", Spec: Spec{Env: []string{"KEY=line1\nline2"}},
		WantLines: []string{"export KEY='line1\nline2'"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			c := newContainerRunner("docker", "missing", false, nil, &pluginCache{},
				DefaultContainerLimits())
			path, cleanup, err := c.writeEnvFile(test.Spec, test.ExtraEnv)
			defer cleanup()
			if err != nil {
				t.Fatalf("%s: writeEnvFile() error = %v", test.Name, err)
			}
			if len(test.WantLines) == 0 {
				if path != "" {
					t.Errorf("%s: a run with nothing to inject wrote %q", test.Name, path)
				}
				return
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: ReadFile() error = %v", test.Name, err)
			}
			want := strings.Join(test.WantLines, "\n") + "\n"
			if diff := cmp.Diff(want, string(body)); diff != "" {
				t.Errorf("%s: env file mismatch (-want +got):\n%s", test.Name, diff)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("%s: Stat() error = %v", test.Name, err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s: env file mode = %o, want 600 for a file holding credentials",
					test.Name, perm)
			}
			cleanup()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s: the environment file outlived the run: %v", test.Name, err)
			}
		})
	}
}

// TestWriteEnvFileCarriesTheCallbackVariables pins that an Ansible run asking for events gets the
// callback wiring inside the container too, materializing the plugin so the mount that follows has
// something to share.
func TestWriteEnvFileCarriesTheCallbackVariables(t *testing.T) {
	t.Parallel()
	cache := &pluginCache{}
	c := newContainerRunner("docker", "missing", false, nil, cache, DefaultContainerLimits())
	events := filepath.Join(t.TempDir(), "events.ndjson")

	path, cleanup, err := c.writeEnvFile(Spec{EventsPath: events}, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("writeEnvFile() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache.dir) })
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{
		"export ANSIBLE_CALLBACKS_ENABLED='" + pluginName + "'",
		"export SWITCHTENDER_EVENTS_PATH='" + events + "'",
		"export ANSIBLE_CALLBACK_PLUGINS='" + cache.dir + "'",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("env file %q missing %q", body, want)
		}
	}
}

// TestValidEnvNameBoundaries pins the one check that cannot be replaced by quoting. A value is
// single quoted so nothing in it escapes, but a name sits unquoted on the left of an assignment,
// and a custom credential type lets an operator choose that name, so anything outside a POSIX shell
// name has to be refused.
func TestValidEnvNameBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name is the candidate variable name.
		Name string
		// WantValid is whether it is a POSIX shell variable name.
		WantValid bool
	}{
		{"A", true},       // Test 0.
		{"_", true},       // Test 1.
		{"_0", true},      // Test 2.
		{"A_B_9", true},   // Test 3.
		{"", false},       // Test 4: An empty name is not an assignment.
		{"0A", false},     // Test 5: A leading digit is not a shell name.
		{"A-B", false},    // Test 6: A hyphen is not a name character.
		{"A.B", false},    // Test 7.
		{"Æ", false},      // Test 8: Non-ASCII letters are refused, not folded.
		{"A B", false},    // Test 9.
		{"A\x00B", false}, // Test 10: A null byte cannot pass as a name.
		{"A$", false},     // Test 11.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := validEnvName(test.Name); got != test.WantValid {
				t.Errorf("validEnvName(%q) = %v, want %v", test.Name, got, test.WantValid)
			}
		})
	}
}

// TestRegistryHostBoundaries pins which part of a reference is treated as a registry to log in to.
// Getting it wrong sends a credential to the wrong host: a namespace read as a registry would hand
// the password to a registry the operator never named.
func TestRegistryHostBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Image is the reference under test.
		Image string
		// WantHost is the registry the login must target, empty for Docker Hub.
		WantHost string
	}{
		{"", ""},                          // Test 0: No reference at all.
		{"alpine", ""},                    // Test 1: A bare image is Docker Hub.
		{"org/team/img", ""},              // Test 2: A namespace is not a registry.
		{"reg.io/img", "reg.io"},          // Test 3: A dot makes it a host.
		{"localhost/img", "localhost"},    // Test 4: The local name is a host.
		{"localhosts/img", ""},            // Test 5: A similar name is not.
		{"host:5000/img", "host:5000"},    // Test 6: A port makes it a host.
		{"/img", ""},                      // Test 7: An empty first segment is not a host.
		{"reg.io", ""},                    // Test 8: With no slash there is no registry part.
		{"reg.io/a@sha256:abc", "reg.io"}, // Test 9: A digest does not change the host.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantHost, registryHost(test.Image)); diff != "" {
				t.Errorf("registryHost(%q) mismatch (-want +got):\n%s", test.Image, diff)
			}
		})
	}
}
