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
	}
	if err := collectJenkinsJobs(root, "", &jobs); err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("read jenkins jobs: no config.xml found under %s. Point this at a "+
			"JENKINS_HOME, its jobs directory, or a single job's config.xml", path)
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

// stripXMLDeclaration removes a leading <?xml ... ?> from a document, which is only legal at the
// very start of one and so cannot survive being nested inside the bundle.
func stripXMLDeclaration(data []byte) []byte {
	trimmed := bytes.TrimLeft(data, " \t\r\n\uFEFF")
	if !bytes.HasPrefix(trimmed, []byte("<?xml")) {
		return trimmed
	}
	if end := bytes.Index(trimmed, []byte("?>")); end >= 0 {
		return bytes.TrimLeft(trimmed[end+2:], " \t\r\n")
	}
	return trimmed
}

// JenkinsJobNames lists the job names a bundle carries, for the message the import prints before it
// reports the plan.
func JenkinsJobNames(bundle []byte) []string {
	var doc struct {
		// Jobs are the bundled job elements, read only for their names.
		Jobs []struct {
			// Name is the job's full name.
			Name string `xml:"name,attr"`
		} `xml:"job"`
	}
	if err := xml.Unmarshal(bundle, &doc); err != nil {
		return nil
	}
	names := make([]string, 0, len(doc.Jobs))
	for _, j := range doc.Jobs {
		names = append(names, j.Name)
	}
	return names
}

// JenkinsFoundLine describes what a walk turned up, so an import that skips most of a Jenkins says
// so before it reports a small plan.
func JenkinsFoundLine(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Found %d job definition(s): %s", len(names), strings.Join(names, ", "))
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
		return nil, fmt.Errorf("read jenkins archive: %w", err)
	}
	if len(r.File) > maxJenkinsZipEntries {
		return nil, fmt.Errorf("read jenkins archive: it holds %d entries, more than the %d this "+
			"reads. Zip just the jobs directory rather than the whole JENKINS_HOME",
			len(r.File), maxJenkinsZipEntries)
	}
	var jobs []jenkinsJobFile
	total := 0
	for _, f := range r.File {
		if path.Base(f.Name) != "config.xml" || f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > maxJenkinsConfigSize {
			return nil, fmt.Errorf("read jenkins archive: %s is larger than a job definition "+
				"should be", path.Clean(f.Name))
		}
		data, err := readZipEntry(f)
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > maxJenkinsTotalSize {
			return nil, fmt.Errorf("read jenkins archive: the definitions in it exceed the %d MiB "+
				"this reads", maxJenkinsTotalSize>>20)
		}
		name := jenkinsNameFromZipPath(f.Name)
		if name == "" {
			continue
		}
		jobs = append(jobs, jenkinsJobFile{Name: name, Data: data})
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("read jenkins archive: no config.xml found in it. Zip the jobs " +
			"directory from your JENKINS_HOME")
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return encodeJenkinsBundle(jobs)
}

// readZipEntry reads one archive member under the per-entry ceiling.
func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("read jenkins archive: %w", err)
	}
	defer func() { _ = rc.Close() }()
	// The declared size is a claim the archive makes about itself, so the read is bounded again
	// rather than trusted: a zip may declare one byte and deliver a terabyte.
	data, err := io.ReadAll(io.LimitReader(rc, maxJenkinsConfigSize+1))
	if err != nil {
		return nil, fmt.Errorf("read jenkins archive: %w", err)
	}
	if len(data) > maxJenkinsConfigSize {
		return nil, fmt.Errorf("read jenkins archive: %s expands to more than a job definition "+
			"should", path.Clean(f.Name))
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
