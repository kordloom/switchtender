package importer

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kordloom/switchtender/internal/util"
)

// jenkinsJobsDir is the directory name Jenkins nests job definitions under, both at the top of
// JENKINS_HOME and inside every folder.
const jenkinsJobsDir = "jobs"

// JenkinsBundle collects Jenkins job definitions from a path into one document the importer reads.
//
// A job's name is the name of the directory holding its config.xml and appears nowhere inside the
// file, so the names are gathered here, where the directory layout is still visible, and handed over
// with each document. The path may be a JENKINS_HOME, its jobs directory, a single job directory, or
// one config.xml on its own.
func JenkinsBundle(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read jenkins jobs: %w", err)
	}
	if !info.IsDir() {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read jenkins jobs: %w", err)
		}
		// Zipping the jobs directory is the usual way to move a Jenkins off its own machine, so an
		// archive is read as one rather than being wrapped as though it were a job definition.
		if IsJenkinsZip(data) {
			return JenkinsBundleFromZip(data)
		}
		// A lone config.xml is named by the directory holding it, which is how Jenkins names it.
		return encodeJenkinsBundle([]jenkinsJobFile{{
			Name: filepath.Base(filepath.Dir(path)), Data: data,
		}})
	}

	var jobs []jenkinsJobFile
	// Pointing at JENKINS_HOME is the common case, so its jobs directory is entered for the caller.
	// Its own config.xml is the controller's configuration, not a job, and is deliberately not read.
	root := path
	if sub := filepath.Join(path, jenkinsJobsDir); jenkinsIsDir(sub) {
		root = sub
	} else if data, err := os.ReadFile(filepath.Join(path, "config.xml")); err == nil &&
		!jenkinsIsControllerConfig(data) {
		// A single job's directory, which holds its config.xml beside its builds and workspace. It
		// is named the way Jenkins names it, after the directory, and nothing under it is walked,
		// because a workspace checkout may carry a config.xml of its own that is no job. A folder
		// is entered through its jobs directory above, and a home with no jobs directory holds the
		// controller's configuration, which is no job and goes on to the walk and its refusal.
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("read jenkins jobs: %w", err)
		}
		return encodeJenkinsBundle([]jenkinsJobFile{{Name: filepath.Base(abs), Data: data}})
	}
	if err := collectJenkinsJobs(root, "", &jobs); err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("read jenkins jobs: no config.xml found under %s. Point this at a "+
			"JENKINS_HOME, its jobs directory, a single job's directory, or a single job's "+
			"config.xml", path)
	}
	return encodeJenkinsBundle(jobs)
}

// jenkinsJobFile is one job's definition and the name its directory gave it.
type jenkinsJobFile struct {
	// Name is the job's full name, with any folders it sits in joined by slashes.
	Name string
	// Data is the raw config.xml.
	Data []byte
}

// collectJenkinsJobs walks a jobs directory, descending through folders and qualifying each job's
// name with the folders above it.
func collectJenkinsJobs(dir, prefix string, jobs *[]jenkinsJobFile) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read jenkins jobs: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if prefix != "" {
			name = prefix + "/" + name
		}
		jobDir := filepath.Join(dir, entry.Name())
		if data, err := os.ReadFile(filepath.Join(jobDir, "config.xml")); err == nil {
			*jobs = append(*jobs, jenkinsJobFile{Name: name, Data: data})
		}
		// A folder holds its children in a nested jobs directory. Only that directory is followed,
		// so a job's builds and workspace, which hold far more files than its configuration, are
		// never walked.
		if nested := filepath.Join(jobDir, jenkinsJobsDir); jenkinsIsDir(nested) {
			if err := collectJenkinsJobs(nested, name, jobs); err != nil {
				return err
			}
		}
	}
	return nil
}

// jenkinsIsDir reports whether a path exists and is a directory.
func jenkinsIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// jenkinsIsControllerConfig reports whether a document is the controller's own configuration, the
// config.xml at the top of a JENKINS_HOME beside its jobs directory, which configures Jenkins
// itself rather than any job. Jenkins writes it behind an XML 1.1 declaration the decoder refuses,
// so its root element is read past the declaration.
func jenkinsIsControllerConfig(data []byte) bool {
	root, err := jenkinsRootElement(stripXMLDeclaration(data))
	return err == nil && root == jenkinsControllerRoot
}

