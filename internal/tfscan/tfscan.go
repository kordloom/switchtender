// Package tfscan reads a Terraform or OpenTofu configuration, and every module it calls, for the
// programs a plan of it would run, before the plan runs.
//
// A plan is the tool's no-change mode, and it is not free of execution. The external data source
// runs the program it names while the plan runs, with the run's credentials and environment, so a
// rule that exempted every plan as a preview let any program through as long as it was declared in
// a data block. This reads the configuration the way the plan will, parsed by the HCL parser
// Terraform is built on rather than matched by pattern, and reports each external data source it
// finds by the address a plan gives it.
//
// It fails closed. A module it cannot read, a source only known when the run plans, a registry
// module nobody has downloaded, and a file it cannot parse all leave the configuration
// unclassified, since what was not read could declare an external data source too. A finding means
// the plan cannot be classified as change free, not that it has side effects.
//
// Providers run code during a plan as well. They are code the team chose and installs, and this
// does not judge them: it looks for explicit program execution and for configuration it could not
// read, nothing else.
package tfscan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
	"github.com/zclconf/go-cty/cty"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// The scanner, as the evidence its scans leave names it.
const (
	// Scanner names this reader.
	Scanner = "terraform-external"
	// Version is the revision of the rules below. It changes whenever what the scan reads, or what
	// it counts as a finding, changes, so a classification can be traced to the rules that made it.
	Version = 1
)

// Bounds on how much one scan reads. A scan runs on the submit path, so a repository must not be
// able to make it slow or large: past any of these the scan stops following, says so, and the
// configuration is left unclassified rather than classified on what was read.
const (
	// maxFileBytes is the largest single file the scan reads.
	maxFileBytes = 1 << 20
	// maxTotalBytes is how much the scan reads across every file.
	maxTotalBytes = 16 << 20
	// maxFiles is how many file reads one scan makes.
	maxFiles = 256
	// maxDepth is how deeply module calls may nest before the scan stops following them.
	maxDepth = 32
	// maxCalls is how many module calls one scan follows. Two calls of the same module in each of a
	// few nested modules multiply, so this bounds the walk where depth alone does not.
	maxCalls = 256
	// maxLabel is how much of a name read from a file an entry repeats.
	maxLabel = 80
	// maxPath is how much of a file's path an entry repeats.
	maxPath = 2 * maxLabel
)

// Registry hosts a module address with no host of its own resolves against.
const (
	// terraformRegistry is the registry Terraform installs a short module address from.
	terraformRegistry = "registry.terraform.io"
	// openTofuRegistry is the registry OpenTofu installs a short module address from.
	openTofuRegistry = "registry.opentofu.org"
)

var (
	// errLimit marks a read refused because the scan has read as much as one scan may.
	errLimit = errors.New("scan limit reached")
	// errTooLarge marks a file larger than one scan reads.
	errTooLarge = errors.New("file too large")
)

// The schemas the scan reads blocks through. Each is partial: everything else in a body is left
// alone, so a configuration using constructs this scan does not model still parses.
var (
	// rootSchema selects the top-level blocks that can run a program during a plan, or call a module
	// that can.
	rootSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "ephemeral", LabelNames: []string{"type", "name"}},
		{Type: "module", LabelNames: []string{"name"}},
		{Type: "check", LabelNames: []string{"name"}},
	}}
	// checkSchema selects the data sources scoped to a check block, which a plan reads as well.
	checkSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "data", LabelNames: []string{"type", "name"}},
	}}
	// moduleSchema selects what a module call needs read: where it comes from and how it repeats.
	moduleSchema = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{
		{Name: "source"}, {Name: "version"}, {Name: "count"}, {Name: "for_each"},
	}}
	// repeatSchema selects how a data block repeats.
	repeatSchema = &hcl.BodySchema{Attributes: []hcl.AttributeSchema{
		{Name: "count"}, {Name: "for_each"},
	}}
	// registryNamespace matches a namespace or name in a module registry address.
	registryNamespace = regexp.MustCompile(`^[0-9A-Za-z](?:[0-9A-Za-z_-]{0,62}[0-9A-Za-z])?$`)
	// registrySystem matches the target system in a module registry address.
	registrySystem = regexp.MustCompile(`^[0-9a-z]{1,64}$`)
)

