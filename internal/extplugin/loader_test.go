package extplugin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// buildModePlugin compiles the mode-driven test extension into a fresh directory and returns that
// directory, so one Load sees exactly one plugin.
func buildModePlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "modeplugin"), "./testdata/modeplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mode plugin: %v\n%s", err, out)
	}
	return dir
}

// processAlive reports whether a process id still names a running process. Signal zero performs the
// permission and existence checks without delivering anything.
func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// TestLoadRefusesUnregistrableDescribe drives the loader against real plugin binaries whose Describe
// response the host must refuse. This is the path that decides whether a dropped-in binary can claim
// a name the server already answers to: a plugin that took "bash" would receive every bash run on the
// fleet. Each refusal must skip the plugin whole, leave the rest of startup running, and end the
// process, since a refused plugin that keeps running is an unsupervised binary holding the host's
// socket.
//
// It does not call t.Parallel: it sets the environment the plugin subprocess reads its mode from.
func TestLoadRefusesUnregistrableDescribe(t *testing.T) {
	tests := []struct {
		Name     string
		Mode     string
		WantText string
	}{{ // Test 0: A tool name that is a built-in is refused before anything is registered.
		Name: "tool collides with a built-in", Mode: "collide-tool",
		WantText: `"bash" is already registered`,
	}, { // Test 1: A name of only spaces is refused, though the SDK's own check allows it.
		Name: "blank tool name", Mode: "blank-tool", WantText: "an empty name",
	}, { // Test 2: An AI provider that collides after the registry's lowercasing is refused.
		Name: "ai provider collides on case", Mode: "collide-ai",
		WantText: `"OpenAI" is already registered`,
	}, { // Test 3: A secret kind declared as both a resolver and a minter is refused.
		Name: "secret kind declared twice", Mode: "dup-secret",
		WantText: `"modeplugin-kind" is declared twice`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			dir := buildModePlugin(t)
			pidFile := filepath.Join(t.TempDir(), "pid")
			t.Setenv("SWITCHTENDER_PLUGIN_MODE", test.Mode)
			t.Setenv("SWITCHTENDER_PLUGIN_PIDFILE", pidFile)

			core, logs := observer.New(zapcore.WarnLevel)
			closePlugins, err := Load(dir, zap.New(core))
			if err != nil {
				t.Fatalf("test %d (%s): Load error: %v, want a skipped plugin and a clean start",
					testNum, test.Name, err)
			}
			defer closePlugins()

			// The plugin was skipped whole, so the notifier it declared beside the bad name is not
			// registered. A loader that registered as it walked the response would have wired this one.
			channel := "modeplugin-" + test.Mode
			if dispatch.NotifierRegistered(channel) {
				t.Errorf("test %d (%s): notifier %q was registered from a refused plugin",
					testNum, test.Name, channel)
			}
			entries := logs.All()
			if len(entries) == 0 {
				t.Fatalf("test %d (%s): the refusal was not logged, so an operator sees nothing",
					testNum, test.Name)
			}
			msg := entries[0].Message
			if !strings.Contains(msg, "skipping plugin") || !strings.Contains(msg, test.WantText) {
				t.Errorf("test %d (%s): log = %q, want a skip naming %q",
					testNum, test.Name, msg, test.WantText)
			}
			pid := readPID(t, pidFile)
			if runtime.GOOS != "windows" && pid == 0 {
				t.Fatalf("test %d (%s): the plugin never recorded its process id, so the check below "+
					"would pass without proving anything", testNum, test.Name)
			}
			if pid != 0 && processAlive(t, pid) {
				t.Errorf("test %d (%s): the refused plugin process %d is still running",
					testNum, test.Name, pid)
			}
		})
	}
}

