package ansibleruntime

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// lockFS holds the requirements locks, one per supported ansible-core release, written by
// scripts/ansible-runtime-locks.py.
//
//go:embed locks/*.txt
var lockFS embed.FS

// Lock is one requirements lock: the ansible-core release it installs, the controller Python
// versions that release runs on, and every package it installs, each pinned with its hashes.
type Lock struct {
	// Release is the ansible-core release the lock installs.
	Release string
	// PythonMin is the oldest Python, as 3.N, the release supports on a control node.
	PythonMin string
	// PythonMax is the newest Python, as 3.N, the release supports on a control node.
	PythonMax string
	// Requirements are the pinned packages, in the order the lock lists them.
	Requirements []Requirement
	// Content is the lock exactly as pip reads it.
	Content []byte
}

// Requirement is one pinned package in a lock.
type Requirement struct {
	// Name is the package name as the lock spells it.
	Name string
	// Version is the exact version the package is pinned to.
	Version string
	// Marker is the environment marker that limits where it installs, empty for everywhere.
	Marker string
	// Hashes are the SHA-256 digests, in hex, pip accepts for the package's files.
	Hashes []string
}

// SHA256 returns the hex SHA-256 of the lock's content, which an install records so a later binary
// with a regenerated lock for the same release replaces the environment instead of trusting it.
func (l *Lock) SHA256() string {
	sum := sha256.Sum256(l.Content)
	return hex.EncodeToString(sum[:])
}

var (
	// releasePattern matches an exact ansible-core release.
	releasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	// minorPattern matches an ansible-core minor version.
	minorPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	// pinPattern matches a requirement pinned to one version, with an optional environment marker.
	pinPattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)==([0-9A-Za-z.!+_-]+)` +
		`(?:\s*;\s*([^;]+?))?$`)
	// hashPattern matches one hash option.
	hashPattern = regexp.MustCompile(`^--hash=sha256:([0-9a-f]{64})$`)
	// pythonHeaderPattern matches the header naming the controller Python range.
	pythonHeaderPattern = regexp.MustCompile(`^# python: (3\.[0-9]+)-(3\.[0-9]+)$`)
	// releaseHeaderPattern matches the header naming the ansible-core release.
	releaseHeaderPattern = regexp.MustCompile(`^# ansible-core: ([0-9]+\.[0-9]+\.[0-9]+)$`)
)

// ParseLock reads a requirements lock, refusing anything pip would install without checking a
// hash or from anywhere but the package index it is pointed at: a requirement not pinned to one
// version or carrying no SHA-256, a URL or path requirement, an option line such as --index-url,
// -r, or -e, an environment variable reference, and a lock that does not pin ansible-core to the
// release its header names.
func ParseLock(content []byte) (*Lock, error) {
	if bytes.Contains(content, []byte("${")) {
		return nil, fmt.Errorf("%w: it references an environment variable, which pip expands",
			ErrLock)
	}
	if err := checkLineBreaks(content); err != nil {
		return nil, err
	}
	lock := &Lock{Content: content}
	var pending strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), " \t\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if pending.Len() > 0 {
				return nil, fmt.Errorf("%w: line %d: a comment inside a continued requirement", ErrLock,
					n)
			}
			if err := lock.readHeader(trimmed); err != nil {
				return nil, fmt.Errorf("%w: line %d: %w", ErrLock, n, err)
			}
			continue
		}
		line = pipComment.ReplaceAllString(line, "")
		cont := strings.HasSuffix(line, `\`)
		pending.WriteString(strings.TrimSuffix(line, `\`))
		pending.WriteByte(' ')
		if cont {
			continue
		}
		logical := strings.TrimSpace(pending.String())
		pending.Reset()
		if logical == "" {
			continue
		}
		req, err := parseRequirement(logical)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrLock, n, err)
		}
		lock.Requirements = append(lock.Requirements, req)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLock, err)
	}
	if pending.Len() > 0 {
		return nil, fmt.Errorf("%w: it ends inside a continued requirement", ErrLock)
	}
	if err := lock.check(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLock, err)
	}
	return lock, nil
}

// pipComment matches a comment the way pip strips one from a requirements line: a # at the start
// of the line or after whitespace, to the end of the line.
var pipComment = regexp.MustCompile(`(^|\s+)#.*$`)

// checkLineBreaks refuses a lock holding any character pip would split a line at, other than the
// newline ParseLock splits at, and any other control character but tab. pip splits a requirements
// file with Python's str.splitlines, which also breaks at a carriage return, a vertical tab, a form
// feed, 0x1c to 0x1e, NEL, and the Unicode line and paragraph separators, so a comment holding one
// of them would hide an option line from this check that pip then obeys. It also refuses a lock
// that is not valid UTF-8, which pip would not read the way this check does.
func checkLineBreaks(content []byte) error {
	if !utf8.Valid(content) {
		return fmt.Errorf("%w: it is not valid UTF-8", ErrLock)
	}
	line := 1
	for _, r := range string(content) {
		switch {
		case r == '\n':
			line++
		case r == '\t':
		case r < 0x20, r == 0x7f, r == 0x85, r == 0x2028, r == 0x2029:
			return fmt.Errorf("%w: line %d holds the character %U, which pip reads as a line break "+
				"or control character and this check does not", ErrLock, line, r)
		}
	}
	return nil
}

// readHeader records the release or Python range a header comment names, and ignores any other
// comment.
func (l *Lock) readHeader(line string) error {
	if m := releaseHeaderPattern.FindStringSubmatch(line); m != nil {
		if l.Release != "" {
			return fmt.Errorf("a second ansible-core header")
		}
		l.Release = m[1]
		return nil
	}
	if m := pythonHeaderPattern.FindStringSubmatch(line); m != nil {
		if l.PythonMin != "" {
			return fmt.Errorf("a second python header")
		}
		l.PythonMin, l.PythonMax = m[1], m[2]
	}
	return nil
}

// parseRequirement reads one logical requirement line: a pinned package, an optional marker, and
// one or more SHA-256 hash options, and nothing else.
func parseRequirement(line string) (Requirement, error) {
	if strings.HasPrefix(line, "-") {
		return Requirement{}, fmt.Errorf("option line %q: a lock holds requirements only",
			firstField(line))
	}
	spec, opts, _ := strings.Cut(line, " --")
	if opts != "" {
		opts = "--" + opts
	}
	m := pinPattern.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return Requirement{}, fmt.Errorf("requirement %q is not pinned to one version with ==",
			firstField(spec))
	}
	req := Requirement{Name: m[1], Version: m[2], Marker: strings.TrimSpace(m[3])}
	for _, opt := range strings.Fields(opts) {
		h := hashPattern.FindStringSubmatch(opt)
		if h == nil {
			return Requirement{}, fmt.Errorf("requirement %s has option %q, and only "+
				"--hash=sha256: is allowed", req.Name, opt)
		}
		req.Hashes = append(req.Hashes, h[1])
	}
	if len(req.Hashes) == 0 {
		return Requirement{}, fmt.Errorf("requirement %s has no sha256 hash", req.Name)
	}
	return req, nil
}

