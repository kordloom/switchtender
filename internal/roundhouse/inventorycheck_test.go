//go:build unix

package roundhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadInventoriesOnTheHost pins the host cross-check reading: it runs the real
// ansible-inventory on each file and reports its version, and it reads with the run's project
// ansible.cfg, so a configuration that changes how Ansible parses an inventory changes the check
// the same way.
func TestReadInventoriesOnTheHost(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath(defaultInventoryBinary); err != nil {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and %s is missing", defaultInventoryBinary)
		}
		t.Skip("ansible-inventory is not installed")
	}
	tests := []struct {
		// Config is the project's ansible.cfg, empty for none.
		Config string
		// WantErr says the reading must fail.
		WantErr bool
	}{{ // Test 0: An INI file reads with the defaults.
		Config: "", WantErr: false,
	}, { // Test 1: A project ansible.cfg enabling only the YAML plugin makes the INI file unreadable.
		Config: "[inventory]\nenable_plugins = yaml\n", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir, project := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "input-000"), []byte("[web]\nweb1 port=80\n"),
				0o600); err != nil {
				t.Fatal(err)
			}
			if test.Config != "" {
				if err := os.WriteFile(filepath.Join(project, "ansible.cfg"), []byte(test.Config),
					0o600); err != nil {
					t.Fatal(err)
				}
			}
			a := newAnsibleRunner()
			a.baseEnv = append(stubBaseEnv(), "HOME="+t.TempDir())
			outs, version, err := a.ReadInventories(context.Background(), Spec{Dir: project}, dir,
				[]string{"input-000"})
			if (err != nil) != test.WantErr {
				t.Fatalf("ReadInventories() error = %v, want error %v", err, test.WantErr)
			}
			if err != nil {
				return
			}
			if !strings.HasPrefix(version, "2.") {
				t.Errorf("version = %q, want an ansible-core version", version)
			}
			var doc map[string]any
			if err := json.Unmarshal(outs[0], &doc); err != nil {
				t.Fatalf("listing is not JSON: %v", err)
			}
			if !strings.Contains(string(outs[0]), `"web1"`) {
				t.Errorf("listing does not name web1: %s", outs[0])
			}
		})
	}
}

// TestReadInventoriesInsideTheImage pins the container cross-check: one container of the run's own
// image reads every file, with the check directory mounted writable and the project read-only, and
// the listings and version it writes beside the files are what the check reports.
func TestReadInventoriesInsideTheImage(t *testing.T) {
	t.Parallel()
	stubDir, dir := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\n" +
		`[ "$1" = stub-warmup ] && exit 0` + "\n" +
		`echo "$@" >> ` + filepath.Join(stubDir, "calls.log") + "\n" +
		`if [ "$1" = run ]; then` + "\n" +
		`  echo '{"web": {"hosts": ["web1"]}}' > ` + filepath.Join(dir, "input-000.json") + "\n" +
		`  echo '{"ungrouped": {"hosts": ["web1"]}}' > ` + filepath.Join(dir, "inventory.json") + "\n" +
		`  echo 'ansible-inventory [core 2.15.13]' > ` + filepath.Join(dir, "version.txt") + "\n" +
		"fi\nexit 0\n"
	bin := filepath.Join(stubDir, "docker")
	writeStub(t, bin, script)
	c := newContainerRunner(bin, "missing", false, stubBaseEnv(), &pluginCache{},
		DefaultContainerLimits())
	project := t.TempDir()
	outs, version, err := c.readInventories(context.Background(),
		Spec{Image: "registry.example/ee:1", Dir: project, Env: []string{"CLOUD_TOKEN=x"}}, dir,
		[]string{"input-000", "inventory"})
	if err != nil {
		t.Fatalf("readInventories() error = %v", err)
	}
	if version != "2.15.13" {
		t.Errorf("version = %q, want the image's 2.15.13", version)
	}
	if len(outs) != 2 || !strings.Contains(string(outs[1]), "ungrouped") {
		t.Errorf("listings = %q, want one per file in order", outs)
	}
	calls := readStubFile(t, stubDir, "calls.log")
	for _, want := range []string{"registry.example/ee:1", "-v " + dir + ":" + dir + " ",
		project + ":" + project + ":ro", "ansible-inventory -i", "input-000 inventory"} {
		if !strings.Contains(calls, want) {
			t.Errorf("the container run lacks %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, dir+":"+dir+":ro") {
		t.Errorf("the check directory is mounted read-only, so the listings cannot be written:\n%s",
			calls)
	}
}