// readPID waits briefly for the plugin's pid file and returns the id it holds, or zero when the file
// never appeared or the platform cannot check a process this way.
func readPID(t *testing.T, path string) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		return 0
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("pid file %s holds %q: %v", path, data, convErr)
			}
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLoadSkipsABinaryThatCannotDescribeItself pins the loader against a binary that answers the
// handshake and then serves none of the protocol. The handshake cookie is a guard against launching
// the wrong binary, not proof of anything, so the first real question is what the binary provides. A
// process that cannot answer it must be skipped and killed, with the server's remaining startup
// unaffected.
func TestLoadSkipsABinaryThatCannotDescribeItself(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "bareplugin"), "./testdata/bareplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build bare plugin: %v\n%s", err, out)
	}
	core, logs := observer.New(zapcore.WarnLevel)
	closePlugins, err := Load(dir, zap.New(core))
	if err != nil {
		t.Fatalf("Load error: %v, want a skipped plugin and a clean start", err)
	}
	defer closePlugins()
	if logs.Len() == 0 {
		t.Fatal("the skip was not logged, so an operator sees a plugin that simply never works")
	}
	if msg := logs.All()[0].Message; !strings.Contains(msg, "skipping plugin") ||
		!strings.Contains(msg, "describe") {
		t.Errorf("log = %q, want a skip naming the describe call", msg)
	}
}

// TestLoadSkipsEntriesThatAreNotPluginBinaries pins what the loader walks past without launching. The
// plugins directory is a directory an operator drops files into, so it holds notes, subdirectories,
// and half-copied files, and each must be skipped rather than launched or treated as a startup
// failure.
func TestLoadSkipsEntriesThatAreNotPluginBinaries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A directory carrying execute bits is still a directory and must not be launched.
	if err := os.Mkdir(filepath.Join(dir, "executable-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zapcore.WarnLevel)
	closePlugins, err := Load(dir, zap.New(core))
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	closePlugins()
	if logs.Len() != 0 {
		t.Errorf("skipping non-plugin entries logged %d warnings, want none: %v",
			logs.Len(), logs.All())
	}
}

// TestLoadClosesEveryPlugin pins that the close func the loader returns is safe to call, including
// when nothing loaded and when it is called more than once. It runs after the dispatcher drains, on
// a shutdown path where a panic would take down an otherwise clean stop.
func TestLoadClosesEveryPlugin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Dir  func(*testing.T) string
	}{ // Test 0: An unconfigured plugins directory closes cleanly.
		{Name: "no directory", Dir: func(*testing.T) string { return "" }},
		// Test 1: An empty plugins directory closes cleanly.
		{Name: "empty directory", Dir: func(t *testing.T) string { t.Helper(); return t.TempDir() }},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			closePlugins, err := Load(test.Dir(t), zap.NewNop())
			if err != nil {
				t.Fatalf("test %d (%s): Load error: %v", testNum, test.Name, err)
			}
			closePlugins()
			closePlugins()
		})
	}
}

// TestLoadRefusesAFileAsADirectory pins that a plugins path naming a file, not a directory, fails
// startup with ErrLoad. Starting anyway would leave an operator who mistyped the setting with a
// server that runs with none of the seams they configured and says nothing about it.
func TestLoadRefusesAFileAsADirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "plugins")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, zap.NewNop()); !errors.Is(err, ErrLoad) {
		t.Errorf("Load(file) error = %v, want ErrLoad", err)
	}
}

