package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
	"github.com/kordloom/switchtender/internal/tfscan"
	"github.com/kordloom/switchtender/internal/util"
)

// Bounds on the module download the gate runs before it reads a plan's configuration. The download
// runs on the submit path, so a configuration must not be able to make it slow or large: past any
// of these it is stopped, and the plan is left unclassified with the reason named.
const (
	// DefaultModuleFetchTimeout bounds how long one download may run.
	DefaultModuleFetchTimeout = 2 * time.Minute
	// DefaultModuleFetchMaxBytes bounds what one download may write, the copied configuration
	// included.
	DefaultModuleFetchMaxBytes = 512 << 20
	// moduleFetchMaxFiles bounds how many files and directories one download may write.
	moduleFetchMaxFiles = 50000
	// moduleFetchPoll is how often a running download's directory is measured.
	moduleFetchPoll = 250 * time.Millisecond
	// moduleFetchOutput bounds how much of a download's output is kept to say why it failed.
	moduleFetchOutput = 64 << 10
	// maxFetchReason bounds how much of a download's own error a record repeats.
	maxFetchReason = 200
	// fetchedScanTTL is how long a scan a completed download fed is reused for the same
	// configuration, so the several rule checks of one submission download once.
	fetchedScanTTL = 2 * time.Minute
	// failedScanTTL is how long a scan a failed download fed is reused, short so a passing outage
	// holds the next submission no longer than it has to.
	failedScanTTL = 15 * time.Second
	// planScanEntries bounds how many scans are kept for reuse.
	planScanEntries = 64
)

var (
	// errFetchTimeout is the cause a download stopped for running too long.
	errFetchTimeout = errors.New("module download timed out")
	// errFetchTooLarge is the cause a download stopped for writing too much.
	errFetchTooLarge = errors.New("module download too large")
	// urlUserinfo matches the credentials a URL may carry before its host.
	urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)
)

// WithModuleKeep sets how long the module trees the gate downloads are kept for the run it judged,
// and how much the kept trees may occupy together, the oldest dropped first. A run whose tree is no
// longer kept downloads its modules again and is held to the digest the gate read. Zero keeps the
// default.
func WithModuleKeep(keepFor time.Duration, maxBytes int64) Option {
	return func(c *config) {
		c.moduleKeepFor = keepFor
		c.moduleKeepMaxBytes = maxBytes
	}
}

// WithModuleFetchLimits bounds the module download the gate runs before it reads a plan's
// configuration: how long it may run and how much it may write. Zero keeps the default.
func WithModuleFetchLimits(timeout time.Duration, maxBytes int64) Option {
	return func(c *config) {
		c.moduleFetchTimeout = timeout
		c.moduleFetchMaxBytes = maxBytes
	}
}

// planFetcher returns what downloads the modules of r's configuration for the gate, or why nothing
// can, worded as the reason a module stays unread.
//
// The download runs where the plan itself runs and nowhere else: this server's runner, for a plan
// this server executes. A plan routed to a worker runs with the worker's network, which this server
// does not share, so its modules are not downloaded here, where the download would reach what the
// plan could not.
func (d *Dispatcher) planFetcher(r *run.Run) (roundhouse.ModuleFetcher, string) {
	fetcher, ok := d.runner.(roundhouse.ModuleFetcher)
	switch {
	case !ok:
		return nil, "not downloaded, since this server's runner cannot download modules for the gate"
	case !slices.Contains(d.queues, r.Queue):
		return nil, fmt.Sprintf("not downloaded, since the plan runs on a worker for queue %q and "+
			"the gate downloads modules only where the plan itself runs", r.Queue)
	}
	return fetcher, ""
}

// scanPlan reads the Terraform or OpenTofu configuration a dry run of r plans, the way the gate
// reads it, and downloads the registry and remote modules it calls first when that is what stands
// between the plan and a classification.
//
// A fresh commit holds no downloaded modules, so without the download almost every configuration
// calling a registry module stayed unclassified. The download is the tool's own get, run in a
// private copy of what the scan read, with the credentials, network, binary, and image the plan
// itself would use. It installs modules and nothing else: no provider is installed and no program a
// configuration names runs. A download that fails, runs too long, writes too much, or cannot run
// leaves the plan unclassified, with the reason in the record.
func (d *Dispatcher) scanPlan(r *run.Run, checkouts CheckoutReader) run.DryRunScan {
	return d.scanPlanFor(r, checkouts, federation.PurposeGateDownload)
}

