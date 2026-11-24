package run

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/util"
)

// The labels every inventory entry starts with, so a refusal or a hold note can tell what an
// inventory caused from what a playbook did.
const (
	inventoryFileLabel   = "the inventory file"
	inventoryDirLabel    = "the inventory directory"
	storedInventoryLabel = "the stored inventory"
)

// inventoryUnreadWords completes the sentence about an inventory the scan could not read.
const inventoryUnreadWords = "so it may choose a program the controller runs"

// InventoryScan is what the gate found reading the inventory an Ansible dry run executes against.
//
// An inventory is read by Ansible on the controller before any task, and what it sets reaches every
// host: an interpreter, a connection plugin, ssh arguments, or a value holding template code runs a
// program on the controller under --check, and an executable inventory file is itself a program
// Ansible runs to list its hosts. The playbook scan never read the inventory, so a pull request could
// carry any of these past a gate that judged the playbook alone.
type InventoryScan struct {
	// Inputs lists the inventory files read, relative to the tree they were read from, and a stored
	// inventory by name.
	Inputs []string
	// Findings lists what in the inventory can run a program on the controller during a dry run.
	Findings []string
	// Unread lists what the inventory holds that the scan could not read, each with why.
	Unread []string
}

// WithInventory returns the scan with what the inventory scan inv found added, an entry already
// present kept once, and classified again. A variable file beside an inventory in a project is read
// by the playbook scan as well, and is reported once.
func (s DryRunScan) WithInventory(inv InventoryScan) DryRunScan {
	s.Inputs = mergeEntries(s.Inputs, inv.Inputs)
	sort.Strings(s.Inputs)
	s.Findings = mergeEntries(s.Findings, inv.Findings)
	s.Unread = mergeEntries(s.Unread, inv.Unread)
	return s.Classified()
}

// mergeEntries appends to list each entry of more it does not already hold.
func mergeEntries(list, more []string) []string {
	seen := make(map[string]bool, len(list))
	for _, e := range list {
		seen[e] = true
	}
	for _, e := range more {
		if !seen[e] {
			seen[e] = true
			list = append(list, e)
		}
	}
	return list
}

// InventoryEntry reports whether a scan entry was recorded by the inventory scan, a step's entry
// included.
func InventoryEntry(entry string) bool {
	entry = strings.TrimPrefix(trimStepPrefix(entry), "could not read ")
	for _, l := range []string{inventoryFileLabel, inventoryDirLabel, storedInventoryLabel} {
		if strings.HasPrefix(entry, l+" ") {
			return true
		}
	}
	return false
}

// ConnectionEntry reports whether a scan entry is for a connection variable, wherever it was set.
func ConnectionEntry(entry string) bool {
	return strings.HasSuffix(entry, connectionVarWhy)
}

// trimStepPrefix removes the step label ScanEntries puts in front of a pipeline step's entry.
func trimStepPrefix(entry string) string {
	rest, ok := strings.CutPrefix(entry, "step ")
	if !ok {
		return entry
	}
	quoted, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return entry
	}
	if after, ok := strings.CutPrefix(rest[len(quoted):], ": "); ok {
		return after
	}
	return entry
}

// ignoredInventoryExts are the extensions Ansible skips by default when it reads an inventory
// directory: its reject list and the inventory ignore list.
var ignoredInventoryExts = []string{".pyc", ".pyo", ".swp", ".bak", "~", ".rpm", ".md", ".txt",
	".rst", ".orig", ".cfg", ".retry"}