// TestValidateRefusesEveryUnregistrableName pins the whole refusal surface of a Describe response,
// which is untrusted input from a third-party binary. Every name here would panic a shared registry,
// and that panic unwinds through the load loop past every plugin left to load, so one bad binary
// would take the rest of the server's extensions down with it.
//
//nolint:funlen // Test function.
func TestValidateRefusesEveryUnregistrableName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Desc *extproto.DescribeResponse
		Want error
	}{{ // Test 0: A tool name of only spaces is refused, since the registry keys on it verbatim.
		Name: "whitespace tool", Desc: &extproto.DescribeResponse{Tools: []string{"   "}},
		Want: ErrPluginContract,
	}, { // Test 1: A tool name of only a tab and a newline is refused.
		Name: "tab and newline tool", Desc: &extproto.DescribeResponse{Tools: []string{"\t\n"}},
		Want: ErrPluginContract,
	}, { // Test 2: A blank AI provider name is refused.
		Name: "whitespace ai provider", Desc: &extproto.DescribeResponse{AiProviders: []string{" "}},
		Want: ErrPluginContract,
	}, { // Test 3: A blank dynamic secret kind is refused.
		Name: "whitespace dynamic secret",
		Desc: &extproto.DescribeResponse{DynamicSecretSources: []string{" "}},
		Want: ErrPluginContract,
	}, { // Test 4: An AI provider that matches a built-in only after lowercasing is refused, because
		// that is how the AI registry keys a name.
		Name: "ai provider case collision",
		Desc: &extproto.DescribeResponse{AiProviders: []string{"OpenAI"}}, Want: ErrPluginContract,
	}, { // Test 5: The same collision with surrounding space, which the registry also trims.
		Name: "ai provider space collision",
		Desc: &extproto.DescribeResponse{AiProviders: []string{" anthropic "}},
		Want: ErrPluginContract,
	}, { // Test 6: Two AI providers differing only in case collide with each other.
		Name: "ai providers collide with each other",
		Desc: &extproto.DescribeResponse{AiProviders: []string{"pluginai", "PluginAI"}},
		Want: ErrPluginContract,
	}, { // Test 7: A secret kind that is the built-in default is refused.
		Name: "secret kind local",
		Desc: &extproto.DescribeResponse{SecretSources: []string{secretsource.KindLocal}},
		Want: ErrPluginContract,
	}, { // Test 8: A built-in secret engine kind is refused.
		Name: "secret kind vault",
		Desc: &extproto.DescribeResponse{SecretSources: []string{secretsource.KindVault}},
		Want: ErrPluginContract,
	}, { // Test 9: A built-in dynamic engine kind is refused.
		Name: "dynamic kind aws sts",
		Desc: &extproto.DescribeResponse{DynamicSecretSources: []string{secretsource.KindAWSSTS}},
		Want: ErrPluginContract,
	}, { // Test 10: A tool declared twice in one response is refused.
		Name: "tool declared twice",
		Desc: &extproto.DescribeResponse{Tools: []string{"plugintool", "plugintool"}},
		Want: ErrPluginContract,
	}, { // Test 11: Every built-in execution tool is claimed, ansible included.
		Name: "tool collides with ansible",
		Desc: &extproto.DescribeResponse{Tools: []string{run.ToolAnsible}}, Want: ErrPluginContract,
	}, { // Test 12: A dynamic kind repeating a static kind from the same response is refused.
		Name: "dynamic repeats a static kind",
		Desc: &extproto.DescribeResponse{
			SecretSources:        []string{"a", "b"},
			DynamicSecretSources: []string{"c", "a"},
		},
		Want: ErrPluginContract,
	}, { // Test 13: A response with nothing in it is registrable, since it registers nothing.
		Name: "empty response", Desc: &extproto.DescribeResponse{},
	}, { // Test 14: Unicode names nothing has claimed are registrable.
		Name: "unicode names",
		Desc: &extproto.DescribeResponse{
			Tools: []string{"außenwerkzeug"}, Notifiers: []string{"通知"},
			AiProviders: []string{"モデル"}, SecretSources: []string{"секрет"},
		},
	}, { // Test 15: A very long name nothing has claimed is registrable.
		Name: "very long name",
		Desc: &extproto.DescribeResponse{Tools: []string{strings.Repeat("t", 4096)}},
	}, { // Test 16: Names differing only in surrounding space are distinct in a verbatim namespace, so
		// this is accepted rather than treated as a duplicate.
		Name: "tools differing by space",
		Desc: &extproto.DescribeResponse{Tools: []string{"plugintool", "plugintool "}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := validate(test.Desc)
			if !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): validate() = %v, want %v", testNum, test.Name, err, test.Want)
			}
		})
	}
}

// TestValidateRefusesTheFirstBadNameInAResponse pins that a response holding several bad names is
// refused on the first one and never partly accepted. The whole response is rejected either way, so
// what matters is that no list is skipped: a bad name late in a response must not slip past because
// an earlier list was clean.
func TestValidateRefusesTheFirstBadNameInAResponse(t *testing.T) {
	t.Parallel()
	desc := &extproto.DescribeResponse{
		Tools:                []string{"plugintool"},
		Notifiers:            []string{"pluginchan"},
		AiProviders:          []string{"pluginai"},
		SecretSources:        []string{"pluginvault"},
		DynamicSecretSources: []string{"vault_dynamic"},
	}
	err := validate(desc)
	if !errors.Is(err, ErrPluginContract) {
		t.Fatalf("validate() = %v, want ErrPluginContract for the last list's collision", err)
	}
	if !strings.Contains(err.Error(), "vault_dynamic") {
		t.Errorf("err = %q, want it to name the offending kind", err)
	}
}