// encodeJenkinsBundle wraps collected job documents in the single document the importer reads.
func encodeJenkinsBundle(jobs []jenkinsJobFile) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("<jobs>")
	for _, job := range jobs {
		b.WriteString(`<job name="`)
		// A job name may hold any character a directory name may, including one that would end the
		// attribute and let the rest of the name be read as markup.
		if err := xml.EscapeText(&b, []byte(job.Name)); err != nil {
			return nil, fmt.Errorf("read jenkins jobs: %w", err)
		}
		b.WriteString(`">`)
		b.Write(stripXMLDeclaration(job.Data))
		b.WriteString("</job>")
	}
	b.WriteString("</jobs>")
	return b.Bytes(), nil
}

// jenkinsDocumentBody returns a Jenkins document with its leading XML declaration removed, refusing
// one that declares something this decoder cannot honor.
//
// A real Jenkins writes every config.xml with an XML 1.1 declaration and Go's encoding/xml supports
// only 1.0, so the decoder refused the file before a single element was read. The CLI never met this
// because a bundle strips the declaration on the way in, but the HTTP import endpoint documents that
// its body may be one config.xml and hands those bytes straight to the mapper. Nothing a job's markup
// needs is decided by the version: 1.1 differs in which control characters it admits, and a name
// carrying one is refused here anyway.
//
// The encoding is read before the declaration goes, because dropping it also drops the decoder's own
// refusal of an encoding it cannot read. A latin-1 export would then be decoded as UTF-8 and import
// host names spelled in mojibake, which is a fleet naming machines that do not exist rather than a
// failed import.
func jenkinsDocumentBody(data []byte) ([]byte, error) {
	trimmed := bytes.TrimLeft(data, xmlLeadingBytes)
	end := bytes.Index(trimmed, []byte("?>"))
	if !bytes.HasPrefix(trimmed, []byte("<?xml")) || end < 0 {
		// No declaration, or an unterminated one the decoder reports better than this could.
		return trimmed, nil
	}
	decl := string(trimmed[:end])
	if enc := xmlDeclValue(decl, "encoding"); enc != "" &&
		!strings.EqualFold(enc, "utf-8") && !strings.EqualFold(enc, "utf8") {
		return nil, fmt.Errorf("parse jenkins xml: the document declares encoding %q and only "+
			"UTF-8 is read: re-save it as UTF-8", oneLine(enc))
	}
	if v := xmlDeclValue(decl, "version"); v != "" && v != "1.0" && v != "1.1" {
		return nil, fmt.Errorf("parse jenkins xml: the document declares XML version %q, which is "+
			"not a version Jenkins writes", oneLine(v))
	}
	return stripXMLDeclaration(data), nil
}