// precheckPlan is scanPlan for a pull request review's pre-check, which asks before any run exists,
// so a federated token the download mints says so rather than naming a run.
func (d *Dispatcher) precheckPlan(r *run.Run, checkouts CheckoutReader) run.DryRunScan {
	return d.scanPlanFor(r, checkouts, federation.PurposeReviewPrecheck)
}

// scanPlanFor is scanPlan with the purpose a federated token minted for the download names.
func (d *Dispatcher) scanPlanFor(r *run.Run, checkouts CheckoutReader, purpose string) run.DryRunScan {
	fetcher, why := d.planFetcher(r)
	opts := tfscan.Options{Tool: r.Tool, NotDownloaded: why}
	if r.ProjectID == "" {
		return d.scanLocalPlan(r, fetcher, opts, purpose)
	}
	if checkouts == nil {
		return unreadConfiguration(r, noCheckout)
	}
	opts.Place = "the project"
	dir := projectDir(r.Command)
	commit, which := commitToRead(r)
	var out run.DryRunScan
	var staged *stagedPlan
	read := false
	err := checkouts.ReadCommit(r.ProjectID, commit, func(fsys fs.FS, sha string) error {
		read = true
		var needs tfscan.Needs
		out, needs = tfscan.ScanNeeds(fsys, dir, opts)
		out.Source = readAt(sha, which)
		// The copy is made here, while the project's lock holds the commit still, and the download
		// runs after the lock is released, so a slow registry never holds up a sync.
		if fetcher != nil && fetchHelps(out, needs) {
			staged = d.stagePlan(r, fsys, dir, out.Inputs)
		}
		return nil
	})
	if err != nil || !read {
		staged.remove()
		return unreadConfiguration(r, "the project's checkout could not be read")
	}
	if staged == nil {
		return out
	}
	opts.NotDownloaded = ""
	return d.fetchAndScan(r, fetcher, staged, dir, opts, out, purpose)
}

// scanLocalPlan is scanPlan for a configuration named by a working directory on this server.
func (d *Dispatcher) scanLocalPlan(r *run.Run, fetcher roundhouse.ModuleFetcher, opts tfscan.Options,
	purpose string) run.DryRunScan {
	root, _, err := openLocal(r.Command, false)
	if err != nil {
		return unreadConfiguration(r, unreadWhy(err, "not on this server"))
	}
	defer func() { _ = root.Close() }()
	opts.Place = "the working directory"
	out, needs := tfscan.ScanNeeds(root.FS(), ".", opts)
	if fetcher == nil || !fetchHelps(out, needs) {
		return out
	}
	opts.NotDownloaded = ""
	return d.fetchAndScan(r, fetcher, d.stagePlan(r, root.FS(), ".", out.Inputs), ".", opts, out,
		purpose)
}

// fetchHelps reports whether downloading a configuration's modules could change a scan's answer:
// it found nothing running a program, and what it could not read is only modules with no current
// downloaded copy. A scan that already found something, or could not read something no download
// supplies, keeps its answer, and nothing is downloaded for it.
func fetchHelps(scan run.DryRunScan, needs tfscan.Needs) bool {
	return len(scan.Findings) == 0 && needs.Downloads > 0 && needs.Other == 0
}

// stagedPlan is a private copy of the files a scan read, which the gate downloads modules into.
type stagedPlan struct {
	// dir is the private run-files directory holding the copy, removed with everything in it.
	dir *runfiles.Dir
	// tree is the copy's root, where each file sits at the path the scan read it from.
	tree string
	// digest covers every copied file's path and content, so a reused scan is one of the same files.
	digest string
	// err says why the copy could not be made, nil when it was.
	err error
}

// stagePlan copies the files a scan read out of fsys into a new private run-files directory, at the
// paths the scan read them from, leaving out whatever an earlier init installed under the
// configuration directory dir's .terraform, so the download installs every module afresh and the
// tree it leaves is whole. The scan's bounds bound the copy. A copy that fails partway is reported
// on the staged plan rather than half used.
func (d *Dispatcher) stagePlan(r *run.Run, fsys fs.FS, dir string, inputs []string) *stagedPlan {
	files, err := runfiles.Create(d.runFiles(), "modules-"+r.ID)
	if err != nil {
		return &stagedPlan{err: err}
	}
	staged := &stagedPlan{dir: files, tree: filepath.Join(files.Path(), "tree")}
	installed := path.Join(dir, modulesDataDir) + "/"
	h := sha256.New()
	for _, name := range inputs {
		if strings.HasPrefix(name, installed) {
			continue
		}
		if err := copyStaged(fsys, name, staged.tree, h); err != nil {
			staged.err = err
			return staged
		}
	}
	staged.digest = hex.EncodeToString(h.Sum(nil))
	return staged
}

