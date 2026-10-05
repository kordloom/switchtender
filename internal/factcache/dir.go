package factcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// fileMode is the permission every cache file is written with: readable by the executor alone.
	fileMode fs.FileMode = 0o600
	// schemaPrefix starts the name ansible-core 2.19 and later give a host's cache file.
	schemaPrefix = "s1_"
	// payloadKey is the one member of a schema-prefixed cache file, the fact document as JSON text.
	payloadKey = "__payload__"
	// typeKey marks a value ansible-core wrote with its tags.
	typeKey = "__ansible_type"
)

// stamp is what a cache file looked like when it was written, so a rewrite can be told apart from
// a file Ansible only read.
type stamp struct {
	// modTime is the file's modification time after the write.
	modTime time.Time
	// size is the file's size after the write.
	size int64
	// sum is the SHA-256 of what was written.
	sum [sha256.Size]byte
}

// Dir is one run's cache directory: the facts written into it before the run and what Ansible
// changed in it afterward.
//
// Ansible has written this directory in two layouts. Through ansible-core 2.18 the jsonfile plugin
// keeps a host's facts in a file named after the host, holding the fact document itself. From 2.19
// it names the file with a schema prefix, s1_ and the host, and wraps the document as JSON text
// under a single __payload__ member, with values a play set carrying their provenance tags. A run
// cannot know which release will execute it, so every host is written in both layouts, each
// release reads its own and ignores the other, and whichever one the run rewrote is read back.
type Dir struct {
	// Path is the directory Ansible's jsonfile cache plugin is pointed at.
	Path string
	// written records every file put there before the run, keyed by file name.
	written map[string]stamp
	// hostOf names the host each written file holds facts for, keyed by file name.
	hostOf map[string]string
}

// Collected is what a run changed in its cache directory.
type Collected struct {
	// Updated are the hosts whose facts Ansible wrote during the run, with the new documents. Their
	// ModifiedAt is when Ansible wrote the file, by the file's modification time, and their
	// InventoryID and RunID are left for the caller to stamp.
	Updated []Entry
	// Cleared are the hosts whose cached file was written before the run and removed by it, which
	// is what a play's meta: clear_facts does.
	Cleared []string
	// Skipped names each file that was not taken, with the reason: a host outside the inventory,
	// a name that cannot be a host, a document past MaxFactsBytes, or one that is not a JSON object.
	Skipped []string
}