// xmlDeclValue returns the value of a named pseudo-attribute of an XML declaration, or empty when the
// declaration does not carry it. The declaration is read here rather than by the decoder, because the
// point of reading it is to decide what to do before the decoder sees the document.
func xmlDeclValue(decl, name string) string {
	i := strings.Index(decl, name+"=")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(decl[i+len(name)+1:])
	if rest == "" || (rest[0] != '\'' && rest[0] != '"') {
		return ""
	}
	quote, rest := rest[0], rest[1:]
	end := strings.IndexByte(rest, quote)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// xmlLeadingBytes are what may sit before a document's declaration: ASCII whitespace and a byte order
// mark, none of which is part of the declaration itself.
const xmlLeadingBytes = " \t\r\n\uFEFF"

// stripXMLDeclaration removes a leading <?xml ... ?> from a document, which is only legal at the
// very start of one and so cannot survive being nested inside the bundle.
func stripXMLDeclaration(data []byte) []byte {
	trimmed := bytes.TrimLeft(data, xmlLeadingBytes)
	if !bytes.HasPrefix(trimmed, []byte("<?xml")) {
		return trimmed
	}
	if end := bytes.Index(trimmed, []byte("?>")); end >= 0 {
		return bytes.TrimLeft(trimmed[end+2:], " \t\r\n")
	}
	return trimmed
}

// JenkinsJobNames lists the job names a bundle carries, for the message the import prints before it
// reports the plan. A folder is left out: it holds jobs rather than being one, and its name already
// leads the names of the jobs inside it.
func JenkinsJobNames(bundle []byte) []string {
	var doc jenkinsBundle
	if err := xml.Unmarshal(bundle, &doc); err != nil {
		return nil
	}
	names := make([]string, 0, len(doc.Jobs))
	for _, j := range doc.Jobs {
		if root, err := jenkinsRootElement(j.Inner); err == nil && root == jenkinsFolderRoot {
			continue
		}
		names = append(names, j.Name)
	}
	return names
}

// jenkinsFoundNames caps how many job names the found line spells out, so a Jenkins with thousands
// of jobs opens its report with a readable line rather than every name ahead of the summary.
const jenkinsFoundNames = 20

// JenkinsFoundLine describes what a walk turned up, so an import that skips most of a Jenkins says
// so before it reports a small plan.
func JenkinsFoundLine(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Found %d %s: %s", len(names),
		util.Plural(len(names), "job definition", "job definitions"),
		strings.Join(capList(names, jenkinsFoundNames), ", "))
}

// Jenkins archives are read under fixed ceilings. The upload endpoint is reachable by anyone allowed
// to import, and a zip is a hostile input: a few kilobytes can expand into gigabytes, and an archive
// of a real JENKINS_HOME carries build logs and workspaces far larger than the definitions wanted.
const (
	// maxJenkinsZipEntries caps how many archive members are examined.
	maxJenkinsZipEntries = 20000
	// maxJenkinsConfigSize caps one config.xml, which is a document rather than a payload.
	maxJenkinsConfigSize = 4 << 20
	// maxJenkinsTotalSize caps the definitions read out of one archive in total.
	maxJenkinsTotalSize = 64 << 20
)

// zipMagic is the local file header every zip archive starts with.
var zipMagic = []byte{'P', 'K', 0x03, 0x04}

// IsJenkinsZip reports whether a body is a zip archive rather than an XML document.
func IsJenkinsZip(data []byte) bool { return bytes.HasPrefix(data, zipMagic) }

// JenkinsBundleFromZip collects job definitions out of a zip of a Jenkins jobs directory.
//
// Jenkins has no single-file export the way AWX and Semaphore do; its definitions are a directory
// tree. Zipping that tree is the one way to hand a whole Jenkins to something over HTTP, so an
// upload is read here rather than being restricted to one job at a time.
func JenkinsBundleFromZip(data []byte) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, refuseArchive("read jenkins archive: %w", err)
	}
	if len(r.File) > maxJenkinsZipEntries {
		return nil, refuseArchive("read jenkins archive: it holds %d entries, more than the %d "+
			"this reads. Zip the jobs directory without each job's builds and workspace "+
			"directories: %s", len(r.File), maxJenkinsZipEntries, jenkinsZipCommand)
	}
	var jobs []jenkinsJobFile
	total, unnamed := 0, 0
	homes, held := jenkinsZipHomes(r.File)
	for _, f := range r.File {
		name := jenkinsZipName(f)
		if path.Base(name) != "config.xml" || f.FileInfo().IsDir() {
			continue
		}
		// A zip of a whole JENKINS_HOME holds the home's own files beside its jobs directory,
		// and several are named config.xml: the controller's, one for each user, and one for
		// each agent. The directory walk enters the jobs directory and reads nothing else, so a
		// config.xml inside a home but outside its jobs directory is passed over the same way.
		dirs := jenkinsZipDirs(name)
		if !jenkinsZipIsJob(dirs, homes, held) {
			continue
		}
		if f.UncompressedSize64 > maxJenkinsConfigSize {
			return nil, refuseArchive("read jenkins archive: %s is larger than a job definition "+
				"should be", path.Clean(name))
		}
		data, err := readZipEntry(f, "jenkins", maxJenkinsConfigSize)
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > maxJenkinsTotalSize {
			return nil, refuseArchive("read jenkins archive: the definitions in it exceed the "+
				"%d MiB this reads", maxJenkinsTotalSize>>20)
		}
		// A zip of a whole JENKINS_HOME carries the controller's own config.xml beside the jobs
		// directory. The directory walk never reads that file, so it is passed over here too rather
		// than read as a job named after the home.
		if jenkinsIsControllerConfig(data) {
			continue
		}
		jobName := jenkinsNameFromZipPath(name)
		if jobName == "" {
			unnamed++
			continue
		}
		jobs = append(jobs, jenkinsJobFile{Name: jobName, Data: data})
	}
	// A config.xml with no directory above it naming a job, such as one at the top of the archive,
	// has no name to import under, so it is refused. Saying no config.xml was found, when one was,
	// left the reader looking for a file that was right there.
	if len(jobs) == 0 && unnamed > 0 {
		return nil, refuseArchive("read jenkins archive: no config.xml in it sits under a " +
			"directory naming its job, so no job has a name to import under. Zip the job's " +
			"directory, or the jobs directory from your JENKINS_HOME")
	}
	if len(jobs) == 0 {
		return nil, refuseArchive("read jenkins archive: no config.xml found in it. Zip the jobs " +
			"directory from your JENKINS_HOME")
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return encodeJenkinsBundle(jobs)
}