// TestCheckNamespaceUsesTheRegistrysNormalization pins that the duplicate check keys names the same
// way the registry does. A check that normalized differently would pass a pair the registry then
// panics on, which is the panic the whole validation step exists to prevent.
func TestCheckNamespaceUsesTheRegistrysNormalization(t *testing.T) {
	t.Parallel()
	never := func(string) bool { return false }
	tests := []struct {
		Name  string
		Names []string
		Norm  func(string) string
		Want  error
	}{{ // Test 0: Under the AI registry's normalization, case variants are one name.
		Name: "ai normalization collapses case", Names: []string{"Foo", "foo"}, Norm: aiNormalize,
		Want: ErrPluginContract,
	}, { // Test 1: Under a verbatim namespace, case variants are two names.
		Name: "identity keeps case apart", Names: []string{"Foo", "foo"}, Norm: identity,
	}, { // Test 2: An empty list is registrable.
		Name: "empty list", Norm: identity,
	}, { // Test 3: One name cannot collide with itself.
		Name: "single name", Names: []string{"only"}, Norm: identity,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkNamespace(test.Names, never, test.Norm)
			if !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): checkNamespace() = %v, want %v",
					testNum, test.Name, err, test.Want)
			}
		})
	}
}

// TestCheckNamespaceRefusesATakenNameBeforeADuplicate pins that a name an earlier plugin already
// claimed is refused, which is what keeps the second of two plugins declaring the same seam from
// panicking the registry mid-startup.
func TestCheckNamespaceRefusesATakenNameBeforeADuplicate(t *testing.T) {
	t.Parallel()
	taken := func(name string) bool { return name == "claimed" }
	err := checkNamespace([]string{"free", "claimed"}, taken, identity)
	if !errors.Is(err, ErrPluginContract) {
		t.Fatalf("checkNamespace() = %v, want ErrPluginContract", err)
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("err = %q, want it to say the name is already registered", err)
	}
}

// TestPluginEnvBoundaries pins the edges of the allowlist that decides what a third-party binary can
// read from this server's environment. The list is deny-by-default because the deny list grows every
// time the server learns to read another secret from the environment, and a missed entry hands that
// secret to every plugin on the machine.
//
// It does not call t.Parallel: it sets process environment variables.
func TestPluginEnvBoundaries(t *testing.T) {
	tests := []struct {
		Name     string
		Key      string
		Value    string
		WantPass bool
	}{{ // Test 0: A variable every process needs passes.
		Name: "path passes", Key: "PATH", Value: "/usr/bin", WantPass: true,
	}, { // Test 1: The allowlist is matched case-insensitively, for platforms whose variables vary.
		Name: "lowercase allowlist entry passes", Key: "Path", Value: "/usr/local/bin", WantPass: true,
	}, { // Test 2: The operator's namespaced configuration passes.
		Name: "namespaced variable passes", Key: "SWITCHTENDER_PLUGIN_X", Value: "1", WantPass: true,
	}, { // Test 3: The bare prefix with nothing after it still counts as namespaced.
		Name: "bare prefix passes", Key: "SWITCHTENDER_PLUGIN_", Value: "1", WantPass: true,
	}, { // Test 4: A namespaced variable with an empty value still passes, since the operator set it.
		Name: "empty namespaced value passes", Key: "SWITCHTENDER_PLUGIN_EMPTY", WantPass: true,
	}, { // Test 5: A name one character short of the prefix is withheld.
		Name: "near miss on the prefix is withheld", Key: "SWITCHTENDER_PLUGINX", Value: "1",
	}, { // Test 6: A server variable outside the prefix is withheld even when it holds no secret.
		Name: "server variable withheld", Key: "SWITCHTENDER_LISTEN_ADDR", Value: ":8080",
	}, { // Test 7: A name that merely contains the prefix is withheld, since the check anchors it.
		Name: "prefix in the middle is withheld", Key: "X_SWITCHTENDER_PLUGIN_Y", Value: "1",
	}, { // Test 8: The dynamic linker preload variable is not on the list and is withheld, so the
		// server cannot hand a plugin a library injection it inherited.
		Name: "linker preload withheld", Key: "LD_PRELOAD", Value: "/tmp/evil.so",
	}, { // Test 9: The macOS equivalent is withheld too.
		Name: "dyld insert withheld", Key: "DYLD_INSERT_LIBRARIES", Value: "/tmp/evil.dylib",
	}, { // Test 10: Cloud credentials in the ambient environment are withheld.
		Name: "cloud credentials withheld", Key: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/creds.json",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Setenv(test.Key, test.Value)
			env := pluginEnv()
			passed := slices.Contains(env, test.Key+"="+test.Value)
			if passed != test.WantPass {
				t.Errorf("test %d (%s): %s passed to the plugin = %v, want %v (env %v)",
					testNum, test.Name, test.Key, passed, test.WantPass, env)
			}
		})
	}
}

