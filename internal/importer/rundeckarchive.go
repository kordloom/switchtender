package importer

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/project"
)

// Rundeck project archives are read under fixed ceilings. The import endpoint is reachable by
// anyone allowed to import and a zip is a hostile input: a few kilobytes can expand into gigabytes.
// A real project archive also carries every execution log, state file, and report alongside the
// definitions this reads, so an archive of a busy project is far larger than what comes out of it.
const (
	// maxRundeckZipEntries caps how many archive members are examined.
	maxRundeckZipEntries = 20000
	// maxRundeckEntrySize caps one member read out of the archive. Every member this reads is a job
	// definition, a properties file, or a manifest, each of which is a document rather than a payload.
	maxRundeckEntrySize = 4 << 20
	// maxRundeckTotalSize caps everything read out of one archive in total.
	maxRundeckTotalSize = 64 << 20
)

// rundeckProjectBasedir is the token Rundeck substitutes into every path-valued project setting in
// place of the project's own directory on the server that exported it. It is reported as it stands
// rather than expanded, because the directory it names is on a machine this cannot see.
const rundeckProjectBasedir = "%PROJECT_BASEDIR%"

// Manifest entries a Rundeck project archive carries about itself.
const (
	// rundeckManifestPath is where the archive keeps its manifest, in JAR layout.
	rundeckManifestPath = "META-INF/MANIFEST.MF"
	// rundeckManifestProject names the project the archive was exported from.
	rundeckManifestProject = "Rundeck-Archive-Project-Name"
	// rundeckManifestFormat is the archive format version, which is 1.0 for every Rundeck 5 export.
	rundeckManifestFormat = "Rundeck-Archive-Format-Version"
	// rundeckArchiveFormat is the archive format version this importer was written against.
	rundeckArchiveFormat = "1.0"
)

// IsRundeckArchive reports whether a body is a Rundeck project archive rather than a job export.
//
// A project archive is a zip in JAR layout and a job export is a YAML or JSON document, so the two
// are told apart by their content. Somebody leaving Rundeck reaches for the project archive, which
// is what the web interface and the API offer for a whole project, so it is recognized rather than
// selected with a flag an operator would have to know to set and could set wrongly.
func IsRundeckArchive(data []byte) bool { return bytes.HasPrefix(data, zipMagic) }

// rundeckArchive is what one project archive carries that this importer reads.
type rundeckArchive struct {
	// Project is the project's name, from the manifest, the project configuration, or the entry
	// prefix, whichever named it first.
	Project string
	// FormatVersion is the archive format version the manifest declared, empty when it declared none.
	FormatVersion string
	// Jobs are the job definition documents, ordered by their path in the archive so an import of
	// the same archive twice reports the same jobs in the same order.
	Jobs []rundeckJobFile
	// Unread names members under the jobs directory that are not XML documents, so a job this
	// cannot read is reported rather than mistaken for one that was never there.
	Unread []string
	// Config is the project configuration from files/etc/project.properties.
	Config map[string]string
	// HasConfig reports whether the project configuration was found at all, which a genuinely empty
	// configuration cannot be told apart from otherwise.
	HasConfig bool
	// SCM holds each SCM plugin configuration the archive carried, export before import.
	SCM []rundeckSCMConfig
}

// rundeckJobFile is one job definition document and the archive path it came from.
type rundeckJobFile struct {
	// Path is the member's cleaned path in the archive, used to order the jobs and to name a
	// document that will not parse.
	Path string
	// Data is the raw job XML.
	Data []byte
}

// rundeckSCMConfig is one SCM plugin configuration read out of an archive.
type rundeckSCMConfig struct {
	// Mode is export or import, the direction the plugin was configured for.
	Mode string
	// Config holds the plugin's settings with the scm.<mode>.config. prefix removed.
	Config map[string]string
}

// rundeckNodeSource is one numbered node source entry of a project's configuration.
type rundeckNodeSource struct {
	// Index is the source's number in the project configuration, counting from one.
	Index int
	// Type names the source plugin, such as file, url, or directory.
	Type string
	// Config holds the source's settings with the resources.source.N.config. prefix removed.
	Config map[string]string
}

// rundeckEntryKind names what an archive member is, so each is classified once and only the members
// this reads are ever opened.
type rundeckEntryKind int

const (
	// rundeckEntryIgnored is a member this importer does not read, such as an execution log.
	rundeckEntryIgnored rundeckEntryKind = iota
	// rundeckEntryJob is a job definition document under the project's jobs directory.
	rundeckEntryJob
	// rundeckEntryUnreadJob is a member under the jobs directory that is not an XML document.
	rundeckEntryUnreadJob
	// rundeckEntryConfig is the project configuration, files/etc/project.properties.
	rundeckEntryConfig
	// rundeckEntrySCM is an SCM plugin configuration, files/etc/scm-export.properties or its import
	// counterpart.
	rundeckEntrySCM
	// rundeckEntryManifest is the archive manifest.
	rundeckEntryManifest
)