// Options shapes one scan.
type Options struct {
	// Tool is terraform or opentofu, terraform when empty. It decides which registry a module
	// address without a host means, and the tool the scan's record names.
	Tool string
	// Place names the tree the scan reads, as an entry says it, such as "the project".
	Place string
	// NotDownloaded says why a registry or remote module with no downloaded copy is unread, such as
	// why the gate's own download of the configuration's modules did not happen or did not finish.
	// Empty says the run's own init downloads it only after the gate has decided.
	NotDownloaded string
	// TrustModuleManifest says the .terraform/modules directory in the tree is one the gate itself
	// downloaded into, so its manifest and module copies may be read. It is false for a tree read
	// from a commit or a working directory, where a .terraform could be anything a pull request
	// author or a checkout left there: a committed manifest pointing at a planted copy would
	// otherwise let a registry or remote module read as a clean copy while the run's own init
	// installs the real one. Left false, every non-local module reads as not downloaded and goes
	// through the gate's own download, which installs into a tree this is then set true for.
	TrustModuleManifest bool
}

// Needs says what a scan could not read, split by whether downloading the configuration's modules
// could change that.
type Needs struct {
	// Downloads counts the registry and remote modules with no current downloaded copy, which a
	// download of the configuration's modules would put in place.
	Downloads int
	// Other counts everything else the scan could not read, which no download changes.
	Other int
}

// Scan reads the configuration in directory dir of fsys, and every module it calls, and returns
// what a plan of it would run as a program, with everything it examined and everything it could
// not read. Nothing outside fsys is read.
//
// A local module is read where its path points. A registry or remote module is read where the
// tool's own init installed it, which the module manifest in the working directory's
// .terraform/modules names, and only when that copy is the one the next init would keep: the
// source the configuration names and, for a registry module, a version its constraint allows. A
// module that is not there is not guessed at. It is listed as unread, which leaves the plan
// unclassified.
func Scan(fsys fs.FS, dir string, opts Options) run.DryRunScan {
	out, _ := ScanNeeds(fsys, dir, opts)
	return out
}