// copyStaged copies one file from fsys into tree at the same path, folding its path and content
// into h.
func copyStaged(fsys fs.FS, name, tree string, h io.Writer) error {
	if !fs.ValidPath(name) {
		return fmt.Errorf("%w: %q is not a path inside the configuration", fs.ErrInvalid, name)
	}
	content, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	dest := filepath.Join(tree, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(h, "%d:%s\x00%d:", len(name), name, len(content))
	_, _ = h.Write(content)
	return nil
}

// remove deletes the copy and everything downloaded into it. It is safe on a nil or failed copy.
func (s *stagedPlan) remove() {
	if s == nil || s.dir == nil {
		return
	}
	_ = s.dir.Remove()
}

// fetchAndScan downloads the modules of the staged configuration and scans what it installed. The
// staged copy is always removed. pre is the scan of the configuration before the download, whose
// source the result keeps.
func (d *Dispatcher) fetchAndScan(r *run.Run, fetcher roundhouse.ModuleFetcher, staged *stagedPlan, dir string,
	opts tfscan.Options, pre run.DryRunScan, purpose string) run.DryRunScan {
	defer staged.remove()
	if staged.err != nil {
		reason := "the gate could not copy the configuration to download its modules: " +
			redactFetchText(nil, staged.err.Error())
		pre.Fetch = &run.ModuleFetch{Command: getCommand(r.Tool), ExitStatus: -1, Error: reason}
		return pre
	}
	key := d.planScanKey(r, dir, staged.digest)
	if cached, ok := d.planScans.get(key); ok {
		cached.Source = pre.Source
		return cached
	}
	fetch := d.runFetch(r, fetcher, staged, dir, purpose)
	if fetch.Fetched() {
		fetch = d.pinFetched(r, fetch, filepath.Join(staged.tree, filepath.FromSlash(dir),
			modulesDataDir, "modules"))
	}
	ttl := fetchedScanTTL
	if !fetch.Fetched() {
		ttl = failedScanTTL
		// Whatever a failed download left half installed is not read: the run's own init would
		// install the configuration's modules afresh, and a partial copy is not the one it installs.
		_ = os.RemoveAll(filepath.Join(staged.tree, filepath.FromSlash(dir), ".terraform"))
		opts.NotDownloaded = "not downloaded, since " + fetch.Error
	}
	// The tree under staged.tree is the gate's own: the committed .terraform was left out when it
	// was staged, and the download installed every module afresh into it, so its manifest and module
	// copies are the ones the gate stands behind and may be read.
	opts.TrustModuleManifest = true
	out := tfscan.Scan(os.DirFS(staged.tree), dir, opts)
	out.Source = pre.Source
	out.Fetch = fetch
	d.planScans.put(key, out, ttl)
	return out
}

// pinFetched records the digest of the module tree a completed download installed at modules, and
// keeps a copy for the run, so the run executes exactly what the scan is about to read. A tree that
// cannot be pinned to a digest, such as one linking outside itself, is a download the gate cannot
// stand behind, so it is reported as not completed, and the scan fails closed. A copy that cannot
// be kept costs nothing but a second download when the run starts, which is checked the same way.
func (d *Dispatcher) pinFetched(r *run.Run, fetch *run.ModuleFetch, modules string) *run.ModuleFetch {
	digest, err := moduleTreeDigest(modules)
	if err != nil {
		reason := redactFetchText(nil, "the gate cannot pin the modules its "+fetch.Command+
			" installed: "+err.Error())
		d.log.Warn("dispatch: "+reason, zap.String("run_id", r.ID))
		return &run.ModuleFetch{Command: fetch.Command, ExitStatus: fetch.ExitStatus, Error: reason}
	}
	if err := d.modules.keep(digest, modules); err != nil {
		d.log.Warn("dispatch: the gate could not keep the modules it read, so the run downloads "+
			"them again: "+err.Error(), zap.String("run_id", r.ID))
	}
	pinned := *fetch
	pinned.ModulesDigest = digest
	return &pinned
}

// runFetch runs the download for a staged configuration and reports how it went. It opens the run's
// credentials for the download alone, the way execution opens them, and removes them and revokes
// anything they minted once it returns.
func (d *Dispatcher) runFetch(r *run.Run, fetcher roundhouse.ModuleFetcher, staged *stagedPlan,
	dir, purpose string) *run.ModuleFetch {
	command := getCommand(r.Tool)
	fail := func(mask *masker, format string, args ...any) *run.ModuleFetch {
		reason := redactFetchText(mask, fmt.Sprintf(format, args...))
		d.log.Warn("dispatch: the gate could not download modules: "+reason,
			zap.String("run_id", r.ID))
		return &run.ModuleFetch{Command: command, ExitStatus: -1, Error: reason}
	}
	// Preparing the image and opening the credentials fall under the same bound as the download, so
	// a credential source that never answers cannot hold up a submission either.
	ctx, cancel := context.WithTimeoutCause(context.Background(), d.moduleFetchTimeout,
		errFetchTimeout)
	defer cancel()
	ctx = withMintPurpose(ctx, purpose)

	spec := roundhouse.Spec{Tool: r.Tool, Command: dir, Dir: staged.tree, DryRun: true}
	imageCleanup, err := d.fetchImage(ctx, r, &spec)
	defer imageCleanup()
	if err != nil {
		return fail(nil, "the gate could not prepare the image the plan runs in for its %s: %v",
			command, err)
	}
	materialized, secrets, err := d.materializeCredentials(ctx, r, &spec)
	credCleanup := sync.OnceFunc(materialized)
	defer credCleanup()
	mask := &masker{}
	mask.set(append(secrets, registrySecrets(&spec)...))
	if err != nil {
		return fail(mask, "the gate could not open the run's credentials for its %s: %v", command,
			err)
	}
	workdir := filepath.Join(staged.tree, filepath.FromSlash(dir))
	return d.fetchBounded(ctx, r.ID, "the gate's", fetcher, spec, workdir, staged.dir.Path(), mask,
		nil)
}

// fetchBounded runs fetcher's get for spec in workdir within the dispatcher's time and size bounds,
// and reports how it went, naming the get as whose it is, such as "the gate's". spec already
// carries the credentials the get runs with, and mask their values, so nothing the get prints
// reaches a record unmasked. measure is the directory the size bound weighs. The get's output also
// goes to out when out is not nil.
//
// The download writes where the scan reads and where the size bound looks, whatever the environment
// would otherwise say, and it never stops to ask anything. The data directory is named relative to
// the working directory, as the tool's default is, so the manifest records the relative directories
// the scan reads and the run's own init finds.
func (d *Dispatcher) fetchBounded(ctx context.Context, runID, whose string, fetcher roundhouse.ModuleFetcher,
	spec roundhouse.Spec, workdir, measure string, mask *masker, out io.Writer) *run.ModuleFetch {
	command := getCommand(spec.Tool)
	get := whose + " " + command
	fail := func(format string, args ...any) *run.ModuleFetch {
		reason := redactFetchText(mask, fmt.Sprintf(format, args...))
		d.log.Warn("dispatch: the module download did not complete: "+reason,
			zap.String("run_id", runID))
		return &run.ModuleFetch{Command: command, ExitStatus: -1, Error: reason}
	}
	timeout, maxBytes := d.moduleFetchTimeout, d.moduleFetchMaxBytes
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errFetchTimeout)
	defer cancel()
	tmp := filepath.Join(workdir, ".switchtender-tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fail("%s could not be prepared: %v", get, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	spec.Env = append(slices.Clip(spec.Env), "TF_DATA_DIR="+modulesDataDir, "TMPDIR="+tmp,
		"TF_INPUT=0", "TF_IN_AUTOMATION=1")

	watched, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchFetchSize(watched, measure, maxBytes, stop)
	}()
	output := &cappedBuffer{cap: moduleFetchOutput}
	var w io.Writer = output
	if out != nil {
		w = io.MultiWriter(output, out)
	}
	res, runErr := fetcher.FetchModules(watched, spec, w)
	stop(nil)
	<-done

	tooLarge := errors.Is(context.Cause(watched), errFetchTooLarge) ||
		overFetchBounds(measure, maxBytes)
	switch {
	case tooLarge:
		return fail("%s wrote more than %d MiB or %d files", get, maxBytes>>20, moduleFetchMaxFiles)
	case errors.Is(context.Cause(watched), errFetchTimeout):
		return fail("%s did not finish within %s", get, timeout)
	case runErr != nil:
		return fail("%s could not run: %v", get, runErr)
	case res.ExitCode != 0:
		return &run.ModuleFetch{Command: command, ExitStatus: res.ExitCode,
			Error: redactFetchText(mask, fmt.Sprintf("%s failed with exit status %d%s", get,
				res.ExitCode, fetchErrorLine(output.String())))}
	}
	return &run.ModuleFetch{Command: command, ExitStatus: 0}
}