// fromRundeckArchive maps a Rundeck project archive into a plan of equivalent objects.
//
// The archive's jobs run through the same mapping as a job export's, so the two artifacts produce
// the same templates and schedules for the same job. Beyond the jobs, an archive carries two things
// a job export does not: the SCM configuration, which becomes a project where it names a repository
// this can reach, and the node source configuration, which is reported and never turned into an
// inventory. The archive holds no node definitions at any path, so there is nothing to build one
// from and guessing at the list of machines a run reaches is the one thing an import must not do.
func fromRundeckArchive(data []byte, inventoryName string, now time.Time) (*Plan, error) {
	arc, err := readRundeckArchive(data)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	plan.warnRundeckInventory(inventoryName)
	if arc.FormatVersion != "" && arc.FormatVersion != rundeckArchiveFormat {
		plan.warn("the archive declares format version %s and this reads version %s, so check the "+
			"plan against the project before applying it",
			oneLine(arc.FormatVersion), rundeckArchiveFormat)
	}
	plan.reportRundeckNodeSources(arc)
	for _, name := range arc.Unread {
		plan.warn("the archive holds %s under the project's jobs directory, which is not a job "+
			"definition this reads, so nothing was imported from it", oneLine(name))
	}
	for _, file := range arc.Jobs {
		jobs, err := decodeRundeckXML(file.Data)
		if err != nil {
			plan.warn("the job definition %s was not read: %v", oneLine(file.Path), err)
			continue
		}
		if len(jobs) == 0 {
			plan.warn("the job definition %s holds no job", oneLine(file.Path))
			continue
		}
		for _, job := range jobs {
			plan.addRundeckJob(job, inventoryName, now)
		}
	}
	plan.addRundeckArchiveProject(arc, now)
	if err := plan.requireObjects("jobs"); err != nil {
		return nil, err
	}
	return plan, nil
}