// jenkinsZipCommand is the command that zips a jobs directory without the build history and
// workspace each job keeps under its own directory. Those hold far more files than the definitions
// and are what pushes a real jobs directory past the entry cap, so the cap names this rather than
// telling a reader who already zipped only the jobs directory to zip only the jobs directory.
const jenkinsZipCommand = `zip -r jobs.zip jobs -x '*/builds/*' '*/workspace/*'`

// jenkinsZipName returns an archive member's name with its separators read as slashes. An archive
// written on Windows may name its members with backslashes, which path reads as part of the name,
// so every test of a member's base name and directories runs on this form.
func jenkinsZipName(f *zip.File) string {
	return strings.ReplaceAll(f.Name, `\`, "/")
}

// jenkinsZipDirs returns the directories above an archive member, reading either separator the way
// jenkinsNameFromZipPath does.
func jenkinsZipDirs(name string) []string {
	segments := strings.Split(path.Clean(strings.ReplaceAll(name, `\`, "/")), "/")
	return segments[:len(segments)-1]
}

// jenkinsZipIsJob reports whether a config.xml under the given directories sits where the directory
// walk would read a job's: directly in a job directory, with the containers above it alternating
// jobs/<name> the way Jenkins nests folders.
//
// Every real jobs directory holds files named config.xml that are not jobs: archived artifacts
// under builds/<n>/archive/, a workspace checkout, each branch of a multibranch project under
// branches/, and each promotion under promotions/. The walk never enters those, because it reads
// only <dir>/<job>/config.xml and descends only into a nested jobs directory. Reading any of them
// would report a job that does not exist, and an archived artifact larger than a definition would
// refuse the whole archive, so each is passed over before its size is checked. held is the set of
// directories holding a config.xml, which is what tells a file inside a job from a job.
func jenkinsZipIsJob(dirs []string, homes, held map[string]bool) bool {
	first := indexOf(dirs, jenkinsJobsDir)
	if first < 0 || !homes[strings.Join(dirs[:first], "/")] {
		// Rooted at the jobs directory's contents or at one job, the job's own directory is the
		// only one above its config.xml. A jobs segment below a directory that is not a home, such
		// as one inside an archived artifact or a workspace, anchors nothing. A config.xml at the
		// very top is let through to be counted as a job with no name, so the refusal can say so.
		// Inside a home but outside its jobs directory, it is one of the home's own files. Below a
		// config.xml at the top, it is a file inside that one job, such as its workspace checkout.
		return len(dirs) <= 1 && !jenkinsInHome(dirs, homes) && !jenkinsInsideJob(dirs, held)
	}
	rest := dirs[first:]
	if len(rest)%2 != 0 {
		return false
	}
	for i := 0; i < len(rest); i += 2 {
		if rest[i] != jenkinsJobsDir {
			return false
		}
	}
	return true
}

// jenkinsZipHomes returns the directories an archive holds job definitions in a jobs directory
// under, each joined with slashes and empty for the top of the archive. They are what
// jenkinsNameFromZipPath drops from the front of a job's name: a JENKINS_HOME, or whatever the
// archive was rooted at above its jobs directory.
//
// A candidate with a config.xml in any directory above it is inside a job, because a JENKINS_HOME
// and the directory holding a jobs directory never are. Its jobs segment belongs to an archived
// artifact or a workspace checkout, which an archive zipped from inside the jobs directory, or from
// one job's directory, carries with nothing above it to tell it apart, so it is not a home.
//
// It also returns the set of directories holding a config.xml, joined the same way, so a member
// with no home above it can be told apart from a file inside a job.
func jenkinsZipHomes(files []*zip.File) (homes, held map[string]bool) {
	held = map[string]bool{}
	var candidates [][]string
	for _, f := range files {
		name := jenkinsZipName(f)
		if path.Base(name) != "config.xml" || f.FileInfo().IsDir() {
			continue
		}
		dirs := jenkinsZipDirs(name)
		held[strings.Join(dirs, "/")] = true
		if i := indexOf(dirs, jenkinsJobsDir); i >= 0 {
			candidates = append(candidates, dirs[:i])
		}
	}
	homes = map[string]bool{}
	for _, home := range candidates {
		if !jenkinsInsideJob(home, held) {
			homes[strings.Join(home, "/")] = true
		}
	}
	return homes, held
}

// jenkinsInsideJob reports whether any directory strictly above dirs, the top of the archive
// included, holds a config.xml, given held, the set of directories that do.
func jenkinsInsideJob(dirs []string, held map[string]bool) bool {
	for i := range dirs {
		if held[strings.Join(dirs[:i], "/")] {
			return true
		}
	}
	return false
}

// jenkinsInHome reports whether a member's directories lie inside one of homes, at any depth.
func jenkinsInHome(dirs []string, homes map[string]bool) bool {
	for i := 0; i <= len(dirs); i++ {
		if homes[strings.Join(dirs[:i], "/")] {
			return true
		}
	}
	return false
}

// readZipEntry reads one archive member under a per-entry ceiling, naming the artifact it came out
// of so the Jenkins and Rundeck readers report their own failures rather than each other's.
func readZipEntry(f *zip.File, artifact string, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, refuseArchive("read %s archive: %w", artifact, err)
	}
	defer func() { _ = rc.Close() }()
	// The declared size is a claim the archive makes about itself, so the read is bounded again
	// rather than trusted: a zip may declare one byte and deliver a terabyte.
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, refuseArchive("read %s archive: %w", artifact, err)
	}
	if int64(len(data)) > limit {
		return nil, refuseArchive("read %s archive: %s expands to more than one definition should",
			artifact, path.Clean(f.Name))
	}
	return data, nil
}

// jenkinsNameFromZipPath derives a job's full name from where its config.xml sits in the archive.
//
// Jenkins nests a folder's children in a jobs directory, so the layout alternates strictly:
// jobs/<name>/jobs/<name>/config.xml. Everything before the first jobs segment is whatever directory
// the archive was rooted at and is dropped, which is what keeps a zip of a whole JENKINS_HOME from
// prefixing every job with the home's own directory name.
func jenkinsNameFromZipPath(p string) string {
	segments := strings.Split(path.Clean(strings.ReplaceAll(p, `\`, "/")), "/")
	if len(segments) < 2 {
		return ""
	}
	segments = segments[:len(segments)-1]
	if first := indexOf(segments, jenkinsJobsDir); first >= 0 {
		segments = segments[first:]
	}
	var parts []string
	for i, seg := range segments {
		// Past the first jobs segment the container directories sit at every other position, so a
		// job genuinely named "jobs" is still kept when it lands in a name position.
		if seg == jenkinsJobsDir && i%2 == 0 && indexOf(segments, jenkinsJobsDir) == 0 {
			continue
		}
		if seg == "." || seg == ".." || seg == "" {
			continue
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "/")
}

// indexOf returns the first position of v in list, or -1.
func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}