// TestPluginEnvIsAnAllowlist pins the shape of the decision rather than one variable's fate: nothing
// reaches a plugin unless it is a named pass-through or carries the plugin prefix. A future variable
// this server learns to read is withheld by default, which is the property the allowlist exists for.
//
// It does not call t.Parallel: it sets process environment variables.
func TestPluginEnvIsAnAllowlist(t *testing.T) {
	t.Setenv("SWITCHTENDER_SOME_FUTURE_SECRET", "value-nobody-added-to-a-deny-list")
	t.Setenv("A_THIRD_PARTY_TOKEN", "another-value")
	for _, kv := range pluginEnv() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			t.Errorf("plugin environment entry %q has no value separator", kv)
			continue
		}
		if pluginPassThrough[strings.ToUpper(name)] || strings.HasPrefix(name, pluginEnvPrefix) {
			continue
		}
		t.Errorf("%q reached the plugin environment without being allowed", name)
	}
}

// TestZapSinkFieldMapping pins how a plugin's log line reaches this server's logger. go-plugin routes
// a plugin crash and its panic trace through this sink, so a line that loses its level or drops its
// fields is a crash an operator cannot diagnose after the fact.
func TestZapSinkFieldMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Logger     string
		Args       []any
		WantFields map[string]any
	}{{ // Test 0: Key and value pairs become fields, and the logger name rides along.
		Name: "pairs become fields", Logger: "plugin.stderr",
		Args:       []any{"pid", int64(42), "file", "hello"},
		WantFields: map[string]any{"logger": "plugin.stderr", "pid": int64(42), "file": "hello"},
	}, { // Test 1: An unnamed logger adds no logger field.
		Name: "no logger name", Args: []any{"k", "v"},
		WantFields: map[string]any{"k": "v"},
	}, { // Test 2: A trailing key with no value is dropped rather than logged as a half pair.
		Name: "odd trailing key", Args: []any{"k", "v", "dangling"},
		WantFields: map[string]any{"k": "v"},
	}, { // Test 3: A key that is not a string is formatted rather than dropped, since hclog allows it.
		Name: "non-string key", Args: []any{7, "v"},
		WantFields: map[string]any{"7": "v"},
	}, { // Test 4: A line with no arguments carries no fields.
		Name: "no arguments", WantFields: map[string]any{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zapcore.DebugLevel)
			zapSink{log: zap.New(core)}.Accept(test.Logger, hclog.Error, "message", test.Args...)
			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("test %d (%s): logged %d entries, want 1", testNum, test.Name, len(entries))
			}
			got := entries[0].ContextMap()
			if len(got) != len(test.WantFields) {
				t.Fatalf("test %d (%s): fields = %v, want %v", testNum, test.Name, got, test.WantFields)
			}
			for k, want := range test.WantFields {
				if fmt.Sprintf("%v", got[k]) != fmt.Sprintf("%v", want) {
					t.Errorf("test %d (%s): field %q = %v, want %v",
						testNum, test.Name, k, got[k], want)
				}
			}
		})
	}
}

// TestZapSinkKeepsTheMessage pins that the plugin's own text survives the hop, since it is often the
// only record of why a plugin process died.
func TestZapSinkKeepsTheMessage(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zapcore.DebugLevel)
	const msg = "panic: runtime error: invalid memory address"
	zapSink{log: zap.New(core)}.Accept("", hclog.Error, msg)
	if logs.Len() != 1 || logs.All()[0].Message != msg {
		t.Errorf("logged %v, want the plugin's own message %q", logs.All(), msg)
	}
}