// skippedInDir reports whether Ansible skips the entry base when it reads an inventory directory:
// a hidden entry, the variable directories it loads as variables rather than as inventory, a vars
// plugin directory, and a file with an ignored extension.
func skippedInDir(base string, dir bool) bool {
	switch {
	case strings.HasPrefix(base, "."):
		return true
	case base == "group_vars" || base == "host_vars" || base == "vars_plugins":
		return true
	case dir:
		return false
	}
	for _, ext := range ignoredInventoryExts {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// ScanInventoryIn reads the inventory at name inside fsys, a file or a directory, the way the run's
// Ansible loads it, with the group_vars and host_vars beside it. place names fsys in what the scan
// records, such as "the project".
//
// The scan is strict. A symbolic link, an executable file, or a file starting with #! is a finding,
// since Ansible runs an executable inventory as a program. Every variable a group or a host sets is
// judged by its name through ConnectionVar and by its value for template code, so a name the scan
// allows holding a lookup is a finding too. A file is read both as YAML and as INI and the findings
// of every reading that parses are kept, since which reading Ansible takes can depend on its
// configuration, but the reading Ansible takes by the file's extension must parse, or the file is
// unread. A plugin configuration, a vaulted value, or anything else only Ansible can resolve is
// unread, because the scan cannot judge what it does.
//
// A name that is not there and holds a comma is the host list Ansible reads such a name as, which
// sets no variable.
func ScanInventoryIn(fsys fs.FS, name, place string) InventoryScan {
	s := newInventoryWalk(fsys, place)
	s.inventoryPath(name)
	return s.result()
}

// ScanStoredInventory reads the content of a stored inventory's snapshot, the inventory the run
// executes against, named name in what the scan records. The snapshot is handed to Ansible as a
// file with no extension, so it is read the way Ansible reads one.
func ScanStoredInventory(content, name string) InventoryScan {
	s := newInventoryWalk(nil, "")
	what := label(storedInventoryLabel, name)
	s.inputs = append(s.inputs, what)
	s.inventoryContent(what, content, true)
	return s.result()
}

// HostListScan is what the scan records for a name holding a comma that is no file, the host list
// Ansible reads it as. A host list sets no variable.
func HostListScan(name string) InventoryScan {
	s := newInventoryWalk(nil, "")
	s.hostList(label(inventoryFileLabel, name), name)
	return s.result()
}

// UnreadStoredInventory is what the scan records for a stored inventory whose snapshot could not be
// read, saying why. Nothing shows such an inventory sets nothing, so the dry run is incomplete.
func UnreadStoredInventory(name, why string) InventoryScan {
	return InventoryScan{Unread: []string{label(storedInventoryLabel, name) + " (" + why + ")"}}
}

// UnreadInventoryFile is what the scan records for an inventory named by a path that could not be
// read at all, saying why.
func UnreadInventoryFile(name, why string) InventoryScan {
	return InventoryScan{Unread: []string{label(inventoryFileLabel, name) + " (" + why + ")"}}
}

// inventoryWalk is one read of an inventory. It reads through a playbookScan, so the bounds on what
// one scan reads, the variable file rules, and the dedupe of entries are the playbook scan's own.
type inventoryWalk struct {
	*playbookScan
	// inputs lists a stored inventory, which is not a file the walk reads.
	inputs []string
}

// newInventoryWalk returns a walk over fsys.
func newInventoryWalk(fsys fs.FS, place string) *inventoryWalk {
	return &inventoryWalk{playbookScan: &playbookScan{fsys: fsys, place: place,
		out: &PlaybookSignals{}, files: map[string]bool{}, visited: map[string]bool{},
		roles: map[string]bool{}, listed: map[string]bool{}}}
}

// result returns what the walk found.
func (s *inventoryWalk) result() InventoryScan {
	out := InventoryScan{Inputs: s.inputs, Findings: s.out.Forced, Unread: s.out.Unread}
	for file := range s.files {
		out.Inputs = append(out.Inputs, file)
	}
	sort.Strings(out.Inputs)
	return out
}

// find records one finding about the inventory, deduped like every other.
func (s *inventoryWalk) find(finding string) {
	if s.listed[finding] {
		return
	}
	s.listed[finding] = true
	s.out.Forced = append(s.out.Forced, finding)
}

// inventoryPath reads the inventory the run names by path.
func (s *inventoryWalk) inventoryPath(name string) {
	name = path.Clean(strings.TrimPrefix(name, "./"))
	what := label(inventoryFileLabel, name)
	if !fs.ValidPath(name) {
		s.skip(what, "outside "+s.place)
		return
	}
	mode, err := entryMode(s.fsys, name)
	switch {
	case errors.Is(err, fs.ErrNotExist) && strings.Contains(name, ","):
		s.hostList(what, name)
		return
	case err != nil:
		s.skip(what, s.why(err))
		return
	case mode&fs.ModeSymlink != 0:
		s.find(what + " is a symbolic link, and the gate judges only an inventory it reads in place")
		return
	case mode.IsDir():
		s.inventoryDir(name)
		return
	}
	s.inventoryFile(name, mode)
	s.adjacentVars(path.Dir(name))
}

// hostList judges the host list Ansible reads a name holding a comma as: hosts only, with no
// variable. A host whose name starts with a dash is handed to ssh where it reads as an option.
func (s *inventoryWalk) hostList(what, name string) {
	for _, h := range strings.Split(name, ",") {
		s.hostName(what, strings.TrimSpace(path.Base(h)))
	}
}

// hostName records a finding for a host named so that ssh reads it as an option.
func (s *inventoryWalk) hostName(what, host string) {
	if strings.HasPrefix(host, "-") {
		s.find(what + ": host " + quoted(host) + " starts with a dash, which ssh reads as an " +
			"option, such as one naming a program to run, rather than as a host")
	}
}

// adjacentVars judges the group_vars and host_vars directories beside an inventory, which Ansible
// loads with it.
func (s *inventoryWalk) adjacentVars(dir string) {
	for _, d := range []string{"group_vars", "host_vars"} {
		s.judgeVarDir(path.Join(dir, d))
	}
}

// inventoryDir reads every file Ansible loads from an inventory directory, and the variables beside
// each directory it descends into.
func (s *inventoryWalk) inventoryDir(dir string) {
	entries := 0
	_ = fs.WalkDir(s.fsys, dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			s.skip(label(inventoryDirLabel, name), "could not be listed")
			return nil
		}
		if entries++; entries > maxScanEntries {
			s.skip(label(inventoryDirLabel, dir), fmt.Sprintf("past the %d entries one scan lists",
				maxScanEntries))
			return fs.SkipAll
		}
		if d.IsDir() {
			if name != dir && skippedInDir(path.Base(name), true) {
				return fs.SkipDir
			}
			s.adjacentVars(name)
			return nil
		}
		if skippedInDir(path.Base(name), false) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			s.find(label(inventoryFileLabel, name) + " is a symbolic link, and the gate judges " +
				"only an inventory it reads in place")
			return nil
		}
		info, err := d.Info()
		if err != nil {
			s.skip(label(inventoryFileLabel, name), "could not be read")
			return nil
		}
		s.inventoryFile(name, info.Mode())
		return nil
	})
}

