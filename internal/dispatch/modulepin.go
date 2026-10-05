package dispatch

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// Where and for how long the gate keeps the module trees it downloads, so the run it judged
// executes exactly them. A plan held for approval can wait days, and the version constraint it
// names can resolve to a newer release in that time.
const (
	// moduleKeepDir names the directory under the run-files root that holds the kept trees.
	moduleKeepDir = "modules-kept"
	// DefaultModuleKeepFor is how long a kept tree is offered to the run it was read for, unless
	// WithModuleKeep says otherwise. Past it, the run downloads its modules again and is held to
	// the same digest.
	DefaultModuleKeepFor = 7 * 24 * time.Hour
	// DefaultModuleKeepMaxBytes bounds what the kept trees occupy, the oldest dropped first, unless
	// WithModuleKeep says otherwise.
	DefaultModuleKeepMaxBytes = 2 << 30
	// modulesDataDir is where the tool keeps what init and get install, relative to the working
	// directory, the layout the gate reads and the run is held to.
	modulesDataDir = ".terraform"
)

var (
	// errModulesUnpinned marks a module tree the gate cannot pin to a digest.
	errModulesUnpinned = errors.New("module tree cannot be pinned")
	// errModulesRefused marks a run refused because its modules cannot be shown to be the ones the
	// gate read.
	errModulesRefused = errors.New("refused")
	// vcsMetadata names the version control directories a download from a repository leaves beside
	// a module's files. They record how the copy was made, not what the module is, so two downloads
	// of the same commit differ in them, and the digest and the kept copy leave them out.
	vcsMetadata = map[string]bool{".git": true, ".hg": true, ".svn": true}
)

