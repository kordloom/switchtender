package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// serviceSettings reads the [Service] section of a systemd unit file into a map, the last
// assignment of a key winning the way systemd applies most of them.
func serviceSettings(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return unitServiceSettings(t, string(raw))
}

// unitServiceSettings reads the [Service] section of a unit's text into a map.
func unitServiceSettings(t *testing.T, unit string) map[string]string {
	t.Helper()
	settings := map[string]string{}
	section := ""
	scan := bufio.NewScanner(strings.NewReader(unit))
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			section = line
		case section == "[Service]":
			if k, v, ok := strings.Cut(line, "="); ok {
				settings[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatalf("read a unit: %v", err)
	}
	return settings
}

// TestShippedSystemdUnits holds the unit files under deploy/systemd, and the one init writes, to
// what the run-files design depends on: a private /tmp, a memory-backed runtime directory of its
// own that systemd removes when the service stops, no core files, no privilege escalation, and a
// stop that lets the process end its runs itself. It also holds each ExecStart to the command line
// the binary actually accepts, since a unit naming a flag that does not exist fails at boot rather
// than in review.
func TestShippedSystemdUnits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// File is the unit file name.
		File string
		// WantCommand is the subcommand ExecStart runs.
		WantCommand string
	}{{ // Test 0: The server.
		File: "switchtender.service", WantCommand: "serve",
	}, { // Test 1: The worker.
		File: "switchtender-worker.service", WantCommand: "worker",
	}, { // Test 2: The unit switchtender init --systemd writes.
		File: "init --systemd", WantCommand: "serve",
	}}
	units := map[string]map[string]string{}
	for _, test := range tests[:2] {
		units[test.File] = serviceSettings(t, filepath.Join("..", "deploy", "systemd", test.File))
	}
	units["init --systemd"] = unitServiceSettings(t, systemdUnit(unitSpec{DB: "/var/lib/st/st.db",
		Addr: "127.0.0.1:8080", Exe: "/usr/local/bin/switchtender", WorkDir: "/var/lib/st",
		User: "st", Group: "st"}))
	server, worker := units[tests[0].File], units[tests[1].File]
	if a, b := server["RuntimeDirectory"], worker["RuntimeDirectory"]; a == b {
		t.Errorf("the server and the worker share the runtime directory %q, so stopping one would "+
			"remove the other's run files", a)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.File), func(t *testing.T) {
			t.Parallel()
			s := units[test.File]
			got := map[string]string{}
			for _, k := range []string{"PrivateTmp", "LimitCORE", "RuntimeDirectoryMode",
				"RuntimeDirectoryPreserve", "KillMode", "NoNewPrivileges"} {
				got[k] = s[k]
			}
			want := map[string]string{"PrivateTmp": "true", "LimitCORE": "0",
				"RuntimeDirectoryMode": "0700", "RuntimeDirectoryPreserve": "no", "KillMode": "mixed",
				"NoNewPrivileges": "true"}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("%s settings (-want +got):\n%s", test.File, diff)
			}
			if s["RuntimeDirectory"] == "" {
				t.Errorf("%s names no RuntimeDirectory, so run files fall back to disk", test.File)
			}
			args := strings.Fields(s["ExecStart"])
			if len(args) < 2 || filepath.Base(args[0]) != "switchtender" || args[1] != test.WantCommand {
				t.Fatalf("%s ExecStart = %q, want switchtender %s", test.File, s["ExecStart"],
					test.WantCommand)
			}
			sub := findCommand(t, test.WantCommand)
			for _, arg := range args[2:] {
				if !strings.HasPrefix(arg, "--") {
					continue
				}
				name, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
				if sub.Flags().Lookup(name) == nil {
					t.Errorf("%s ExecStart passes --%s, which %s does not accept", test.File, name,
						test.WantCommand)
				}
			}
		})
	}
}

// TestSystemdUnitsKeepTheKeyFileFromRuns holds every unit to loading its key from a file only root
// reads. systemd reads EnvironmentFile as root before it starts the service, and a run with no
// execution image runs as the service's account, so a key file that account could read, as the
// shipped headers once installed it at 0640 for the service group, was one cat away from any run.
func TestSystemdUnitsKeepTheKeyFileFromRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// File is the unit file name under deploy/systemd.
		File string
		// WantEnvFile is the environment file the unit loads.
		WantEnvFile string
	}{{ // Test 0: The server.
		File: "switchtender.service", WantEnvFile: "/etc/switchtender/switchtender.env",
	}, { // Test 1: The worker.
		File: "switchtender-worker.service", WantEnvFile: "/etc/switchtender/worker.env",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.File), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join("..", "deploy", "systemd", test.File)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if got := serviceSettings(t, path)["EnvironmentFile"]; got != test.WantEnvFile {
				t.Errorf("%s EnvironmentFile = %q, want %q", test.File, got, test.WantEnvFile)
			}
			want := "#   install -m 0600 -o root -g root /dev/null " + test.WantEnvFile + "\n"
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s header does not install its environment file root-owned at 0600, "+
					"want the line %q", test.File, strings.TrimSpace(want))
			}
			if strings.Contains(string(raw), "0640") {
				t.Errorf("%s still names mode 0640, which lets the service group read the key", test.File)
			}
		})
	}
	unit := unitServiceSettings(t, systemdUnit(unitSpec{DB: "/var/lib/st/st.db", Addr: "127.0.0.1:8080",
		Exe: "/usr/local/bin/switchtender", WorkDir: "/var/lib/st", User: "st", Group: "st"}))
	if got := unit["EnvironmentFile"]; got != systemEnvFile {
		t.Errorf("the unit init writes loads %q, want the root-owned %q", got, systemEnvFile)
	}
}
