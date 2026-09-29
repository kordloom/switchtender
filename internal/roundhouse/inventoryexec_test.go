//go:build unix

package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// stubInventory puts a stand-in ansible-inventory on PATH and returns the file its arguments are
// appended to. The binary name is a package constant rather than an option, so the only way to
// exercise Hosts and Dump without Ansible installed is to own PATH, which is why the tests using
// this helper do not run in parallel.
func stubInventory(t *testing.T, body string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		`[ "$1" = stub-warmup ] && exit 0` + "\n" +
		`echo "$@" >> ` + logPath + "\n" + body
	writeStub(t, filepath.Join(dir, defaultInventoryBinary), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestHostsEnumeratesAndFailsClosed covers the enumeration a sharded run is built on. A shard that
// believes the fleet holds fewer hosts than it does runs against the wrong set, and an enumeration
// that returns an empty list instead of an error turns a broken inventory into a silently unsharded
// run, so every failure mode here has to surface as an error rather than as no hosts.
//
//nolint:funlen // Test function.
func TestHostsEnumeratesAndFailsClosed(t *testing.T) {
	// No t.Parallel: the subtests set PATH, which is process-global.
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Body is the stub's behavior after it has logged its arguments.
		Body string
		// Inventory is the inventory argument.
		Inventory string
		// Limit narrows enumeration to a host pattern.
		Limit string
		// WantHosts is the sorted host set expected.
		WantHosts []string
		// WantArgs are substrings the logged argument line must contain.
		WantArgs []string
		// WantAbsentArgs are substrings the logged argument line must not contain.
		WantAbsentArgs []string
		// Want is the expected error.
		Want error
	}{{ // Test 0: An empty inventory is refused before anything is launched.
		Name: "no inventory", Body: "exit 0", Inventory: "", Want: ErrNoInventory,
	}, { // Test 1: Hosts come back sorted and deduplicated, and no --limit is passed without one.
		Name: "plain enumeration",
		Body: `echo '{"_meta":{"hostvars":{}},"web":{"hosts":["web02","web01"]},` +
			`"db":{"hosts":["db01","web01"]}}'` + "\n",
		Inventory: "hosts.ini", WantHosts: []string{"db01", "web01", "web02"},
		WantArgs: []string{"-i hosts.ini", "--list"}, WantAbsentArgs: []string{"--limit"},
	}, { // Test 2: A limit is handed to ansible-inventory so a shard cannot reach an excluded host.
		Name:      "limited enumeration",
		Body:      `echo '{"_meta":{"hostvars":{}},"web":{"hosts":["web01"]}}'` + "\n",
		Inventory: "hosts.ini", Limit: "web*", WantHosts: []string{"web01"},
		WantArgs: []string{"-i hosts.ini", "--list", "--limit web*"},
	}, { // Test 3: A failing ansible-inventory is an executor error, not an empty fleet.
		Name: "inventory command fails", Body: "echo boom >&2\nexit 3\n",
		Inventory: "hosts.ini", Want: ErrLaunch,
	}, { // Test 4: Output that is not JSON is a parse error, not an empty fleet.
		Name: "unparseable output", Body: "echo not-json\n", Inventory: "hosts.ini",
		Want: ErrInventoryParse,
	}, { // Test 5: A JSON document that is not an object is a parse error.
		Name: "json array", Body: `echo '["web01"]'` + "\n", Inventory: "hosts.ini",
		Want: ErrInventoryParse,
	}, { // Test 6: A _meta of the wrong shape is a parse error, since hosts may be hiding in it.
		Name: "bad meta", Body: `echo '{"_meta":"nonsense"}'` + "\n", Inventory: "hosts.ini",
		Want: ErrInventoryParse,
	}, { // Test 7: A group that is not an object is skipped, since "all" carries only children.
		Name:      "group of the wrong shape",
		Body:      `echo '{"_meta":{"hostvars":{}},"weird":"string","web":{"hosts":["web01"]}}'` + "\n",
		Inventory: "hosts.ini", WantHosts: []string{"web01"},
	}, { // Test 8: An inventory with no hosts is empty rather than an error.
		Name: "empty inventory", Body: `echo '{"_meta":{"hostvars":{}}}'` + "\n",
		Inventory: "hosts.ini", WantHosts: nil,
	}, { // Test 9: Non-ASCII host names survive enumeration intact.
		Name:      "unicode host names",
		Body:      `echo '{"_meta":{"hostvars":{}},"web":{"hosts":["høst-én","zz"]}}'` + "\n",
		Inventory: "hosts.ini", WantHosts: []string{"høst-én", "zz"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			logPath := stubInventory(t, test.Body)
			got, err := newAnsibleRunner().Hosts(context.Background(), test.Inventory, test.Limit)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: Hosts() error = %v, want %v", test.Name, err, test.Want)
			}
			if test.Want != nil {
				if got != nil {
					t.Errorf("%s: a failed enumeration returned %v, want no hosts at all",
						test.Name, got)
				}
				return
			}
			if diff := cmp.Diff(test.WantHosts, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: hosts mismatch (-want +got):\n%s", test.Name, diff)
			}
			calls, _ := os.ReadFile(logPath)
			for _, want := range test.WantArgs {
				if !strings.Contains(string(calls), want) {
					t.Errorf("%s: arguments %q missing %q", test.Name, calls, want)
				}
			}
			for _, absent := range test.WantAbsentArgs {
				if strings.Contains(string(calls), absent) {
					t.Errorf("%s: arguments %q should not contain %q", test.Name, calls, absent)
				}
			}
		})
	}
}