// ScanNeeds is Scan that also says what the scan could not read, and how much of it a download of
// the configuration's modules could change: a module with no current downloaded copy counts toward
// Downloads, and everything else toward Other.
func ScanNeeds(fsys fs.FS, dir string, opts Options) (run.DryRunScan, Needs) {
	s := &scan{
		fsys: fsys, tool: run.ToolTerraform, registry: terraformRegistry, place: opts.Place,
		files: map[string]bool{}, parsed: map[string]*parsedFile{}, listed: map[string]bool{},
		notDownloaded: opts.NotDownloaded, trustManifest: opts.TrustModuleManifest,
	}
	if s.place == "" {
		s.place = "the configuration's tree"
	}
	if s.notDownloaded == "" {
		s.notDownloaded = notDownloaded
	}
	if run.NormalizeTool(opts.Tool) == run.ToolOpenTofu {
		s.tool, s.registry = run.ToolOpenTofu, openTofuRegistry
	}
	root := path.Clean(strings.ReplaceAll(dir, `\`, "/"))
	switch {
	case !fs.ValidPath(root):
		s.skip("the working directory "+quote(dir), "outside "+s.place)
	case !s.isDir(root):
		s.skip("the working directory "+quote(dir), "not in "+s.place)
	default:
		s.root = root
		s.module(call{dir: root})
	}
	return s.result(), s.needs
}

// scan is one walk over a configuration and the modules it calls.
type scan struct {
	// fsys holds the configuration and everything the scan may read.
	fsys fs.FS
	// tool is terraform or opentofu.
	tool string
	// place names fsys in what an entry says.
	place string
	// registry is the host a module address without one resolves against.
	registry string
	// root is the working directory, where the module manifest lives.
	root string
	// files records each distinct file read, for the inputs the evidence names.
	files map[string]bool
	// parsed caches each configuration file's blocks by path, so a module called twice is parsed once.
	parsed map[string]*parsedFile
	// listed records the findings and unread entries already reported, so one problem met twice is
	// reported once.
	listed map[string]bool
	// findings collects each external data source found, by address.
	findings []string
	// unread collects what the scan could not read, each with why.
	unread []string
	// reads counts the file reads made, against maxFiles.
	reads int
	// bytes counts what the walk has read, against maxTotalBytes.
	bytes int
	// calls counts the module calls followed, against maxCalls.
	calls int
	// manifest maps a module key to the record of its installed copy, once read.
	manifest map[string]manifestRecord
	// manifestWhy says why the manifest could not be used, empty when it was read.
	manifestWhy string
	// manifestRead records that the manifest has been looked for.
	manifestRead bool
	// manifestMissing records that the manifest is not there at all, which a download writes.
	manifestMissing bool
	// trustManifest says the .terraform in the tree is one the gate downloaded, so it may be read.
	// Left false, a committed .terraform is ignored and every non-local module reads as not
	// downloaded, so the gate downloads it into a tree of its own rather than trusting a copy a
	// commit carried.
	trustManifest bool
	// notDownloaded says why a module with no downloaded copy is unread.
	notDownloaded string
	// needs counts what the scan could not read, by whether a download could change it.
	needs Needs
}

// call is one module the walk reads.
type call struct {
	// dir is the module's directory inside the tree.
	dir string
	// key is the module's key in the installed module manifest: empty for the root module, then
	// each call's name joined by dots.
	key string
	// address is what every address inside the module begins with: empty for the root module,
	// then module.<name>. for each call.
	address string
	// repeats names each enclosing module call that sets count or for_each, outermost first.
	repeats []string
	// depth is how many calls deep the module is.
	depth int
}

// parsedFile is what one configuration file declares that the scan reads.
type parsedFile struct {
	// externals are the external data sources and ephemeral resources it declares.
	externals []externalBlock
	// modules are the module calls it declares.
	modules []moduleBlock
}

// externalBlock is one data or ephemeral block of type external.
type externalBlock struct {
	// kind is data or ephemeral.
	kind string
	// name is the block's name label.
	name string
	// check names the check block it is scoped to, empty at the top level.
	check string
	// file is the file declaring it.
	file string
	// line is the line its declaration starts on.
	line int
	// repeat is count or for_each when the block sets one, empty otherwise.
	repeat string
}

// moduleBlock is one module call.
type moduleBlock struct {
	// name is the call's name label.
	name string
	// source is the source address, when it is a literal string.
	source string
	// hasSource reports that the call sets a source at all.
	hasSource bool
	// sourceKnown reports that the source is a literal the scan can read without evaluating.
	sourceKnown bool
	// constraint is the version constraint, when it is a literal string.
	constraint string
	// hasVersion reports that the call sets a version.
	hasVersion bool
	// versionKnown reports that the version is a literal the scan can read without evaluating.
	versionKnown bool
	// repeat is count or for_each when the call sets one, empty otherwise.
	repeat string
	// override reports that the call sits in an override file, which merges into a call declared
	// elsewhere and need not name a source of its own.
	override bool
}

// manifestRecord is one entry of the module manifest an init writes.
type manifestRecord struct {
	// Key is the module's key: each call's name from the root, joined by dots.
	Key string `json:"Key"`
	// Source is the source address the module was installed from, as the tool normalized it.
	Source string `json:"Source"`
	// Version is the registry version installed, empty for any other source.
	Version string `json:"Version"`
	// Dir is where the module's files are, relative to the working directory.
	Dir string `json:"Dir"`
}

// result returns the scan's record.
func (s *scan) result() run.DryRunScan {
	inputs := make([]string, 0, len(s.files))
	for name := range s.files {
		inputs = append(inputs, name)
	}
	sort.Strings(inputs)
	return run.DryRunScan{
		Tool: s.tool, Scanner: Scanner, Version: Version, Inputs: inputs,
		Findings: s.findings, Unread: s.unread,
	}.Classified()
}

// module reads every configuration file of the module c names and follows each module it calls.
func (s *scan) module(c call) {
	names, err := s.moduleFiles(c.dir)
	if err != nil {
		s.skip(moduleWhat(c, ""), s.why(err))
		return
	}
	for _, name := range names {
		f := s.parse(name)
		for _, ext := range f.externals {
			s.found(c, ext)
		}
		for _, m := range f.modules {
			s.follow(c, m)
		}
	}
}

// moduleFiles returns the configuration files of the module in dir, sorted: every file a plan
// loads, which is each .tf, .tf.json, .tofu, and .tofu.json file directly inside it. Both tools'
// files are read whichever tool runs, since reading a file the tool ignores can only find more.
// Files the tools skip as editor leftovers are skipped here too, the same way.
func (s *scan) moduleFiles(dir string) ([]string, error) {
	entries, err := fs.ReadDir(s.fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || ignoredFile(name) || configSuffix(name) == "" {
			continue
		}
		out = append(out, path.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// parse returns what the configuration file at name declares, reading and parsing it once. A file
// that cannot be read or parsed is listed as unread, and whatever did parse is still searched,
// since searching more can only find more.
func (s *scan) parse(name string) *parsedFile {
	if f, ok := s.parsed[name]; ok {
		return f
	}
	f := &parsedFile{}
	s.parsed[name] = f
	src, err := s.read(name)
	if err != nil {
		s.skip("file "+quote(name), s.why(err))
		return f
	}
	// The HCL parsers recurse once per nesting level with no depth guard, so a file nested past what
	// any configuration holds would overflow the stack and crash this process before it could be
	// read. It is left unread instead, the same fail-closed answer an unreadable file gets.
	if !withinNestingBound(src) {
		s.skip("file "+quote(name), fmt.Sprintf("its brackets nest deeper than the %d levels one "+
			"file may, so it is not parsed", maxNestingDepth))
		return f
	}
	var file *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(name, ".json") {
		file, diags = hcljson.Parse(src, name)
	} else {
		file, diags = hclsyntax.ParseConfig(src, name, hcl.InitialPos)
	}
	if diags.HasErrors() {
		s.skip("file "+quote(name), "it could not be parsed: "+firstError(diags))
	}
	if file == nil || file.Body == nil {
		return f
	}
	override := overrideFile(name)
	content, _, diags := file.Body.PartialContent(rootSchema)
	if diags.HasErrors() {
		s.skip("file "+quote(name), "it could not be read as configuration: "+firstError(diags))
	}
	if content == nil {
		return f
	}
	for _, b := range content.Blocks {
		switch b.Type {
		case "data", "ephemeral":
			if ext, ok := externalOf(b, ""); ok {
				f.externals = append(f.externals, ext)
			}
		case "module":
			f.modules = append(f.modules, moduleOf(b, override))
		case "check":
			inner, _, cdiags := b.Body.PartialContent(checkSchema)
			if cdiags.HasErrors() {
				s.skip("file "+quote(name), "a check block could not be read: "+firstError(cdiags))
			}
			if inner == nil {
				continue
			}
			for _, d := range inner.Blocks {
				if ext, ok := externalOf(d, b.Labels[0]); ok {
					f.externals = append(f.externals, ext)
				}
			}
		}
	}
	return f
}

// externalOf returns the block as an external block when its type is external. The type label is
// what selects the external provider's schema, whatever local name the provider was given, so the
// label alone decides.
func externalOf(b *hcl.Block, check string) (externalBlock, bool) {
	if len(b.Labels) < 2 || b.Labels[0] != "external" {
		return externalBlock{}, false
	}
	ext := externalBlock{
		kind: b.Type, name: b.Labels[1], check: check,
		file: b.DefRange.Filename, line: b.DefRange.Start.Line,
	}
	if attrs, _, _ := b.Body.PartialContent(repeatSchema); attrs != nil {
		ext.repeat = repeatOf(attrs.Attributes)
	}
	return ext, true
}

// moduleOf reads a module call's source, version, and repetition.
func moduleOf(b *hcl.Block, override bool) moduleBlock {
	m := moduleBlock{name: b.Labels[0], override: override}
	attrs, _, _ := b.Body.PartialContent(moduleSchema)
	if attrs == nil {
		return m
	}
	if a, ok := attrs.Attributes["source"]; ok {
		m.hasSource = true
		m.source, m.sourceKnown = literal(a)
	}
	if a, ok := attrs.Attributes["version"]; ok {
		m.hasVersion = true
		m.constraint, m.versionKnown = literal(a)
	}
	m.repeat = repeatOf(attrs.Attributes)
	return m
}

// repeatOf names the meta-argument that repeats a block, empty when none does.
func repeatOf(attrs hcl.Attributes) string {
	if _, ok := attrs["for_each"]; ok {
		return "for_each"
	}
	if _, ok := attrs["count"]; ok {
		return "count"
	}
	return ""
}

// literal returns the string an attribute holds when it is a literal the scan can read without
// evaluating anything. A reference, a function call, or an interpolation of either is only known
// when the run plans, so it is reported as not known rather than guessed at.
func literal(a *hcl.Attribute) (string, bool) {
	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsWhollyKnown() || !v.Type().Equals(cty.String) {
		return "", false
	}
	return v.AsString(), true
}

// found records an external block as a finding, by the address a plan gives it.
func (s *scan) found(c call, ext externalBlock) {
	addr := c.address + ext.kind + ".external." + clip(ext.name)
	if s.listed["finding\x00"+addr] {
		return
	}
	s.listed["finding\x00"+addr] = true
	var notes []string
	if ext.check != "" {
		notes = append(notes, "in check "+quote(ext.check))
	}
	if ext.repeat != "" {
		notes = append(notes, "once for each instance its "+ext.repeat+" makes")
	}
	for _, outer := range c.repeats {
		notes = append(notes, "once for each instance of "+outer)
	}
	where := fmt.Sprintf("%s line %d", util.Clip(util.SafeText(ext.file), maxPath), ext.line)
	if len(notes) > 0 {
		where += ", " + strings.Join(notes, ", ")
	}
	verb := "runs a program during plan"
	if ext.kind == "ephemeral" {
		verb = "may run a program during plan"
	}
	s.findings = append(s.findings, addr+" "+verb+" ("+where+")")
}

// follow reads the module a call names, when the scan can tell which files that is.
func (s *scan) follow(parent call, m moduleBlock) {
	if m.override && !m.hasSource {
		// It merges into the call it overrides, whose source is read where that call is declared.
		return
	}
	key := m.name
	if parent.key != "" {
		key = parent.key + "." + m.name
	}
	child := call{
		key: key, address: parent.address + "module." + clip(m.name) + ".",
		repeats: parent.repeats, depth: parent.depth + 1,
	}
	if m.repeat != "" {
		addr := strings.TrimSuffix(child.address, ".")
		child.repeats = append(append([]string(nil), parent.repeats...), addr)
	}
	what := moduleWhat(child, "")
	switch {
	case !m.hasSource:
		s.skip(what, "it names no source")
		return
	case !m.sourceKnown:
		s.skip(what, "its source is only known when the run plans")
		return
	case child.depth > maxDepth:
		s.skip(moduleWhat(child, m.source), fmt.Sprintf("nested more than %d deep", maxDepth))
		return
	case s.calls >= maxCalls:
		s.skip(moduleWhat(child, m.source), fmt.Sprintf("past the %d module calls one scan follows",
			maxCalls))
		return
	}
	s.calls++
	dir, ok := s.resolve(parent, child, m)
	if !ok {
		return
	}
	child.dir = dir
	s.module(child)
}

// resolve returns the directory the module a call names is read from: where a local path points,
// or where the tool's own init installed a registry or remote module. It lists the module as
// unread and returns false when there is no such directory the scan can trust.
func (s *scan) resolve(parent, child call, m moduleBlock) (string, bool) {
	what := moduleWhat(child, m.source)
	src := strings.ReplaceAll(m.source, `\`, "/")
	if localSource(src) {
		dir := path.Join(parent.dir, src)
		switch {
		case !fs.ValidPath(dir):
			s.skip(what, "outside "+s.place)
			return "", false
		case !s.isDir(dir):
			s.skip(what, "not in "+s.place)
			return "", false
		}
		return dir, true
	}
	if m.hasVersion && !m.versionKnown {
		s.skip(what, "its version is only known when the run plans")
		return "", false
	}
	s.loadManifest()
	switch {
	case s.manifestMissing:
		s.missing(what, s.notDownloaded)
		return "", false
	case s.manifestWhy != "":
		s.skip(what, s.manifestWhy)
		return "", false
	}
	rec, ok := s.manifest[child.key]
	if !ok {
		s.missing(what, s.notDownloaded)
		return "", false
	}
	if !s.sameSource(m.source, rec.Source) {
		s.missing(what, "the copy downloaded here is of "+quote(rec.Source)+
			", so the run's own init replaces it with a copy the gate has not read")
		return "", false
	}
	if m.hasVersion && rec.Version != "" && !allows(m.constraint, rec.Version) {
		s.missing(what, "the copy downloaded here is version "+quote(rec.Version)+", which "+
			quote(m.constraint)+" does not allow, so the run's own init replaces it")
		return "", false
	}
	recDir := strings.ReplaceAll(rec.Dir, `\`, "/")
	// A manifest written under a data directory named by an absolute path records absolute
	// directories. Joined to the tree they would read a relative copy that is not the one the tool
	// uses, so an absolute directory is outside the tree, as it really is.
	absolute := path.IsAbs(recDir) || (len(recDir) > 1 && recDir[1] == ':')
	dir := path.Join(s.root, recDir)
	switch {
	case absolute || !fs.ValidPath(dir):
		s.skip(what, "its downloaded copy is outside "+s.place)
		return "", false
	case !s.isDir(dir):
		s.missing(what, "the manifest names a downloaded copy that is not there")
		return "", false
	}
	return dir, true
}

// notDownloaded says why a registry or remote module the manifest does not hold cannot be read.
const notDownloaded = "not downloaded here, and the run's own init installs it only after the " +
	"gate has decided"

// loadManifest reads the module manifest the tool's init wrote in the working directory, once.
func (s *scan) loadManifest() {
	if s.manifestRead {
		return
	}
	s.manifestRead = true
	// A .terraform the gate did not download is never trusted: a commit or a working directory can
	// carry one a pull request author planted, pointing a registry or remote module at a clean copy
	// while the real module hides an external data source the run's own init would install. Reading
	// it as absent sends every non-local module through the gate's own download instead.
	if !s.trustManifest {
		s.manifestMissing = true
		return
	}
	name := path.Join(s.root, ".terraform", "modules", "modules.json")
	content, err := s.read(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.manifestMissing = true
		return
	case err != nil:
		s.manifestWhy = "the module manifest " + quote(name) + " is " + s.why(err)
		return
	}
	var doc struct {
		// Modules are the installed modules.
		Modules []manifestRecord `json:"Modules"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		s.manifestWhy = "the module manifest " + quote(name) + " could not be parsed"
		return
	}
	s.manifest = make(map[string]manifestRecord, len(doc.Modules))
	for _, rec := range doc.Modules {
		s.manifest[rec.Key] = rec
	}
}

// sameSource reports whether a module installed from recorded is what the configuration's source
// names, as the tool's init compares them. The init normalizes an address before it records it,
// so a registry address gains its host and a GitHub shorthand becomes the git address it stands
// for. Anything else must match as written. A form this does not know reads as a different
// source, which leaves the module unread rather than read from a copy the init would replace.
func (s *scan) sameSource(configured, recorded string) bool {
	if configured == recorded {
		return true
	}
	if reg, ok := registrySource(configured, s.registry); ok && reg == recorded {
		return true
	}
	if gh, ok := githubSource(configured); ok && gh == recorded {
		return true
	}
	return false
}

// allows reports whether a registry version satisfies a version constraint, read with the version
// library Terraform's own module installer uses. Anything that does not parse allows nothing.
func allows(constraint, installed string) bool {
	c, err := version.NewConstraint(constraint)
	if err != nil {
		return false
	}
	v, err := version.NewVersion(installed)
	if err != nil {
		return false
	}
	return c.Check(v)
}

// read returns one file's content and counts it against the scan's bounds.
func (s *scan) read(name string) ([]byte, error) {
	if s.reads >= maxFiles || s.bytes >= maxTotalBytes {
		return nil, errLimit
	}
	s.reads++
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxFileBytes {
		return nil, errTooLarge
	}
	s.bytes += len(content)
	s.files[name] = true
	return content, nil
}

// why says why something could not be read, in the words an entry uses.
func (s *scan) why(err error) string {
	switch {
	case errors.Is(err, errLimit):
		return fmt.Sprintf("past the %d files or %d MiB one scan reads", maxFiles, maxTotalBytes>>20)
	case errors.Is(err, errTooLarge):
		return fmt.Sprintf("larger than the %d MiB one file may be", maxFileBytes>>20)
	case errors.Is(err, fs.ErrNotExist):
		return "not in " + s.place
	default:
		return "unreadable"
	}
}

// skip lists something the plan runs that the scan could not read, and why, once.
func (s *scan) skip(what, why string) {
	if s.unreadOnce(what, why) {
		s.needs.Other++
	}
}

// missing lists a registry or remote module with no current downloaded copy, and why, once. A
// download of the configuration's modules is what would put one in place.
func (s *scan) missing(what, why string) {
	if s.unreadOnce(what, why) {
		s.needs.Downloads++
	}
}

// unreadOnce lists what the scan could not read, and why, and reports whether this is the first
// time it was listed.
func (s *scan) unreadOnce(what, why string) bool {
	entry := what + " (" + why + ")"
	if s.listed[entry] {
		return false
	}
	s.listed[entry] = true
	s.unread = append(s.unread, entry)
	return true
}

// isDir reports whether name is a directory in the tree.
func (s *scan) isDir(name string) bool {
	info, err := fs.Stat(s.fsys, name)
	return err == nil && info.IsDir()
}

// moduleWhat names a module as an entry repeats it: the root module by its directory, any other by
// its address, with the source it names when there is one.
func moduleWhat(c call, source string) string {
	if c.address == "" {
		return "the module in " + quote(c.dir)
	}
	what := strings.TrimSuffix(c.address, ".")
	if source != "" {
		what += " from " + quote(source)
	}
	return what
}

// configSuffix returns the configuration file suffix name ends in, empty for any other file.
func configSuffix(name string) string {
	for _, suffix := range []string{".tf.json", ".tofu.json", ".tf", ".tofu"} {
		if strings.HasSuffix(name, suffix) {
			return suffix
		}
	}
	return ""
}

// ignoredFile reports whether the tools skip a file by its name: a hidden file, or what an editor
// leaves behind beside the file it is editing.
func ignoredFile(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") ||
		(strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"))
}

// overrideFile reports whether name is an override file, which merges into blocks declared
// elsewhere in its module rather than declaring its own.
func overrideFile(name string) bool {
	base := strings.TrimSuffix(path.Base(name), configSuffix(name))
	return base == "override" || strings.HasSuffix(base, "_override")
}

// localSource reports whether a module source is a path on disk, which the tools recognize by its
// prefix alone.
func localSource(src string) bool {
	return strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../")
}

// registrySource returns the address a module registry source normalizes to: a host, a namespace,
// a name, and a target system, with any subdirectory. A source without a host is the default
// registry's. It reports false for anything that is not a registry address.
func registrySource(src, defaultHost string) (string, bool) {
	pkg, sub := splitSubdir(src)
	if strings.ContainsAny(pkg, "?@") || strings.Contains(pkg, "::") {
		return "", false
	}
	parts := strings.Split(pkg, "/")
	host := defaultHost
	switch len(parts) {
	case 3:
	case 4:
		host = strings.ToLower(parts[0])
		parts = parts[1:]
		if host == "github.com" || host == "bitbucket.org" || !strings.Contains(host, ".") {
			return "", false
		}
	default:
		return "", false
	}
	if !registryNamespace.MatchString(parts[0]) || !registryNamespace.MatchString(parts[1]) ||
		!registrySystem.MatchString(parts[2]) {
		return "", false
	}
	out := host + "/" + strings.Join(parts, "/")
	if sub != "" {
		out += "//" + sub
	}
	return out, true
}

// githubSource returns the git address a GitHub shorthand source stands for, as the tools'
// installer rewrites it, and false for any other source.
func githubSource(src string) (string, bool) {
	if !strings.HasPrefix(src, "github.com/") {
		return "", false
	}
	pkg, sub := splitSubdir(src)
	query := ""
	if q := strings.Index(pkg, "?"); q >= 0 {
		pkg, query = pkg[:q], pkg[q:]
	}
	parts := strings.Split(pkg, "/")
	if len(parts) < 3 {
		return "", false
	}
	repo := strings.Join(parts[:3], "/")
	if !strings.HasSuffix(repo, ".git") {
		repo += ".git"
	}
	if extra := strings.Join(parts[3:], "/"); extra != "" {
		sub = path.Join(extra, sub)
	}
	out := "git::https://" + repo
	if sub != "" {
		out += "//" + sub
	}
	return out + query, true
}

// splitSubdir splits a module source into the package it downloads and the subdirectory within it,
// marked by a double slash that is not part of a scheme. A query string stays with the package.
func splitSubdir(src string) (string, string) {
	offset := 0
	if i := strings.Index(src, "://"); i >= 0 {
		offset = i + 3
	}
	i := strings.Index(src[offset:], "//")
	if i < 0 {
		return src, ""
	}
	i += offset
	pkg, sub := src[:i], src[i+2:]
	if q := strings.Index(sub, "?"); q >= 0 {
		pkg += sub[q:]
		sub = sub[:q]
	}
	return pkg, sub
}

// firstError returns the first error diagnostic in diags as one line, with where it is.
func firstError(diags hcl.Diagnostics) string {
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		// A diagnostic summary can quote a token read from the file, so it is a value from somebody
		// else's bytes and passes through SafeText, the same as every other file-derived string a
		// record carries, before it reaches a pull request comment, the API, or a log.
		msg := clip(util.SafeText(d.Summary))
		if d.Subject != nil {
			msg += fmt.Sprintf(", on line %d", d.Subject.Start.Line)
		}
		return msg
	}
	return "an unknown error"
}

// quote renders a name read from a file as an entry repeats it.
func quote(s string) string {
	return strconv.Quote(clip(util.SafeText(s)))
}

// clip bounds a name read from a file to maxLabel characters.
func clip(s string) string {
	return util.Clip(s, maxLabel)
}
