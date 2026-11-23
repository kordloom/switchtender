package roundhouse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProtectedPaths are the executor's own paths. No container mount may be one, sit below one, or
// hold one, so a run in an image never reaches the database, the signing identity, the license, or
// the directories the executor stages runs and loads code from.
//
// The database's directory and the identity directory are judged by their direct regular files and
// not as trees, because the quick start puts the database in the working directory, and a tree rule
// there would refuse every checkout beside it. A subdirectory of either passes. The four roots below
// them are trees: nothing a run names belongs inside one.
type ProtectedPaths struct {
	// DatabaseDir is the directory of a SQLite database. Every regular file directly in it is
	// protected: the database and its -wal, -shm, and -journal sidecars, the first admin token, the
	// producer key, and a backup written beside it. Empty for a PostgreSQL database.
	DatabaseDir string
	// IdentityDir is the directory holding the producer signing identity and, on a PostgreSQL
	// install, the first admin token. Every regular file directly in it is protected.
	IdentityDir string
	// License is the license file.
	License string
	// AnsibleRoot is the managed Ansible runtime root, protected as a tree.
	AnsibleRoot string
	// RunFiles is the run-files root every run directory lives under, protected as a tree.
	RunFiles string
	// ProjectCache is the root project checkouts live under, protected as a tree.
	ProjectCache string
}

// WithProtectedPaths registers the executor's own paths with the container runner, which refuses
// any mount that is, sits below, or holds one of them.
func WithProtectedPaths(p ProtectedPaths) Option {
	return func(a *ansibleRunner) { a.protected = &p }
}

// protectedKind says how a protected path is matched.
type protectedKind int

const (
	// protectedFile is one file: a mount that is it or holds it is refused.
	protectedFile protectedKind = iota
	// protectedDirFiles is a directory whose direct regular files are protected: a mount that is it,
	// holds it, or is a file directly in it is refused, and a subdirectory of it passes.
	protectedDirFiles
	// protectedTree is a directory and everything below it.
	protectedTree
)

// protectedEntry is one protected path with the words its refusal uses.
type protectedEntry struct {
	// kind says how path is matched.
	kind protectedKind
	// path is the protected path as registered.
	path string
	// what names the path in a refusal, as in "holds this server's database".
	what string
	// fix says what to do instead, for a mount that holds the path or is the path itself.
	fix string
	// besideFix says what to do instead, for a file directly in a protected directory.
	besideFix string
}

// entries returns the protected entries, plugin being the callback plugin directory the runner
// loads from, which is a tree like the other roots.
func (p *ProtectedPaths) entries(plugin string) []protectedEntry {
	var out []protectedEntry
	if p != nil {
		if p.DatabaseDir != "" {
			out = append(out, protectedEntry{kind: protectedDirFiles, path: p.DatabaseDir,
				what: "this server's database",
				fix: "Move the database with --db to its own directory, or name the subdirectory " +
					"holding the configuration.",
				besideFix: "Move the database with --db to its own directory, or move the file into a " +
					"subdirectory."})
		}
		if p.IdentityDir != "" {
			out = append(out, protectedEntry{kind: protectedDirFiles, path: p.IdentityDir,
				what: "this server's signing identity",
				fix: "Set SWITCHTENDER_IDENTITY_DIR to a directory of its own, or name a " +
					"subdirectory.",
				besideFix: "Set SWITCHTENDER_IDENTITY_DIR to a directory of its own, or move the file " +
					"into a subdirectory."})
		}
		if p.License != "" {
			out = append(out, protectedEntry{kind: protectedFile, path: p.License,
				what: "this server's license", fix: "Name a directory that does not hold it."})
		}
		trees := []struct{ path, what string }{
			{p.AnsibleRoot, "this server's managed Ansible runtime"},
			{p.RunFiles, "this server's run files"},
			{p.ProjectCache, "this server's project checkouts"},
		}
		for _, t := range trees {
			if t.path != "" {
				out = append(out, protectedEntry{kind: protectedTree, path: t.path, what: t.what,
					fix: "Name a directory inside it."})
			}
		}
	}
	if plugin != "" {
		out = append(out, protectedEntry{kind: protectedTree, path: plugin,
			what: "this server's callback plugin", fix: "Name a directory inside it."})
	}
	return out
}

// pathForms returns path made absolute and clean, followed by the form with every symlink resolved
// when that differs, so a protected path registered as /tmp/x on macOS is also known as
// /private/tmp/x, which is how a mount resolved through the /tmp link reaches it. A path that does
// not exist yet is resolved through its nearest existing ancestor.
func pathForms(path string) []string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return []string{filepath.Clean(path)}
	}
	forms := []string{abs}
	if resolved := resolveExisting(abs); resolved != abs {
		forms = append(forms, resolved)
	}
	return forms
}