// TestDumpReturnsTheSourceVerbatimAndLayersItsEnvironment pins the dynamic inventory path. The bytes
// are handed on to ansible-playbook unchanged, so any reshaping here would corrupt an inventory, and
// a cloud plugin only finds its account when the caller's environment is layered over the base.
func TestDumpReturnsTheSourceVerbatimAndLayersItsEnvironment(t *testing.T) {
	// No t.Parallel: stubInventory sets PATH.
	const doc = `{"_meta":{"hostvars":{}},"web":{"hosts":["web01"]}}`
	logPath := stubInventory(t, "printf '%s' \"${AWS_PROFILE:-unset}\"\n")
	a := newAnsibleRunner()

	out, err := a.Dump(context.Background(), "aws_ec2.yml", []string{"AWS_PROFILE=prod"})
	if err != nil {
		t.Fatalf("Dump() error = %v", err)
	}
	if diff := cmp.Diff("prod", string(out)); diff != "" {
		t.Errorf("the caller's environment did not reach the plugin (-want +got):\n%s", diff)
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "-i aws_ec2.yml --list") {
		t.Errorf("arguments %q, want the source listed", calls)
	}

	// The document comes back byte for byte, since ansible-playbook consumes it directly.
	stubInventory(t, "printf '%s' '"+doc+"'\n")
	raw, err := newAnsibleRunner().Dump(context.Background(), "aws_ec2.yml", nil)
	if err != nil {
		t.Fatalf("second Dump() error = %v", err)
	}
	if diff := cmp.Diff(doc, string(raw)); diff != "" {
		t.Errorf("dumped inventory mismatch (-want +got):\n%s", diff)
	}
}

// TestDumpFailsClosed pins that a dump with no source and a dump whose plugin fails both return an
// error rather than empty bytes. Empty bytes downstream parse as an inventory with no hosts, which
// is a run that quietly targets nothing and reports success.
func TestDumpFailsClosed(t *testing.T) {
	// No t.Parallel: stubInventory sets PATH.
	if _, err := newAnsibleRunner().Dump(context.Background(), "", nil); !errors.Is(err, ErrNoInventory) {
		t.Errorf("Dump(\"\") error = %v, want ErrNoInventory", err)
	}

	stubInventory(t, "echo denied >&2\nexit 2\n")
	out, err := newAnsibleRunner().Dump(context.Background(), "aws_ec2.yml", nil)
	if !errors.Is(err, ErrLaunch) {
		t.Errorf("Dump() error = %v, want ErrLaunch", err)
	}
	if out != nil {
		t.Errorf("a failed dump returned %q, want no bytes at all", out)
	}
}

// TestInventoryCommandsAreNotFoundWhenTheToolIsMissing pins that an absent ansible-inventory is an
// executor error. The enumeration underpins sharding, so an executor without Ansible must say so
// rather than report a fleet of zero hosts.
func TestInventoryCommandsAreNotFoundWhenTheToolIsMissing(t *testing.T) {
	// No t.Parallel: it replaces PATH for the process.
	t.Setenv("PATH", t.TempDir())
	a := newAnsibleRunner()
	if _, err := a.Hosts(context.Background(), "hosts.ini", ""); !errors.Is(err, ErrLaunch) {
		t.Errorf("Hosts() error = %v, want ErrLaunch", err)
	}
	if _, err := a.Dump(context.Background(), "hosts.ini", nil); !errors.Is(err, ErrLaunch) {
		t.Errorf("Dump() error = %v, want ErrLaunch", err)
	}
}