// fetchImage puts on spec the image a run of r executes in, resolved the way execution resolves it,
// with its pull login: the image pinned onto the run when it was submitted, which already took its
// project's or the server's default when the run named none, else this executor's own default. It
// returns a cleanup for the login, always safe to call.
func (d *Dispatcher) fetchImage(ctx context.Context, r *run.Run, spec *roundhouse.Spec) (func(), error) {
	image, pull := r.Image, r.PullCredentialID
	if image == "" {
		image, pull = d.defaultImage, ""
	}
	spec.Image = image
	if image == "" {
		return func() {}, nil
	}
	return d.resolvePullCredential(ctx, pull, spec)
}

// watchFetchSize measures the directory a download writes into until ctx ends, and stops the
// download through stop the moment it holds more than maxBytes or more than moduleFetchMaxFiles
// entries.
func watchFetchSize(ctx context.Context, root string, maxBytes int64, stop context.CancelCauseFunc) {
	ticker := time.NewTicker(moduleFetchPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if overFetchBounds(root, maxBytes) {
				stop(errFetchTooLarge)
				return
			}
		}
	}
}

// overFetchBounds reports whether root holds more than maxBytes, or more than moduleFetchMaxFiles
// entries. It stops walking as soon as either bound is passed, so measuring a huge tree costs no
// more than measuring one at the bound.
func overFetchBounds(root string, maxBytes int64) bool {
	var size int64
	entries := 0
	errOver := errors.New("over")
	err := filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		entries++
		if !e.IsDir() {
			if info, ierr := e.Info(); ierr == nil {
				size += info.Size()
			}
		}
		if entries > moduleFetchMaxFiles || size > maxBytes {
			return errOver
		}
		return nil
	})
	return errors.Is(err, errOver)
}