// readRundeckArchive reads the members of a project archive this importer needs, refusing an
// archive whose members are hostile before any of them is opened.
func readRundeckArchive(data []byte) (*rundeckArchive, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read rundeck archive: %w", err)
	}
	if len(r.File) > maxRundeckZipEntries {
		return nil, fmt.Errorf("read rundeck archive: it holds %d entries, more than the %d this "+
			"reads. Export one project at a time rather than a whole Rundeck",
			len(r.File), maxRundeckZipEntries)
	}
	arc := &rundeckArchive{}
	var manifest map[string]string
	prefix := ""
	total := 0
	for _, f := range r.File {
		if err := checkRundeckEntry(f); err != nil {
			return nil, err
		}
		name := path.Clean(strings.ReplaceAll(f.Name, `\`, "/"))
		if f.FileInfo().IsDir() {
			continue
		}
		kind := classifyRundeckEntry(name)
		if kind == rundeckEntryIgnored {
			continue
		}
		if kind == rundeckEntryUnreadJob {
			arc.Unread = append(arc.Unread, name)
			continue
		}
		if f.UncompressedSize64 > maxRundeckEntrySize {
			return nil, fmt.Errorf("read rundeck archive: %s is larger than a definition should be",
				name)
		}
		body, err := readZipEntry(f, "rundeck", maxRundeckEntrySize)
		if err != nil {
			return nil, err
		}
		total += len(body)
		if total > maxRundeckTotalSize {
			return nil, fmt.Errorf("read rundeck archive: the definitions in it exceed the %d MiB "+
				"this reads", maxRundeckTotalSize>>20)
		}
		switch kind {
		case rundeckEntryJob:
			arc.Jobs = append(arc.Jobs, rundeckJobFile{Path: name, Data: body})
			if prefix == "" {
				prefix = strings.Split(name, "/")[0]
			}
		case rundeckEntryConfig:
			arc.Config = parseJavaProperties(body)
			arc.HasConfig = true
		case rundeckEntrySCM:
			arc.SCM = append(arc.SCM, rundeckSCMFromProperties(name, body))
		case rundeckEntryManifest:
			manifest = parseManifest(body)
		case rundeckEntryIgnored, rundeckEntryUnreadJob:
		}
	}
	if len(arc.Jobs) == 0 && !arc.HasConfig {
		return nil, fmt.Errorf("read rundeck archive: it holds no job definitions and no project " +
			"configuration, so it is not a Rundeck project archive. Export the project from " +
			"Rundeck, or point this at a job export file instead")
	}
	sort.Slice(arc.Jobs, func(i, j int) bool { return arc.Jobs[i].Path < arc.Jobs[j].Path })
	sort.Slice(arc.SCM, func(i, j int) bool { return arc.SCM[i].Mode < arc.SCM[j].Mode })
	arc.FormatVersion = manifest[rundeckManifestFormat]
	arc.Project = rundeckArchiveProjectName(manifest, arc.Config, prefix)
	return arc, nil
}

// checkRundeckEntry refuses an archive member that no export writes and every crafted zip does.
//
// Nothing here is written to disk, so a traversal cannot overwrite a file today. That is a property
// of the current reader rather than of the archive, and the next person to add an extraction step
// would inherit an input nobody had checked. An absolute path, a parent segment, and a symlink each
// mean the archive was built by hand to escape wherever it lands, so the whole archive is refused
// rather than quietly filtered: a hand-built archive is not a partial import, it is a hostile one.
func checkRundeckEntry(f *zip.File) error {
	name := strings.ReplaceAll(f.Name, `\`, "/")
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("read rundeck archive: the member %q has an absolute path, which no "+
			"Rundeck export writes", oneLine(f.Name))
	}
	if len(name) > 1 && name[1] == ':' {
		return fmt.Errorf("read rundeck archive: the member %q names a drive, which no Rundeck "+
			"export writes", oneLine(f.Name))
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return fmt.Errorf("read rundeck archive: the member %q climbs out of the archive, "+
				"which no Rundeck export writes", oneLine(f.Name))
		}
	}
	if mode := f.Mode(); mode&fs.ModeSymlink != 0 {
		return fmt.Errorf("read rundeck archive: the member %q is a symbolic link, which no "+
			"Rundeck export writes", oneLine(f.Name))
	}
	return nil
}

// classifyRundeckEntry classifies one archive member by where it sits.
//
// A project archive nests everything under a single rundeck-<project>/ prefix, but the prefix is
// matched by position rather than by spelling, so an archive rooted at a different directory still
// reads. Jobs are recognized by sitting directly under a jobs directory, which is what keeps the
// executions, reports, and jobfiles directories, whose members are also XML, out of the job list.
func classifyRundeckEntry(name string) rundeckEntryKind {
	if name == rundeckManifestPath {
		return rundeckEntryManifest
	}
	dir, base := path.Split(name)
	dir = strings.TrimSuffix(dir, "/")
	switch path.Base(dir) {
	case "jobs":
		if strings.EqualFold(path.Ext(base), ".xml") {
			return rundeckEntryJob
		}
		return rundeckEntryUnreadJob
	case "etc":
		switch base {
		case "project.properties":
			return rundeckEntryConfig
		case "scm-export.properties", "scm-import.properties":
			return rundeckEntrySCM
		}
	}
	return rundeckEntryIgnored
}

// rundeckSCMFromProperties reads one SCM plugin configuration, taking the direction it was
// configured for from the file's own name and stripping the scm.<mode>.config. prefix from the
// settings so a caller reads url and branch rather than the full key.
func rundeckSCMFromProperties(name string, body []byte) rundeckSCMConfig {
	mode := "export"
	if path.Base(name) == "scm-import.properties" {
		mode = "import"
	}
	scm := rundeckSCMConfig{Mode: mode, Config: map[string]string{}}
	prefix := "scm." + mode + ".config."
	for key, value := range parseJavaProperties(body) {
		if field, ok := strings.CutPrefix(key, prefix); ok {
			scm.Config[field] = value
		}
	}
	return scm
}

// rundeckArchiveProjectName settles what the project is called, preferring the manifest, then the
// project configuration, then the directory the archive nests everything under.
func rundeckArchiveProjectName(manifest, config map[string]string, prefix string) string {
	if name := strings.TrimSpace(manifest[rundeckManifestProject]); name != "" {
		return name
	}
	if name := strings.TrimSpace(config["project.name"]); name != "" {
		return name
	}
	return strings.TrimPrefix(prefix, "rundeck-")
}

// warnRundeckInventory reports an import that named no inventory, so a template created without one
// is a decision the operator sees rather than a default they discover at launch. Both the job export
// and the project archive go through it, since neither artifact names a host.
func (p *Plan) warnRundeckInventory(inventoryName string) {
	if inventoryName != "" {
		return
	}
	p.warn("no inventory was named, so every imported template launches without one. " +
		"Re-run with --inventory to say which hosts these jobs target, or set it on each " +
		"template afterward.")
}

// reportRundeckNodeSources names where the project got its nodes, which is all a project archive can
// honestly say about them.
//
// The archive carries the node source configuration and not one node definition, at any path. A file
// source names a path on the Rundeck server and a url source names an endpoint, and neither travels
// in the export. Rebuilding hosts from the execution history would be worse: it holds bare node
// names, only for the nodes that happened to run, with no address, login, or connection attributes.
// So no inventory is created here, and each source is named so the operator can attach the right one.
func (p *Plan) reportRundeckNodeSources(arc *rundeckArchive) {
	if !arc.HasConfig {
		p.warn("the archive holds no project configuration, so it says nothing about where this " +
			"project's nodes came from. No inventory was imported: an archive carries no node " +
			"definitions, so attach an inventory of your own to the templates this created.")
		return
	}
	sources := rundeckNodeSources(arc.Config)
	if len(sources) == 0 {
		p.warn("the project configuration names no node source, so the project ran against " +
			"whatever the Rundeck server itself provided. No inventory was imported: an archive " +
			"carries no node definitions, so attach an inventory of your own to the templates " +
			"this created.")
		return
	}
	for _, s := range sources {
		p.warn("this project's nodes came from source %d, %s. No inventory was imported from it: "+
			"a project archive carries the source's configuration and not one node definition, so "+
			"attach an inventory of your own to the templates this created.",
			s.Index, rundeckSourceLocation(s))
	}
}

// rundeckNodeSources reads the numbered node source entries out of a project configuration, in
// number order so the report matches the order Rundeck consulted them in.
func rundeckNodeSources(config map[string]string) []rundeckNodeSource {
	byIndex := map[int]*rundeckNodeSource{}
	for key, value := range config {
		rest, ok := strings.CutPrefix(key, "resources.source.")
		if !ok {
			continue
		}
		number, field, ok := strings.Cut(rest, ".")
		if !ok {
			continue
		}
		index, err := strconv.Atoi(number)
		if err != nil {
			continue
		}
		src := byIndex[index]
		if src == nil {
			src = &rundeckNodeSource{Index: index, Config: map[string]string{}}
			byIndex[index] = src
		}
		if field == "type" {
			src.Type = value
			continue
		}
		if name, ok := strings.CutPrefix(field, "config."); ok {
			src.Config[name] = value
		}
	}
	out := make([]rundeckNodeSource, 0, len(byIndex))
	for _, s := range byIndex {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// rundeckSourceLocation describes one node source for the report, naming the path or endpoint it
// pointed at so the operator knows what to attach an inventory from.
func rundeckSourceLocation(s rundeckNodeSource) string {
	kind := strings.ToLower(strings.TrimSpace(s.Type))
	switch kind {
	case "file", "script":
		if p := strings.TrimSpace(s.Config["file"]); p != "" {
			return "a file on the Rundeck server at " + oneLine(p) + rundeckBasedirNote(p)
		}
	case "directory":
		if p := strings.TrimSpace(s.Config["directory"]); p != "" {
			return "a directory on the Rundeck server at " + oneLine(p) + rundeckBasedirNote(p)
		}
	case "url":
		if u := strings.TrimSpace(s.Config["url"]); u != "" {
			return "the endpoint " + oneLine(u)
		}
	}
	if kind == "" {
		return "a source whose type the project configuration did not name"
	}
	return "a " + strconv.Quote(oneLine(kind)) +
		" source, whose location the project configuration did not name in a form this reads"
}

// rundeckBasedirNote explains Rundeck's project directory token when a path carries it, since the
// path is otherwise unreadable to anyone who has not seen a Rundeck project configuration. The token
// is left as it stands rather than expanded, because the directory it names is on a machine this
// cannot see and writing a guess at it would be a path the operator might act on.
func rundeckBasedirNote(p string) string {
	if !strings.Contains(p, rundeckProjectBasedir) {
		return ""
	}
	return ", where " + rundeckProjectBasedir + " is that project's own directory on that server"
}

// addRundeckArchiveProject creates a project from the archive's SCM configuration, but only where
// that configuration names a repository this can actually reach.
//
// A Rundeck project is a job store rather than a git repository, so there is nothing to make a
// project out of unless the SCM plugin was configured to mirror the jobs into one. Where it was, the
// repository URL is real and the project is created. A file URL is refused: it names a directory on
// the Rundeck server, which is a different machine, so a project built from it either fails on its
// first sync or, worse, finds an unrelated directory of that name on this host and clones that.
// Naming a machine resource this cannot see is the same mistake as inventing an inventory, so the
// path is reported and the operator decides what to do with it.
func (p *Plan) addRundeckArchiveProject(arc *rundeckArchive, now time.Time) {
	if len(arc.SCM) == 0 {
		p.warn("the archive carries no SCM configuration, so no project was created. A Rundeck " +
			"project is a job store rather than a git repository, so a project is created only " +
			"where the archive names a repository to clone.")
		return
	}
	created := false
	for _, scm := range arc.SCM {
		url := strings.TrimSpace(scm.Config["url"])
		switch {
		case url == "":
			p.warn("the archive's SCM %s configuration names no repository, so no project was "+
				"created from it", scm.Mode)
		case strings.Contains(url, rundeckProjectBasedir):
			p.warn("the archive's SCM %s configuration points at %s, which is a directory on the "+
				"Rundeck server rather than a repository this can reach, so no project was created",
				scm.Mode, oneLine(url))
		case strings.HasPrefix(strings.ToLower(url), "file:"):
			p.warn("the archive's SCM %s configuration points at %s, which is a path on the "+
				"Rundeck server rather than a repository this can reach, so no project was "+
				"created. Push that repository somewhere this can clone from, then create the "+
				"project by hand.", scm.Mode, oneLine(url))
		case created:
			p.warn("the archive's SCM %s configuration points at a second repository, %s, which "+
				"was not imported: a project archive is one project and it already has one",
				scm.Mode, oneLine(url))
		default:
			created = p.addRundeckSCMProject(arc, scm, url, now)
		}
	}
}

// addRundeckSCMProject creates the project one SCM configuration names, reporting whether it did.
// The repository is held to the same check the API applies when a person creates a project by hand,
// so an archive cannot create a project the API itself would have refused.
func (p *Plan) addRundeckSCMProject(arc *rundeckArchive, scm rundeckSCMConfig, url string,
	now time.Time) bool {
	if err := project.ValidateRepoURL(url); err != nil {
		p.warn("the archive's SCM %s configuration points at %s, which was not imported as a "+
			"project: %v", scm.Mode, oneLine(url), err)
		return false
	}
	name := arc.Project
	if name == "" {
		name = "rundeck-import"
	}
	p.Projects = append(p.Projects, &project.Project{
		ID: project.NewID(), Name: name, RepoURL: url,
		Branch: strings.TrimSpace(scm.Config["branch"]), InstallDeps: true, CreatedAt: now,
	})
	p.warn("project %q was created from the archive's SCM %s configuration, pointing at %s. That "+
		"repository holds the job definitions Rundeck mirrored into it rather than Ansible "+
		"playbooks, so point your templates at your own playbook repository if that is a "+
		"different one. Only the repository and its branch came across: the export's path "+
		"template, format, and committer identity describe how Rundeck wrote into it and have no "+
		"equivalent here.", name, scm.Mode, oneLine(url))
	return true
}

// decodeRundeckXML reads one job definition document out of a project archive and converts each job
// it holds into the shape the job export path reads.
func decodeRundeckXML(data []byte) ([]rundeckJob, error) {
	var doc rundeckXMLJobList
	if err := xml.Unmarshal(bytes.TrimLeft(data, xmlLeadingBytes), &doc); err != nil {
		return nil, err
	}
	jobs := make([]rundeckJob, 0, len(doc.Jobs))
	for _, x := range doc.Jobs {
		jobs = append(jobs, rundeckJobFromXML(x))
	}
	return jobs, nil
}

// rundeckXMLJobList is a Rundeck job definition document, which holds one or more jobs.
type rundeckXMLJobList struct {
	// XMLName pins the root element, so a document that is not a job list is refused by name rather
	// than decoded into an empty list and reported as a project holding no jobs.
	XMLName xml.Name `xml:"joblist"`
	// Jobs are the job definitions in the document.
	Jobs []rundeckXMLJob `xml:"job"`
}

// rundeckXMLJob is one job as a project archive writes it.
//
// The archive's XML and a job export's YAML carry the same job in different shapes: options sit
// under a context element with every setting an attribute, dispatch sits beside the node filter
// rather than inside it, and a schedule is a set of attributed elements rather than nested keys. So
// the document is decoded into its own shape here and converted, rather than teaching one struct two
// spellings, and the conversion feeds the single mapping both artifacts share.
type rundeckXMLJob struct {
	// Name is the job name.
	Name string `xml:"name"`
	// Group is the job's folder path within its project.
	Group string `xml:"group"`
	// Description is the job description.
	Description string `xml:"description"`
	// Timeout caps a job execution, written as a duration such as 30m or as plain seconds.
	Timeout string `xml:"timeout"`
	// ScheduleEnabled reports whether the job's schedule is active, absent when Rundeck omitted it.
	ScheduleEnabled *string `xml:"scheduleEnabled"`
	// ExecutionEnabled reports whether the job may run at all, absent when Rundeck omitted it.
	ExecutionEnabled *string `xml:"executionEnabled"`
	// Context holds the job's project and its prompted options.
	Context rundeckXMLContext `xml:"context"`
	// Dispatch paces the fan out across nodes, which the archive keeps beside the node filter.
	Dispatch rundeckXMLDispatch `xml:"dispatch"`
	// NodeFilters selects which nodes the job dispatches to.
	NodeFilters rundeckXMLNodeFilters `xml:"nodefilters"`
	// Sequence holds the ordered steps the job runs.
	Sequence rundeckXMLSequence `xml:"sequence"`
	// Schedule is the job's cadence, absent when the job is not scheduled.
	Schedule *rundeckXMLSchedule `xml:"schedule"`
}

// rundeckXMLContext holds a job's project and the options it prompts for.
type rundeckXMLContext struct {
	// Project is the Rundeck project the job belongs to.
	Project string `xml:"project"`
	// Options are the job's prompted inputs.
	Options []rundeckXMLOption `xml:"options>option"`
}

// rundeckXMLOption is one prompted input, whose every setting the archive writes as an attribute.
type rundeckXMLOption struct {
	// Name is the option name, which becomes the survey field's variable.
	Name string `xml:"name,attr"`
	// Value is the default value.
	Value string `xml:"value,attr"`
	// Values is the allowed set as one delimited string.
	Values string `xml:"values,attr"`
	// ValuesDelimiter separates the allowed set, a comma when the archive names none.
	ValuesDelimiter string `xml:"valuesListDelimiter,attr"`
	// Enforced restricts the answer to Values rather than merely suggesting them.
	Enforced string `xml:"enforcedvalues,attr"`
	// Required rejects a launch that leaves the option empty.
	Required string `xml:"required,attr"`
	// Secure marks the option as a password, whose value Rundeck stores obscured.
	Secure string `xml:"secure,attr"`
	// Multivalued lets the option carry several values at once.
	Multivalued string `xml:"multivalued,attr"`
	// Description is shown to the person launching the job.
	Description string `xml:"description"`
}

// rundeckXMLDispatch paces a job's fan out across nodes.
type rundeckXMLDispatch struct {
	// ThreadCount is how many nodes run at once, the equivalent of Ansible forks.
	ThreadCount string `xml:"threadcount"`
	// KeepGoing continues across nodes after one fails.
	KeepGoing string `xml:"keepgoing"`
}

// rundeckXMLNodeFilters selects which nodes a job dispatches to.
type rundeckXMLNodeFilters struct {
	// Filter is the node filter expression, which selects hosts by attribute rather than by name.
	Filter string `xml:"filter"`
}

// rundeckXMLSequence is a job's ordered step list.
type rundeckXMLSequence struct {
	// KeepGoing continues the sequence after a step fails.
	KeepGoing string `xml:"keepgoing,attr"`
	// Commands are the steps in order.
	Commands []rundeckXMLCommand `xml:"command"`
}

// rundeckXMLCommand is one step of a job sequence.
type rundeckXMLCommand struct {
	// Description labels the step.
	Description string `xml:"description"`
	// Exec is a single shell command to run.
	Exec string `xml:"exec"`
	// Script is an inline script body, which the archive writes as character data.
	Script string `xml:"script"`
	// ScriptInterpreter names the program an inline script is fed to.
	ScriptInterpreter string `xml:"scriptinterpreter"`
	// ScriptFile names a script on the node rather than inline content.
	ScriptFile string `xml:"scriptfile"`
	// ScriptURL names a script fetched from a URL.
	ScriptURL string `xml:"scripturl"`
	// JobRef calls another job, which has no direct single-template equivalent.
	JobRef *rundeckXMLJobRef `xml:"jobref"`
	// StepPlugin is a workflow step plugin, which does not map.
	StepPlugin *rundeckXMLPlugin `xml:"step-plugin"`
	// NodeStepPlugin is a node step plugin, which does not map.
	NodeStepPlugin *rundeckXMLPlugin `xml:"node-step-plugin"`
}

// rundeckXMLJobRef is a reference from one job to another.
type rundeckXMLJobRef struct {
	// Name is the referenced job's name.
	Name string `xml:"name,attr"`
	// Group is the referenced job's folder path.
	Group string `xml:"group,attr"`
}

// rundeckXMLPlugin is a plugin step, read only for the type named in the report.
type rundeckXMLPlugin struct {
	// Type is the plugin's provider name.
	Type string `xml:"type,attr"`
}

// rundeckXMLSchedule is a job's cadence, which the archive writes as attributed elements.
type rundeckXMLSchedule struct {
	// Crontab is a Quartz expression, when the job was scheduled with one.
	Crontab string `xml:"crontab,attr"`
	// Time holds the hour, minute, and seconds.
	Time rundeckXMLTime `xml:"time"`
	// Month carries the month field, and the day of month folded into it.
	Month rundeckXMLMonth `xml:"month"`
	// WeekDay is the weekday field.
	WeekDay rundeckXMLDay `xml:"weekday"`
	// DayOfMonth is the day of month field, which Rundeck 5 leaves empty as a marker after folding
	// the day itself onto the month element.
	DayOfMonth rundeckXMLDay `xml:"dayofmonth"`
	// Year is the year field, which a standard cron expression cannot represent.
	Year rundeckXMLYear `xml:"year"`
}

// rundeckXMLTime is the clock portion of a schedule.
type rundeckXMLTime struct {
	// Hour is the hour field.
	Hour string `xml:"hour,attr"`
	// Minute is the minute field.
	Minute string `xml:"minute,attr"`
	// Seconds is the seconds field, which a standard cron expression cannot represent.
	Seconds string `xml:"seconds,attr"`
}

// rundeckXMLMonth is a schedule's month element, which also carries the day of month.
type rundeckXMLMonth struct {
	// Month is the month field.
	Month string `xml:"month,attr"`
	// Day is the day of month, which Rundeck writes here rather than on the dayofmonth element.
	Day string `xml:"day,attr"`
}

// rundeckXMLDay is a schedule element carrying a single day expression.
type rundeckXMLDay struct {
	// Day is the day expression.
	Day string `xml:"day,attr"`
}

// rundeckXMLYear is a schedule's year element.
type rundeckXMLYear struct {
	// Year is the year field.
	Year string `xml:"year,attr"`
}

// rundeckJobFromXML converts one archive job into the shape the job export path reads, so both
// artifacts run through one mapping and cannot drift into producing different templates for the
// same job.
func rundeckJobFromXML(x rundeckXMLJob) rundeckJob {
	job := rundeckJob{
		Name: x.Name, Group: x.Group, Project: x.Context.Project, Description: x.Description,
		Timeout:          x.Timeout,
		ScheduleEnabled:  rundeckXMLBoolPtr(x.ScheduleEnabled),
		ExecutionEnabled: rundeckXMLBoolPtr(x.ExecutionEnabled),
	}
	for _, o := range x.Context.Options {
		job.Options = append(job.Options, rundeckOption{
			Name: o.Name, Description: o.Description, Value: o.Value,
			Values:      rundeckOptionValues(o),
			Required:    rundeckXMLBool(o.Required),
			Enforced:    rundeckXMLBool(o.Enforced),
			Secure:      rundeckXMLBool(o.Secure),
			Multivalued: rundeckXMLBool(o.Multivalued),
		})
	}
	job.Sequence.KeepGoing = rundeckXMLBool(x.Sequence.KeepGoing)
	for _, c := range x.Sequence.Commands {
		job.Sequence.Commands = append(job.Sequence.Commands, rundeckCommandFromXML(c))
	}
	job.NodeFilters.Filter = x.NodeFilters.Filter
	job.NodeFilters.Dispatch.KeepGoing = rundeckXMLBool(x.Dispatch.KeepGoing)
	if n, err := strconv.Atoi(strings.TrimSpace(x.Dispatch.ThreadCount)); err == nil {
		job.NodeFilters.Dispatch.ThreadCount = rundeckInt(n)
	}
	if x.Schedule != nil {
		job.Schedule = rundeckScheduleFromXML(*x.Schedule)
	}
	return job
}

// rundeckCommandFromXML converts one archive step into the shape the shared mapping reads. A plugin
// step keeps its provider name, which is the part the report names.
func rundeckCommandFromXML(c rundeckXMLCommand) rundeckCommand {
	cmd := rundeckCommand{
		Description: c.Description, Exec: c.Exec, Script: c.Script,
		ScriptInterpreter: c.ScriptInterpreter, ScriptFile: c.ScriptFile, ScriptURL: c.ScriptURL,
	}
	if c.JobRef != nil {
		cmd.JobRef = &rundeckJobRef{Name: c.JobRef.Name, Group: c.JobRef.Group}
	}
	switch {
	case c.StepPlugin != nil:
		cmd.Type = c.StepPlugin.Type
	case c.NodeStepPlugin != nil:
		cmd.Type = c.NodeStepPlugin.Type
	}
	return cmd
}

// rundeckScheduleFromXML converts an archive schedule into the shape the shared mapping reads.
//
// The day of month is read from the month element as well as from the dayofmonth element. Rundeck 5
// folds the day onto the month element and writes an empty dayofmonth as the marker that the job
// schedules by day of month at all, so reading only the obvious element turned every monthly job
// into a daily one.
func rundeckScheduleFromXML(x rundeckXMLSchedule) *rundeckSchedule {
	sc := &rundeckSchedule{Crontab: x.Crontab, Month: x.Month.Month, Year: x.Year.Year}
	sc.Time.Hour = x.Time.Hour
	sc.Time.Minute = x.Time.Minute
	sc.Time.Seconds = x.Time.Seconds
	sc.WeekDay.Day = x.WeekDay.Day
	sc.DayOfMonth.Day = x.DayOfMonth.Day
	if strings.TrimSpace(sc.DayOfMonth.Day) == "" {
		sc.DayOfMonth.Day = x.Month.Day
	}
	return sc
}

// rundeckOptionValues splits an option's allowed set, which the archive writes as one delimited
// string rather than as a list.
func rundeckOptionValues(o rundeckXMLOption) []string {
	raw := strings.TrimSpace(o.Values)
	if raw == "" {
		return nil
	}
	delimiter := o.ValuesDelimiter
	if delimiter == "" {
		delimiter = ","
	}
	var out []string
	for _, v := range strings.Split(raw, delimiter) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// rundeckXMLBool reads a Rundeck XML boolean, which the archive writes as true or false.
//
// Anything else reads as false rather than failing the document. Decoding straight into a bool once
// cost a whole export over a single quoted number, and the same shape applies here: one attribute
// spelled in a way this did not expect must not discard the job it sits on.
func rundeckXMLBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

// rundeckXMLBoolPtr reads an optional Rundeck XML boolean, keeping the difference between an element
// the archive omitted and one it wrote as false. Rundeck omits both enabled flags when they are true,
// and the shared mapping reads the absence as true, so the distinction has to survive.
func rundeckXMLBoolPtr(s *string) *bool {
	if s == nil {
		return nil
	}
	v := rundeckXMLBool(*s)
	return &v
}

// parseManifest reads a JAR manifest, whose entries are Name: value pairs and whose long values
// continue onto the following line behind a single leading space.
func parseManifest(data []byte) map[string]string {
	out := map[string]string{}
	key := ""
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			key = ""
			continue
		}
		if strings.HasPrefix(line, " ") {
			if key != "" {
				out[key] += line[1:]
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			key = ""
			continue
		}
		key = strings.TrimSpace(name)
		out[key] = strings.TrimSpace(value)
	}
	return out
}

// parseJavaProperties reads a Java properties file into a map.
//
// Rundeck writes its project and SCM configuration in this format, which is not key=value with the
// rest of the line as the value. A comment opens with # or !, the separator may be =, :, or plain
// whitespace, a backslash escapes whatever follows it, and a line ending in an odd number of
// backslashes continues onto the next. The escaping is where this bites: Rundeck writes a repository
// URL as file\:///home/rundeck/... and a node source URL as https\://cmdb.example.com/..., so
// splitting on the first colon yields a key of "scm.export.config.url=file\" and reading the line as
// written yields a URL no git and no fetch can dial.
func parseJavaProperties(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range javaPropertyLines(string(data)) {
		if key, value, ok := splitJavaProperty(line); ok {
			out[key] = value
		}
	}
	return out
}

// javaPropertyLines returns the logical lines of a properties file, joining a line that continues
// onto the next and dropping blank and comment lines. A comment is only a comment at the start of a
// logical line, so a continuation carrying a # stays part of its value.
func javaPropertyLines(text string) []string {
	var lines []string
	var buf strings.Builder
	continuing := false
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimLeft(strings.TrimRight(raw, "\r"), " \t\f")
		if !continuing {
			if line == "" || line[0] == '#' || line[0] == '!' {
				continue
			}
		}
		if trailingBackslashes(line)%2 == 1 {
			buf.WriteString(line[:len(line)-1])
			continuing = true
			continue
		}
		buf.WriteString(line)
		lines = append(lines, buf.String())
		buf.Reset()
		continuing = false
	}
	if buf.Len() > 0 {
		lines = append(lines, buf.String())
	}
	return lines
}

// trailingBackslashes counts the backslashes a line ends with, which is what decides whether its
// final backslash continues the line or is an escaped backslash of its own.
func trailingBackslashes(line string) int {
	n := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		n++
	}
	return n
}

// splitJavaProperty splits one logical line into its key and value at the first unescaped separator,
// which may be =, :, or a run of whitespace. A line with no separator is a key with an empty value.
func splitJavaProperty(line string) (key, value string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' {
			i++
			continue
		}
		switch line[i] {
		case '=', ':':
			return unescapeJava(strings.TrimRight(line[:i], " \t\f")),
				unescapeJava(strings.TrimLeft(line[i+1:], " \t\f")), true
		case ' ', '\t', '\f':
			rest := strings.TrimLeft(line[i:], " \t\f")
			if rest != "" && (rest[0] == '=' || rest[0] == ':') {
				rest = strings.TrimLeft(rest[1:], " \t\f")
			}
			return unescapeJava(line[:i]), unescapeJava(rest), true
		}
	}
	if line == "" {
		return "", "", false
	}
	return unescapeJava(line), "", true
}

// unescapeJava resolves the backslash escapes a properties value may carry: the usual control
// characters, a four digit unicode escape, and a backslash before any other character, which stands
// for that character alone. That last rule is the one Rundeck's URLs rely on.
func unescapeJava(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			if i+4 < len(s) {
				if n, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(n))
					i += 4
					continue
				}
			}
			b.WriteByte('u')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