// NewDir creates a private cache directory under parent, or under the system temp directory when
// parent is empty. The caller removes it with Remove.
func NewDir(parent string) (*Dir, error) {
	path, err := os.MkdirTemp(parent, "switchtender-facts-*")
	if err != nil {
		return nil, fmt.Errorf("create fact cache directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("create fact cache directory: %w", err)
	}
	return &Dir{Path: path, written: make(map[string]stamp), hostOf: make(map[string]string)}, nil
}

// Remove deletes the directory and everything in it. The facts it held are secrets as much as
// anything a run materializes, so they do not outlive the run.
func (d *Dir) Remove() {
	if d != nil && d.Path != "" {
		_ = os.RemoveAll(d.Path)
	}
}

// Write puts each entry that is still fresh at now under timeout into the directory as the files
// Ansible's jsonfile plugin reads for that host, in both layouts, and returns how many hosts it
// wrote. An entry whose host cannot be a file name is skipped rather than written somewhere it
// should not go. The schema-prefixed file is left out for a host whose prefixed name is itself
// another host in the entries, so one host's facts never stand in for another's.
func (d *Dir) Write(entries []Entry, now time.Time, timeout time.Duration) (int, error) {
	hosts := make(map[string]bool, len(entries))
	for _, e := range entries {
		hosts[e.Host] = true
	}
	n := 0
	for _, e := range entries {
		if !ValidHost(e.Host) || len(e.Facts) == 0 || !Fresh(e.ModifiedAt, now, timeout) {
			continue
		}
		if err := d.writeFile(e.Host, e.Host, e.Facts); err != nil {
			return n, err
		}
		if wrapped := schemaPrefix + e.Host; ValidHost(wrapped) && !hosts[wrapped] {
			body, err := json.Marshal(map[string]string{payloadKey: string(e.Facts)})
			if err != nil {
				return n, fmt.Errorf("write cached facts for %s: %w", e.Host, err)
			}
			if err := d.writeFile(wrapped, e.Host, body); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// writeFile writes one cache file holding host's facts and records what it wrote.
func (d *Dir) writeFile(name, host string, body []byte) error {
	path := filepath.Join(d.Path, name)
	if err := os.WriteFile(path, body, fileMode); err != nil {
		return fmt.Errorf("write cached facts for %s: %w", host, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("write cached facts for %s: %w", host, err)
	}
	d.written[name] = stamp{modTime: info.ModTime(), size: info.Size(), sum: sha256.Sum256(body)}
	d.hostOf[name] = host
	return nil
}

// Collect reads the directory after the run and reports what changed. A file counts as new facts
// when it was not written before the run or when Ansible rewrote it, which a changed modification
// time, size, or content shows. allowed decides which hosts' facts are kept: the hosts the run's
// inventory names, so a play cannot plant facts for a host the inventory does not hold. When both
// layouts of one host changed, the schema-prefixed one is the newer release's and is kept.
func (d *Dir) Collect(allowed func(host string) bool) (Collected, error) {
	var out Collected
	files, err := os.ReadDir(d.Path)
	if err != nil {
		return out, fmt.Errorf("read fact cache directory: %w", err)
	}
	present := make(map[string]bool, len(files))
	updated := map[string]Entry{}
	fromWrapped := map[string]bool{}
	for _, f := range files {
		name := f.Name()
		present[name] = true
		if !f.Type().IsRegular() {
			out.Skipped = append(out.Skipped, name+": not a regular file")
			continue
		}
		if !ValidHost(name) {
			out.Skipped = append(out.Skipped, name+": not a host in this run's inventory")
			continue
		}
		data, wrote, changed, rerr := d.readChanged(name)
		switch {
		case errors.Is(rerr, ErrTooLarge):
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: facts larger than %d bytes",
				name, MaxFactsBytes))
			continue
		case rerr != nil:
			return out, rerr
		}
		host, doc, wrapped := name, data, false
		if h, payload, ok := unwrap(name, data); ok {
			host, doc, wrapped = h, payload, true
		}
		if !ValidHost(host) || !allowed(host) {
			out.Skipped = append(out.Skipped, name+": not a host in this run's inventory")
			continue
		}
		if !changed {
			continue
		}
		facts, ok := normalize(doc, wrapped)
		if !ok {
			out.Skipped = append(out.Skipped, name+": facts are not a JSON object")
			continue
		}
		if len(facts) > MaxFactsBytes {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: facts larger than %d bytes",
				name, MaxFactsBytes))
			continue
		}
		if _, seen := updated[host]; seen && fromWrapped[host] && !wrapped {
			continue
		}
		updated[host] = Entry{Host: host, Facts: facts, Bytes: len(facts), ModifiedAt: wrote}
		fromWrapped[host] = wrapped
	}
	for _, e := range updated {
		out.Updated = append(out.Updated, e)
	}
	sort.Slice(out.Updated, func(i, j int) bool { return out.Updated[i].Host < out.Updated[j].Host })
	cleared := map[string]bool{}
	for name, host := range d.hostOf {
		if _, kept := updated[host]; !present[name] && !kept {
			cleared[host] = true
		}
	}
	for host := range cleared {
		out.Cleared = append(out.Cleared, host)
	}
	sort.Strings(out.Cleared)
	sort.Strings(out.Skipped)
	return out, nil
}

// unwrap reads a cache file in the schema-prefixed layout, returning the host it is for and the
// fact document it wraps. A file that only looks prefixed, such as a host that happens to be named
// s1_db in the older layout, holds a fact document rather than the wrapper, and is reported as not
// wrapped.
func unwrap(name string, data []byte) (string, []byte, bool) {
	rest, ok := strings.CutPrefix(name, "s")
	if !ok {
		return "", nil, false
	}
	digits, host, ok := strings.Cut(rest, "_")
	if !ok || digits == "" || host == "" || strings.Trim(digits, "0123456789") != "" {
		return "", nil, false
	}
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(data, &wrapper) != nil || len(wrapper) != 1 {
		return "", nil, false
	}
	var payload string
	if json.Unmarshal(wrapper[payloadKey], &payload) != nil {
		return "", nil, false
	}
	return host, []byte(payload), true
}

// normalize returns a fact document in the stored form: compact JSON. A document from the
// schema-prefixed layout has each tagged value replaced by the value it carries, since the tags
// record where a play set it, which is the server's own file layout rather than a fact about the
// host, and the older layout reads plain values. It reports false for anything but a JSON object.
func normalize(doc []byte, wrapped bool) ([]byte, bool) {
	if !isObject(doc) {
		return nil, false
	}
	if !wrapped {
		var compact bytes.Buffer
		if err := json.Compact(&compact, doc); err != nil {
			return nil, false
		}
		return compact.Bytes(), true
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(untag(v))
	if err != nil {
		return nil, false
	}
	return out, true
}

// untag replaces every tagged value ansible-core writes, an object carrying __ansible_type and the
// value itself, with the value, throughout a decoded document.
func untag(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if _, typed := x[typeKey]; typed {
			if inner, ok := x["value"]; ok {
				return untag(inner)
			}
		}
		for k, child := range x {
			x[k] = untag(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = untag(child)
		}
		return x
	default:
		return v
	}
}

// readChanged reads one cache file and returns it with its modification time, which is when Ansible
// wrote it, and whether it differs from what was written before the run. A file larger than
// MaxFactsBytes is refused without being read whole.
func (d *Dir) readChanged(name string) ([]byte, time.Time, bool, error) {
	f, err := os.Open(filepath.Join(d.Path, name))
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("read cached facts for %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("read cached facts for %s: %w", name, err)
	}
	if info.Size() > MaxFactsBytes {
		return nil, time.Time{}, false, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFactsBytes+1))
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("read cached facts for %s: %w", name, err)
	}
	if len(data) > MaxFactsBytes {
		return nil, time.Time{}, false, ErrTooLarge
	}
	before, wrote := d.written[name]
	if !wrote {
		return data, info.ModTime(), true, nil
	}
	changed := !info.ModTime().Equal(before.modTime) || info.Size() != before.size ||
		sha256.Sum256(data) != before.sum
	return data, info.ModTime(), changed, nil
}