// fetchErrorLine returns the first error a download printed, as ": Error: ..." for a record, or the
// empty string when it printed none.
func fetchErrorLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if i := strings.Index(line, "Error: "); i >= 0 {
			return ": " + strings.TrimSpace(line[i:])
		}
	}
	return ""
}

// redactFetchText makes a reason safe to record: every secret the download was handed is masked,
// any credentials a URL carries are removed, and the result is bounded.
func redactFetchText(mask *masker, text string) string {
	if mask != nil {
		text = mask.redactString(text)
	}
	text = urlUserinfo.ReplaceAllString(text, "${1}")
	return util.Clip(util.SafeText(text), maxFetchReason)
}

// getCommand names the download the gate runs for tool.
func getCommand(tool string) string {
	if run.NormalizeTool(tool) == run.ToolOpenTofu {
		return "tofu get"
	}
	return "terraform get"
}

// planScanKey is what a scan a download fed is reused under: everything that decides what the
// download installs. That is the files the scan read, the tool, where the plan runs, and the
// credentials it downloads with.
func (d *Dispatcher) planScanKey(r *run.Run, dir, digest string) string {
	creds := append([]string(nil), d.effectiveCredentialIDs(context.Background(), r)...)
	sort.Strings(creds)
	parts := []string{run.NormalizeTool(r.Tool), r.ProjectID, path.Clean(dir), r.Queue, r.Image,
		r.PullCredentialID, strings.Join(creds, ","), digest}
	if r.ProjectID == "" {
		parts = append(parts, r.Command)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// planScanCache keeps the scans recent downloads fed, so the several rule checks one submission
// makes, and the probe a pull request review makes first, download once.
type planScanCache struct {
	// mu guards entries.
	mu sync.Mutex
	// entries holds each kept scan by key.
	entries map[string]planScanEntry
}

// planScanEntry is one kept scan.
type planScanEntry struct {
	// scan is the scan.
	scan run.DryRunScan
	// expires is when the scan stops being reused.
	expires time.Time
}

// newPlanScanCache returns an empty cache.
func newPlanScanCache() *planScanCache {
	return &planScanCache{entries: map[string]planScanEntry{}}
}

// get returns the scan kept under key when there is one still current.
func (c *planScanCache) get(key string) (run.DryRunScan, bool) {
	if c == nil {
		return run.DryRunScan{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return run.DryRunScan{}, false
	}
	return e.scan.Clone(), true
}

// put keeps scan under key for ttl, dropping what has expired and, when the cache is still full,
// the entry closest to expiring.
func (c *planScanCache) put(key string, scan run.DryRunScan, ttl time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= planScanEntries {
		oldest := ""
		for k, e := range c.entries {
			if oldest == "" || e.expires.Before(c.entries[oldest].expires) {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = planScanEntry{scan: scan, expires: now.Add(ttl)}
}