// resolveExisting resolves the symlinks along path through its nearest existing ancestor, keeping
// the part below that ancestor as written.
func resolveExisting(path string) string {
	rest := ""
	probe := path
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return path
		}
		rest = filepath.Join(filepath.Base(probe), rest)
		probe = parent
	}
}

// foldPath lowers a path for comparison. The filesystems this runs on fold case, so a guard that
// matched byte for byte would hand out /Users/me/Infra to a run that spelled /users/me/infra.
func foldPath(p string) string {
	return strings.ToLower(p)
}

// pathHolds reports whether dir is a strict ancestor of path. Both are absolute, clean, and folded.
func pathHolds(dir, path string) bool {
	if dir == path {
		return false
	}
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, dir+"/")
}

// checkProtected judges one mount against the executor's own paths and the system directories no
// mount may take whole. named marks a path the run named, which is refused at or below a protected
// tree, where a path the dispatcher staged passes, since the run-files root and the project cache
// are where it stages. Every form of the mount is judged: the absolute spelling and each hop its
// symlinks resolve through.
func checkProtected(path string, named bool, entries []protectedEntry) error {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrForbiddenMount, path, err)
	}
	candidates := mountPathChain(abs)
	for _, e := range entries {
		forms := pathForms(e.path)
		for _, c := range candidates {
			if err := judgeEntry(c, named, e, forms); err != nil {
				return err
			}
		}
	}
	for _, c := range candidates {
		if err := judgeSystem(c); err != nil {
			return err
		}
	}
	return nil
}

// judgeEntry judges one candidate form of a mount against one protected entry.
func judgeEntry(c string, named bool, e protectedEntry, forms []string) error {
	fc := foldPath(c)
	for _, form := range forms {
		ff := foldPath(form)
		switch e.kind {
		case protectedFile:
			if fc == ff || pathHolds(fc, ff) {
				return refuse(c, "holds "+e.what+" ("+form+"). "+e.fix)
			}
		case protectedDirFiles:
			if fc == ff || pathHolds(fc, ff) {
				return refuse(c, "holds "+e.what+" ("+form+"). "+e.fix)
			}
			if foldPath(filepath.Dir(c)) == ff && !isDir(c) {
				return refuse(c, "sits beside "+e.what+" in "+form+". "+e.besideFix)
			}
		case protectedTree:
			if pathHolds(fc, ff) {
				return refuse(c, "holds "+e.what+" ("+form+"). "+e.fix)
			}
			if named && (fc == ff || pathHolds(ff, fc)) {
				return refuse(c, "is inside "+e.what+" ("+form+"), which no run may name. Name a "+
					"directory outside it, or add the repository as a project.")
			}
		}
	}
	return nil
}

// isDir reports whether path is an existing directory. A path that does not exist is not one, so a
// file not yet written beside the database, such as a -wal sidecar, is refused before it appears.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// judgeSystem refuses a mount of a system directory taken whole, or of a directory holding one that
// is refused at any depth. /var/lib holds /var/lib/docker, and the server account's home holds .ssh,
// so neither may be mounted whole even though a directory inside either may.
func judgeSystem(c string) error {
	fc := foldPath(c)
	for _, w := range wholeMountDirs() {
		for _, form := range pathForms(w.path) {
			if fc == foldPath(form) {
				return refuse(c, "is "+w.what+". Name a directory inside it.")
			}
		}
	}
	for _, tree := range sensitiveMountTrees {
		if pathHolds(fc, tree) {
			return refuse(c, "holds "+tree+", which no run may mount. Name a directory inside it.")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for name := range sensitiveMountNames {
			for _, form := range pathForms(filepath.Join(home, name)) {
				if ff := foldPath(form); fc == ff || pathHolds(fc, ff) {
					return refuse(c, "holds "+form+", which no run may mount. Name a directory "+
						"inside it.")
				}
			}
		}
	}
	return nil
}

// wholeMount is a directory no mount may take whole, with the words its refusal uses.
type wholeMount struct {
	// path is the directory.
	path string
	// what names it in a refusal.
	what string
}

// wholeMountDirs returns the directories refused as whole mounts, looked up when a mount is checked
// so a changed home or temporary directory is the one judged.
func wholeMountDirs() []wholeMount {
	out := []wholeMount{
		{"/tmp", "the system temporary directory"},
		{"/var/tmp", "the system temporary directory"},
		{os.TempDir(), "the temporary directory"},
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, wholeMount{home, "the server account's home directory"},
			wholeMount{filepath.Join(home, ".cache"), "the server account's cache directory"})
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		out = append(out, wholeMount{cache, "the server account's cache directory"})
	}
	return out
}

// refuse builds the refusal for mount c.
func refuse(c, reason string) error {
	return fmt.Errorf("%w: %s %s", ErrForbiddenMount, c, reason)
}