// check confirms the headers are present and ansible-core is pinned to the release they name,
// once, and that no package appears twice.
func (l *Lock) check() error {
	if l.Release == "" {
		return fmt.Errorf("no '# ansible-core: X.Y.Z' header")
	}
	if l.PythonMin == "" {
		return fmt.Errorf("no '# python: 3.N-3.M' header")
	}
	if pythonMinor(l.PythonMin) > pythonMinor(l.PythonMax) {
		return fmt.Errorf("python range %s-%s is empty", l.PythonMin, l.PythonMax)
	}
	seen := map[string]bool{}
	core := 0
	for _, r := range l.Requirements {
		name := normalizeName(r.Name)
		if seen[name] {
			return fmt.Errorf("%s is pinned twice", r.Name)
		}
		seen[name] = true
		if name == "ansible-core" {
			core++
			if r.Version != l.Release {
				return fmt.Errorf("it pins ansible-core %s, and its header names %s", r.Version,
					l.Release)
			}
		}
	}
	if core == 0 {
		return fmt.Errorf("it does not pin ansible-core")
	}
	return nil
}

// normalizeName returns a package name in the form PyPI compares names in.
func normalizeName(name string) string {
	return strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(name))
}

// firstField returns the first whitespace-separated field of s, for naming a line in an error.
func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

// pythonMinor returns N from a Python version 3.N, or -1 when it is not one.
func pythonMinor(v string) int {
	rest, ok := strings.CutPrefix(v, "3.")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return -1
	}
	return n
}

// Locks returns every embedded lock, oldest release first.
func Locks() ([]*Lock, error) {
	names, err := fs.Glob(lockFS, "locks/*.txt")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLock, err)
	}
	out := make([]*Lock, 0, len(names))
	for _, name := range names {
		content, err := lockFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrLock, name, err)
		}
		lock, err := ParseLock(content)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if name != "locks/ansible-core-"+lock.Release+".txt" {
			return nil, fmt.Errorf("%w: %s holds ansible-core %s", ErrLock, name, lock.Release)
		}
		out = append(out, lock)
	}
	slices.SortFunc(out, func(a, b *Lock) int { return compareReleases(a.Release, b.Release) })
	return out, nil
}

// Releases returns the ansible-core releases the embedded locks install, oldest first.
func Releases() []string {
	locks, err := Locks()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(locks))
	for _, l := range locks {
		out = append(out, l.Release)
	}
	return out
}

// LockFor returns the lock for version: the newest supported release when version is empty, the
// pinned release of a minor version such as 2.20, or the exact release named. Anything else is
// ErrVersion, naming what is supported.
func LockFor(version string) (*Lock, error) {
	locks, err := Locks()
	if err != nil {
		return nil, err
	}
	if len(locks) == 0 {
		return nil, fmt.Errorf("%w: this build carries no locks", ErrLock)
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	switch {
	case version == "":
		return locks[len(locks)-1], nil
	case minorPattern.MatchString(version):
		for _, l := range locks {
			if strings.HasPrefix(l.Release, version+".") {
				return l, nil
			}
		}
	case releasePattern.MatchString(version):
		for _, l := range locks {
			if l.Release == version {
				return l, nil
			}
		}
		for _, l := range locks {
			if minorOf(l.Release) == minorOf(version) {
				return nil, fmt.Errorf("%w: %s is not the pinned %s release. This build installs "+
					"ansible-core %s for %s", ErrVersion, version, minorOf(version), l.Release,
					minorOf(version))
			}
		}
	}
	return nil, fmt.Errorf("%w: %q. Supported: %s, or a minor version such as %s", ErrVersion,
		version, strings.Join(releasesOf(locks), ", "), minorOf(locks[len(locks)-1].Release))
}

// releasesOf returns the releases of locks.
func releasesOf(locks []*Lock) []string {
	out := make([]string, 0, len(locks))
	for _, l := range locks {
		out = append(out, l.Release)
	}
	return out
}

// minorOf returns the major.minor part of a release.
func minorOf(release string) string {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return release
	}
	return parts[0] + "." + parts[1]
}

// compareReleases orders two X.Y.Z releases numerically.
func compareReleases(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na - nb
		}
	}
	return len(pa) - len(pb)
}