// moduleTreeDigest returns the digest of the module tree under dir: the path, kind, executable bit,
// and content of every entry, in a fixed order. Version control metadata is left out, and the
// manifest is read for the records it holds rather than how they were written. A link is recorded
// by where it points, and one that points outside dir is refused, since what it points at could
// change without the digest changing. So is anything that is not a file, a directory, or a link.
func moduleTreeDigest(dir string) (string, error) {
	h := sha256.New()
	err := walkModuleTree(dir, func(rel string, e fs.DirEntry, full string) error {
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "L %d:%s %d:%s\n", len(rel), rel, len(target), target)
		case e.IsDir():
			_, _ = fmt.Fprintf(h, "D %d:%s\n", len(rel), rel)
		default:
			return hashModuleFile(h, full, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// walkModuleTree calls visit for every entry under dir in lexical order, with its slash-separated
// path relative to dir, skipping version control metadata. It refuses a link that leaves dir and an
// entry that is not a file, a directory, or a link.
func walkModuleTree(dir string, visit func(rel string, e fs.DirEntry, full string) error) error {
	return filepath.WalkDir(dir, func(full string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if vcsMetadata[e.Name()] {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch t := e.Type(); {
		case t&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			if !linkStaysInside(rel, target) {
				return fmt.Errorf("%w: %s links to %s, outside the downloaded modules",
					errModulesUnpinned, rel, target)
			}
		case t.IsDir(), t.IsRegular():
		default:
			return fmt.Errorf("%w: %s is not a file, a directory, or a link", errModulesUnpinned, rel)
		}
		return visit(rel, e, full)
	})
}

// linkStaysInside reports whether a link at rel pointing at target resolves inside the tree rel is
// relative to, judged by the paths alone.
func linkStaysInside(rel, target string) bool {
	target = filepath.ToSlash(target)
	if path.IsAbs(target) || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(rel), target))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// hashModuleFile folds one file of a module tree into h: its path, whether it is executable, and
// its content, the manifest's in canonical form.
func hashModuleFile(h hash.Hash, full, rel string) error {
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	exec := info.Mode()&0o111 != 0
	if rel == "modules.json" {
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		content := canonicalManifest(raw)
		_, _ = fmt.Fprintf(h, "F %d:%s %t %d:", len(rel), rel, exec, len(content))
		_, _ = h.Write(content)
		return nil
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(h, "F %d:%s %t %d:", len(rel), rel, exec, info.Size())
	if _, err := io.Copy(h, io.LimitReader(f, info.Size())); err != nil {
		return err
	}
	return nil
}

// canonicalManifest returns the module manifest with its records in key order and its JSON
// re-encoded, so two downloads that recorded the same modules digest the same however the tool
// ordered or spaced them. A manifest that does not parse is digested as written.
func canonicalManifest(raw []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if list, ok := m["Modules"].([]any); ok {
		sort.SliceStable(list, func(i, j int) bool { return manifestKey(list[i]) < manifestKey(list[j]) })
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// manifestKey returns the key a manifest record names, empty for a record that names none.
func manifestKey(v any) string {
	rec, _ := v.(map[string]any)
	key, _ := rec["Key"].(string)
	return key
}

// moduleStore keeps the module trees the gate downloaded, each as an archive named by its digest,
// in a private directory. It is a cache in front of a check, never a substitute for one: a tree
// past its time, dropped for room, or kept on another machine is downloaded again by the run and
// held to the same digest, and a kept tree is offered only when it still has the digest it is named
// by.
type moduleStore struct {
	// dir holds the archives.
	dir string
	// ttl is how long an archive is offered after it was last kept.
	ttl time.Duration
	// maxBytes bounds what the archives occupy together.
	maxBytes int64
	// mu serializes keeping and sweeping.
	mu sync.Mutex
}

// newModuleStore returns a store keeping its archives under the run-files root, offering each for
// ttl and holding them all within maxBytes.
func newModuleStore(runFilesRoot string, ttl time.Duration, maxBytes int64) *moduleStore {
	return &moduleStore{dir: filepath.Join(runFilesRoot, moduleKeepDir), ttl: ttl,
		maxBytes: maxBytes}
}

// archivePath returns where the archive for digest lives, or false for a digest that is not one.
func (s *moduleStore) archivePath(digest string) (string, bool) {
	sum, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(sum) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", false
	}
	return filepath.Join(s.dir, sum+".tar.gz"), true
}

// keep archives the module tree at modules under digest, which the caller computed for it. Keeping
// a tree already kept only renews its time.
func (s *moduleStore) keep(digest, modules string) error {
	dest, ok := s.archivePath(digest)
	if !ok {
		return fmt.Errorf("%w: %q is not a module digest", errModulesUnpinned, digest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("keep modules: %w", err)
	}
	if info, err := os.Lstat(s.dir); err != nil || !info.IsDir() {
		return fmt.Errorf("keep modules: %s is not a directory", s.dir)
	}
	now := time.Now()
	if err := os.Chtimes(dest, now, now); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(s.dir, ".keep-*")
	if err != nil {
		return fmt.Errorf("keep modules: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := archiveModuleTree(tmp, modules); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("keep modules: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keep modules: %w", err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("keep modules: %w", err)
	}
	s.sweepLocked(now, dest)
	return nil
}

// archiveModuleTree writes the tree at modules to w as a gzip-compressed tar archive, leaving out
// what the digest leaves out.
func archiveModuleTree(w io.Writer, modules string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := walkModuleTree(modules, func(rel string, e fs.DirEntry, full string) error {
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeSymlink, Linkname: target,
				Mode: 0o777})
		case e.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: mode,
			Size: info.Size()}); err != nil {
			return err
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, io.LimitReader(f, info.Size()))
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// restore puts the tree kept under digest at workdir's .terraform/modules, replacing whatever is
// there, and reports whether it did. Every write stays inside workdir, whatever links the working
// directory holds. It reports false, and leaves nothing it extracted behind, when no current
// archive is kept for digest or what the archive holds does not have that digest.
func (s *moduleStore) restore(digest, workdir string) (bool, error) {
	src, ok := s.archivePath(digest)
	if !ok {
		return false, nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return false, nil
	}
	if time.Since(info.ModTime()) > s.ttl {
		return false, nil
	}
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return false, fmt.Errorf("restore modules: %w", err)
	}
	defer func() { _ = root.Close() }()
	modules := filepath.Join(modulesDataDir, "modules")
	if err := root.RemoveAll(modules); err != nil {
		return false, fmt.Errorf("restore modules: %w", err)
	}
	if err := extractModuleTree(root, src, modules); err != nil {
		_ = root.RemoveAll(modules)
		return false, fmt.Errorf("restore modules: %w", err)
	}
	got, err := moduleTreeDigest(filepath.Join(workdir, modules))
	if err != nil || got != digest {
		_ = root.RemoveAll(modules)
		_ = os.Remove(src)
		return false, fmt.Errorf("restore modules: the kept copy does not have the digest %s it is "+
			"kept under, so it was dropped", digest)
	}
	return true, nil
}

// extractModuleTree unpacks the archive at src into dest inside root, refusing any entry that would
// land outside dest or link outside the tree.
func extractModuleTree(root *os.Root, src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	if err := root.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("the kept copy names %q, outside the modules", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !linkStaysInside(rel, hdr.Linkname) {
				return fmt.Errorf("the kept copy links %s outside the modules", rel)
			}
			if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, io.LimitReader(tr, hdr.Size))
			if err := errors.Join(cerr, out.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the kept copy holds %q, which is not a file, a directory, or a link",
				hdr.Name)
		}
	}
}

// keptArchive is one archive the store holds, as a sweep weighs it.
type keptArchive struct {
	// path is where the archive is.
	path string
	// mod is when it was last kept.
	mod time.Time
	// size is how many bytes it occupies.
	size int64
}

// sweepLocked drops the archives past their time, then the oldest until what is left fits the
// bound, never the archive at just, which was kept a moment ago for a run about to need it. The
// caller holds s.mu.
func (s *moduleStore) sweepLocked(now time.Time, just string) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	var all []keptArchive
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(s.dir, e.Name())
		if now.Sub(info.ModTime()) > s.ttl {
			_ = os.Remove(p)
			continue
		}
		all = append(all, keptArchive{path: p, mod: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, k := range all {
		if total <= s.maxBytes {
			return
		}
		if k.path == just {
			continue
		}
		_ = os.Remove(k.path)
		total -= k.size
	}
}

// pinModules puts in the run's working directory exactly the module tree the gate downloaded and
// read before it judged r, and tells the runner that init installs no module, so the run cannot
// resolve a version again and execute something newer than what the gate read and an approver
// released. The tree comes from the copy the gate kept or, when that copy is gone, past its time or
// on another executor, from a fresh download that must have the same digest. A run whose modules
// cannot be shown to be the ones the gate read is refused, with why, before anything runs.
func (d *Dispatcher) pinModules(ctx context.Context, r *run.Run, spec *roundhouse.Spec, mask *masker,
	out io.Writer) error {
	want := r.ModulesDigest()
	if want == "" {
		return nil
	}
	refuse := func(why string) error {
		return fmt.Errorf("%w: %s", errModulesRefused, why)
	}
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "TF_DATA_DIR="); ok &&
			path.Clean(filepath.ToSlash(v)) != modulesDataDir {
			return refuse("the run sets TF_DATA_DIR, so its init would not find the modules the gate " +
				"read where the gate read them")
		}
	}
	workdir, err := roundhouse.WorkDir(spec.Dir, spec.Command)
	if err != nil {
		return refuse("the modules the gate read have nowhere to go: " + err.Error())
	}
	modules := filepath.Join(workdir, modulesDataDir, "modules")
	command := getCommand(r.Tool)
	source := "the copy the gate kept"
	restored, rerr := d.modules.restore(want, workdir)
	if rerr != nil {
		d.log.Warn("dispatch: "+rerr.Error(), zap.String("run_id", r.ID))
	}
	if !restored {
		fetcher, ok := d.runner.(roundhouse.ModuleFetcher)
		if !ok {
			return refuse("the copy of the modules the gate read is not kept on this executor, and this " +
				"executor cannot download them to check them")
		}
		_, _ = fmt.Fprintf(out, "The modules the gate read are not kept on this executor, so %s "+
			"downloads them again, and the run goes ahead only if they are the same.\n", command)
		// Whatever an earlier init left is cleared first, so the download installs every module
		// afresh, as the gate's did, rather than keeping a copy the gate never read.
		if root, rerr := os.OpenRoot(workdir); rerr == nil {
			_ = root.RemoveAll(filepath.Join(modulesDataDir, "modules"))
			_ = root.Close()
		}
		if err := os.MkdirAll(filepath.Join(workdir, modulesDataDir), 0o755); err != nil {
			return refuse("the modules could not be downloaded again: " + err.Error())
		}
		fetch := d.fetchBounded(ctx, r.ID, "this executor's", fetcher, *spec, workdir,
			filepath.Join(workdir, modulesDataDir), mask, out)
		if !fetch.Fetched() {
			return refuse("the copy of the modules the gate read is not kept on this executor, and " +
				"downloading them again did not complete: " + fetch.Error)
		}
		source = "a fresh download"
	}
	got, err := moduleTreeDigest(modules)
	if err != nil {
		return refuse("the modules this run would use cannot be checked against the ones the gate " +
			"read: " + err.Error())
	}
	if got != want {
		return refuse(fmt.Sprintf("the modules this run would use are not the ones the gate read "+
			"when it was submitted: the gate read %s, and %s holds %s. A module changed after the "+
			"gate read it, so this run would execute code nobody reviewed. Submit it again so the "+
			"gate reads the current modules", want, source, got))
	}
	_, _ = fmt.Fprintf(out, "Using the modules the gate read, %s, from %s.\n", want, source)
	spec.ModulesInstalled = true
	return nil
}