// inventoryFile reads one inventory file, whose mode is mode.
func (s *inventoryWalk) inventoryFile(name string, mode fs.FileMode) {
	what := label(inventoryFileLabel, name)
	if mode&0o111 != 0 {
		s.find(what + " is executable, so Ansible runs it as a program on the controller to list " +
			"its hosts")
		return
	}
	if strings.EqualFold(path.Ext(name), ".toml") {
		s.skip(what, "a TOML inventory, which the gate does not read")
		return
	}
	content, err := s.read(name)
	if err != nil {
		s.skip(what, s.why(err))
		return
	}
	if bytes.HasPrefix(content, []byte("#!")) {
		s.find(what + " starts with #!, the first line of a program, which Ansible runs on the " +
			"controller once the file is executable")
	}
	s.inventoryContent(what, string(content), inventory.YAMLExtension(name))
}

// inventoryContent judges one inventory document, what, read both as YAML and as INI. yamlExt says
// whether Ansible's YAML plugin takes the file, so whether the reading Ansible takes is the YAML
// then INI one or the INI one alone. That reading must parse, or the document is unread.
func (s *inventoryWalk) inventoryContent(what, content string, yamlExt bool) {
	native, nerr := inventory.ReadAsAnsible(content)
	ini, ierr := inventory.ReadAsINI(content)
	taken := nerr
	if !yamlExt {
		taken = ierr
	}
	if taken != nil {
		s.skip(what, inventoryUnreadWhy(taken))
		return
	}
	for _, rd := range []*inventory.Reading{native, ini} {
		if rd != nil {
			s.judgeReading(what, rd)
		}
	}
}

