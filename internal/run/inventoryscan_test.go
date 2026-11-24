package run

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// TestTheInventoryScanIsStrict pins what the inventory pass reads in a tree: every variable a group
// or a host sets judged by name and by value, an executable file, a symbolic link, or a #! line as a
// program, and whatever only Ansible can resolve as unread. Each finding row has a clean twin, so a
// scan that flagged everything would fail as surely as one that flagged nothing.
//
//nolint:funlen // Test table.
func TestTheInventoryScanIsStrict(t *testing.T) {
	t.Parallel()
	const proxy = "[all:vars]\nansible_ssh_common_args='-o ProxyCommand=\"sh -c id\"'\n\n[web]\nweb01\n"
	tests := []struct {
		// Files is the tree, by path.
		Files fstest.MapFS
		// Name is the inventory the run names.
		Name string
		// WantFinding is a fragment one finding must hold, empty for none.
		WantFinding string
		// WantUnread is a fragment one unread entry must hold, empty for none.
		WantUnread string
	}{{ // Test 0: An INI host line setting ssh arguments.
		Files: inv("hosts.ini", "web01 ansible_ssh_common_args='-o ProxyCommand=\"sh -c id\"'\n"),
		Name:  "hosts.ini", WantFinding: `host "web01" sets "ansible_ssh_common_args"`,
	}, { // Test 1: An INI group variable pointing the connection at the controller.
		Files: inv("hosts.ini", "[all:vars]\nansible_connection=local\n\n[web]\nweb01\n"),
		Name:  "hosts.ini", WantFinding: `group "all" sets "ansible_connection"`,
	}, { // Test 2: A YAML group variable naming an interpreter.
		Files: inv("hosts.yml", "all:\n  vars:\n    ansible_python_interpreter: /usr/bin/python3\n"+
			"  hosts:\n    web01:\n"),
		Name: "hosts.yml", WantFinding: `group "all" sets "ansible_python_interpreter"`,
	}, { // Test 3: An address the scan allows by name, holding a pipe lookup.
		Files: inv("hosts.yml", "all:\n  hosts:\n    web01:\n"+
			"      ansible_host: \"{{ lookup('pipe', 'id') }}\"\n"),
		Name: "hosts.yml", WantFinding: `sets "ansible_host" to template code`,
	}, { // Test 4: A user the scan allows by name, holding a lookup.
		Files: inv("hosts.ini", "web01 ansible_user=\"{{ lookup('pipe','id') }}\"\n"),
		Name:  "hosts.ini", WantFinding: `sets "ansible_user" to template code`,
	}, { // Test 5: A group with no host still sets the variables of the implicit localhost.
		Files: inv("hosts.ini", "[all:vars]\nansible_python_interpreter=./x.sh\n"),
		Name:  "hosts.ini", WantFinding: `group "all" sets "ansible_python_interpreter"`,
	}, { // Test 6: A clean INI inventory.
		Files: inv("hosts.ini", "[web]\nweb01 ansible_host=10.0.0.1 ansible_user=deploy ansible_port=2222\n"),
		Name:  "hosts.ini",
	}, { // Test 7: A clean YAML inventory, with a !unsafe value Ansible never templates.
		Files: inv("hosts.yml", "all:\n  hosts:\n    web01:\n      ansible_user: deploy\n"+
			"      motd: !unsafe '{{ not templated }}'\n"),
		Name: "hosts.yml",
	}, { // Test 8: An executable inventory file is a program.
		Files: fstest.MapFS{"inv.py": {Data: []byte("[web]\nweb01\n"), Mode: 0o755}},
		Name:  "inv.py", WantFinding: `"inv.py" is executable`,
	}, { // Test 9: The same bytes, not executable, are read as data.
		Files: fstest.MapFS{"inv.py": {Data: []byte("[web]\nweb01\n"), Mode: 0o644}},
		Name:  "inv.py",
	}, { // Test 10: A #! line with no execute bit is a program waiting for one.
		Files: inv("hosts", "#!/bin/sh\n[web]\nweb01\n"),
		Name:  "hosts", WantFinding: `"hosts" starts with #!`,
	}, { // Test 11: A directory holding one executable file.
		Files: fstest.MapFS{
			"inventory/static.ini": {Data: []byte("[web]\nweb01\n"), Mode: 0o644},
			"inventory/ec2.py":     {Data: []byte("print('{}')\n"), Mode: 0o755},
		},
		Name: "inventory", WantFinding: `"inventory/ec2.py" is executable`,
	}, { // Test 12: A clean directory, its README and hidden files skipped the way Ansible skips them.
		Files: fstest.MapFS{
			"inventory/static.ini": {Data: []byte("[web]\nweb01\n"), Mode: 0o644},
			"inventory/README.md":  {Data: []byte("# ansible_connection=local\n"), Mode: 0o755},
			"inventory/.notes":     {Data: []byte("{{ lookup('pipe','id') }}\n"), Mode: 0o644},
		},
		Name: "inventory",
	}, { // Test 13: An inventory directory whose host_vars set an ssh executable.
		Files: fstest.MapFS{
			"inventory/hosts": {Data: []byte("[web]\nweb01\n"), Mode: 0o644},
			"inventory/host_vars/web01.yml": {Data: []byte("ansible_ssh_executable: /tmp/x\n"),
				Mode: 0o644},
		},
		Name: "inventory", WantFinding: `sets "ansible_ssh_executable"`,
	}, { // Test 14: group_vars beside an inventory file.
		Files: fstest.MapFS{
			"inv/hosts.ini":          {Data: []byte("[web]\nweb01\n"), Mode: 0o644},
			"inv/group_vars/all.yml": {Data: []byte("ansible_shell_executable: /tmp/sh\n"), Mode: 0o644},
		},
		Name: "inv/hosts.ini", WantFinding: `sets "ansible_shell_executable"`,
	}, { // Test 15: A plugin configuration only Ansible runs.
		Files: inv("aws_ec2.yml", "plugin: amazon.aws.aws_ec2\nregions: [us-east-1]\n"),
		Name:  "aws_ec2.yml", WantUnread: "inventory plugin configuration",
	}, { // Test 16: A constructed plugin configuration, which templates on the controller.
		Files: inv("built.yml", "plugin: ansible.builtin.constructed\ncompose:\n  x: lookup('pipe','id')\n"),
		Name:  "built.yml", WantUnread: "inventory plugin configuration",
	}, { // Test 17: A whole file encrypted with Ansible Vault.
		Files: inv("hosts", "$ANSIBLE_VAULT;1.1;AES256\n6162\n"),
		Name:  "hosts", WantUnread: "Ansible Vault",
	}, { // Test 18: A symbolic link is not followed.
		Files: fstest.MapFS{
			"real.ini":  {Data: []byte("[web]\nweb01\n"), Mode: 0o644},
			"hosts.ini": {Data: []byte("real.ini"), Mode: fs.ModeSymlink | 0o777},
		},
		Name: "hosts.ini", WantFinding: "is a symbolic link",
	}, { // Test 19: A file named .ini is read as INI alone, and must parse as INI.
		Files: inv("hosts.ini", "all:\n  hosts:\n    web01:\n      ansible_user: deploy\n"),
		Name:  "hosts.ini", WantUnread: "does not parse as an inventory Ansible reads",
	}, { // Test 20: A file named .yml that YAML declines is read as INI, as Ansible does.
		Files: inv("hosts.yml", "[web]\nweb01 ansible_connection=local\n"),
		Name:  "hosts.yml", WantFinding: `host "web01" sets "ansible_connection"`,
	}, { // Test 21: A host whose name ssh would read as an option.
		Files: inv("hosts.ini", "-oProxyCommand=id\n"),
		Name:  "hosts.ini", WantFinding: "starts with a dash",
	}, { // Test 22: An address ssh would read as an option.
		Files: inv("hosts.ini", "web01 ansible_host=-oProxyCommand=id\n"),
		Name:  "hosts.ini", WantFinding: "to a value starting with a dash",
	}, { // Test 23: An inventory the tree does not hold.
		Files: inv("site.yml", "- hosts: all\n"),
		Name:  "hosts.ini", WantUnread: "not in the project",
	}, { // Test 24: A name with a comma that is no file is a host list, which sets no variable.
		Files: inv("site.yml", "- hosts: all\n"),
		Name:  "web01,web02",
	}, { // Test 25: A TOML inventory.
		Files: inv("hosts.toml", "[web.hosts.web01]\n"),
		Name:  "hosts.toml", WantUnread: "TOML",
	}, { // Test 26: A name outside the tree.
		Files: inv("hosts.ini", "[web]\nweb01\n"),
		Name:  "../hosts.ini", WantUnread: "outside the project",
	}, { // Test 27: The ProxyCommand inventory the room named.
		Files: inv("hosts.ini", proxy),
		Name:  "hosts.ini", WantFinding: `group "all" sets "ansible_ssh_common_args"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ScanInventoryIn(test.Files, test.Name, "the project")
			checkInventoryScan(t, got, test.WantFinding, test.WantUnread)
		})
	}
}

// TestAStoredInventoryIsScannedAsItsSnapshot pins the stored inventory row of the pass: the
// snapshot the run executes is read as a file with no extension, judged the same way, and a dynamic
// source, whose content is a plugin configuration, is unread.
func TestAStoredInventoryIsScannedAsItsSnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Content is the snapshot.
		Content string
		// WantFinding is a fragment one finding must hold, empty for none.
		WantFinding string
		// WantUnread is a fragment one unread entry must hold, empty for none.
		WantUnread string
	}{{ // Test 0: Bastion arguments on every host.
		Content:     "[all:vars]\nansible_ssh_common_args=-o ProxyCommand=nc\n[web]\nweb01\n",
		WantFinding: `the stored inventory "web": group "all" sets "ansible_ssh_common_args"`,
	}, { // Test 1: A clean snapshot.
		Content: "all:\n  hosts:\n    web01:\n      ansible_host: 10.0.0.1\n",
	}, { // Test 2: A dynamic source.
		Content: "plugin: amazon.aws.aws_ec2\n", WantUnread: "inventory plugin configuration",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			checkInventoryScan(t, ScanStoredInventory(test.Content, "web"), test.WantFinding,
				test.WantUnread)
		})
	}
}

// TestInventoryFindingsCostTheExemption pins that what the inventory pass finds reaches the scan the
// gate classifies, and that the hold note and its fix name the inventory, not the playbook.
func TestInventoryFindingsCostTheExemption(t *testing.T) {
	t.Parallel()
	clean := DryRunScan{Tool: ToolAnsible, Scanner: CheckModeScanner, Version: CheckModeScannerVersion,
		Inputs: []string{"site.yml"}}.Classified()
	if !clean.ChangeFree() {
		t.Fatal("the clean scan is not change free")
	}
	inv := ScanInventoryIn(inv("hosts.ini", "[all:vars]\nansible_connection=local\n"), "hosts.ini",
		"the project")
	got := clean.WithInventory(inv)
	if got.ChangeFree() {
		t.Fatal("an inventory pointing the connection at the controller left the dry run change free")
	}
	if !strings.Contains(strings.Join(got.Inputs, " "), "hosts.ini") {
		t.Errorf("inputs = %q, want the inventory named", got.Inputs)
	}
	if twice := got.WithInventory(inv); len(twice.Findings) != len(got.Findings) {
		t.Errorf("findings = %q, want each entry once", twice.Findings)
	}
	fix := AnsibleFix(got.Entries(), false)
	if !strings.Contains(fix, "remove each ansible_ setting") || strings.Contains(fix, "check mode") {
		t.Errorf("fix = %q, want the connection setting named and no playbook fix", fix)
	}
	unread := clean.WithInventory(ScanStoredInventory("plugin: amazon.aws.aws_ec2\n", "aws"))
	fix = AnsibleFix(unread.Entries(), true)
	if !strings.Contains(fix, "stored inventory that syncs from a source") ||
		strings.Contains(fix, "readable") {
		t.Errorf("fix = %q, want the stored inventory with a source named", fix)
	}
	if e := unread.Entries(); len(e) != 1 || !InventoryEntry(e[0]) || !InventoryEntry(StepPrefix("a")+e[0]) {
		t.Errorf("entries = %q, want one inventory entry, read as one under a step's label too", e)
	}
}

// inv returns a tree holding one file.
func inv(name, content string) fstest.MapFS {
	return fstest.MapFS{name: {Data: []byte(content), Mode: 0o644}}
}

// checkInventoryScan checks that got holds a finding with wantFinding and an unread entry with
// wantUnread, and nothing else, so an empty want means none.
func checkInventoryScan(t *testing.T, got InventoryScan, wantFinding, wantUnread string) {
	t.Helper()
	for _, c := range []struct {
		what string
		list []string
		want string
	}{{"findings", got.Findings, wantFinding}, {"unread", got.Unread, wantUnread}} {
		if c.want == "" {
			if len(c.list) != 0 {
				t.Errorf("%s = %q, want none", c.what, c.list)
			}
			continue
		}
		if !strings.Contains(strings.Join(c.list, "\n"), c.want) {
			t.Errorf("%s = %q, want one holding %q", c.what, c.list, c.want)
		}
	}
}