// inventoryUnreadWhy says why the scan could not judge an inventory document.
func inventoryUnreadWhy(err error) string {
	if reason, ok := inventory.NeedsAnsibleReason(err); ok {
		return reason
	}
	if errors.Is(err, inventory.ErrTooManyHosts) {
		return "it names more hosts than the gate reads"
	}
	return "it does not parse as an inventory Ansible reads"
}

// hostAddressVars are the variables that set the address ssh is handed for a host.
var hostAddressVars = map[string]bool{"ansible_host": true, "ansible_ssh_host": true}

// judgeReading judges every host and every variable one reading of an inventory found.
func (s *inventoryWalk) judgeReading(what string, rd *inventory.Reading) {
	for _, h := range rd.Hosts {
		s.hostName(what, h)
	}
	for _, st := range rd.Settings {
		owner := "group " + quoted(st.Group)
		if st.Host != "" {
			owner = "host " + quoted(st.Host)
		}
		name := quoted(st.Name)
		if ConnectionVar(st.Name) {
			s.find(what + ": " + owner + " sets " + name + ", " + connectionVarWhy)
		}
		if inventory.HasTemplateMarker(st.Name) || HoldsTemplate(st.Value) {
			s.find(what + ": " + owner + " sets " + name + " to template code, which Ansible " +
				"renders on the controller during a dry run")
		}
		if v, ok := st.Value.(string); ok && hostAddressVars[strings.ToLower(st.Name)] &&
			strings.HasPrefix(strings.TrimSpace(v), "-") {
			s.find(what + ": " + owner + " sets " + name + " to a value starting with a dash, " +
				"which ssh reads as an option rather than as a host")
		}
	}
}

// quoted renders a name read from an inventory as an entry repeats it.
func quoted(name string) string {
	return strconv.Quote(util.Clip(util.SafeText(name), maxScanLabel))
}

// entryMode returns the mode of the entry at name in fsys without following a symbolic link at
// name itself, read from the directory listing that holds it, so a link is reported as a link.
func entryMode(fsys fs.FS, name string) (fs.FileMode, error) {
	if name == "." {
		info, err := fs.Stat(fsys, name)
		if err != nil {
			return 0, err
		}
		return info.Mode(), nil
	}
	entries, err := fs.ReadDir(fsys, path.Dir(name))
	if err != nil {
		return 0, err
	}
	base := path.Base(name)
	for _, e := range entries {
		if e.Name() != base {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			return fs.ModeSymlink, nil
		}
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		return info.Mode(), nil
	}
	return 0, fs.ErrNotExist
}

// AnsibleFix says what to change so the gate finds an Ansible dry run change free, worded for what
// its scan entries name: a playbook, a connection variable, or an inventory. incomplete says the
// scan found nothing and could not read everything. It is a clause, lower case, with no ending.
//
// The gate reads strictly, so a connection or interpreter setting has no fix but its removal, and
// an inventory that is a program, a plugin configuration, or a vaulted value has no fix but a stored
// inventory that syncs from a source, whose hosts are stored as data.
func AnsibleFix(entries []string, incomplete bool) string {
	var playbook, connection, inv bool
	for _, e := range entries {
		switch {
		case ConnectionEntry(e):
			connection = true
		case InventoryEntry(e):
			inv = true
		default:
			playbook = true
		}
	}
	var parts []string
	if incomplete {
		if playbook || connection || !inv {
			parts = append(parts, "make what the playbook pulls in readable to the gate, such as a "+
				"role committed to the project or installed from its requirements file")
		}
		if inv {
			parts = append(parts, "plan against a stored inventory that syncs from a source, whose "+
				"hosts the gate reads as data, since an inventory plugin configuration, a script, or a "+
				"vaulted value is something only Ansible can resolve")
		}
		return strings.Join(parts, " and ")
	}
	if playbook || (!connection && !inv) {
		parts = append(parts, "rework the task so check mode is safe")
	}
	if connection {
		parts = append(parts, "remove each ansible_ setting it names, since the gate passes only "+
			"the user, host, port, become, and password settings")
	}
	if inv {
		parts = append(parts, "take the script or template code out of the inventory, moving a "+
			"script inventory to a stored inventory that syncs from a source")
	}
	return strings.Join(parts, " and ")
}
