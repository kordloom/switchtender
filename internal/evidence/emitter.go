// Package evidence generates the period change register on a cadence and writes it where an
// auditor can find it, so the evidence a review samples from exists without anyone remembering to
// produce it. A pack nobody generated is the same as no pack on the day it is asked for.
package evidence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/run"
)

// stamp is the time layout in a pack's name. It is precise to the second because the cadence can be
// as short as an hour, and a name coarser than the cadence makes two periods the same file, so the
// second silently replaces the first.
const stamp = "20060102T150405Z"

// namePrefix and nameSuffix bracket the period a pack covers.
const (
	namePrefix = "change-register-"
	nameMiddle = "-to-"
	nameSuffix = ".html"
)

// Emitter writes a change register covering each elapsed period.
type Emitter struct {
	// runs reads the period's runs.
	runs run.Store
	// audits reads the chain the register is drawn from and verified against.
	audits audit.Store
	// installID is the install the tree profile's leaves bind to, needed to check a tree anchor.
	installID string
	// dir is where packs are written, and is also where the emitter reads its own progress from.
	dir string
	// cadence is how long each pack covers and how often one is written.
	cadence time.Duration
	// limit caps how many changes one pack carries. A period holding more is split into
	// consecutive packs rather than truncated, so the archive stays gap free.
	limit int
	// log records generation activity.
	log *zap.Logger
	// notify, when set, is called with the path of each pack written, so an operator hears about
	// it through whatever channel they already use.
	notify func(path string, from, to time.Time)
	// now reads the clock, replaced in tests.
	now func() time.Time
	// ctx and cancel stop the loop.
	ctx    context.Context
	cancel context.CancelFunc
	// wg waits for the loop to finish.
	wg sync.WaitGroup
}

// Option configures an Emitter.
type Option func(*Emitter)

// WithNotify calls fn with each written pack, so delivery rides the operator's existing channel
// rather than this package growing one.
func WithNotify(fn func(path string, from, to time.Time)) Option {
	return func(e *Emitter) { e.notify = fn }
}

// WithClock replaces the clock, so a test can advance periods without waiting for them.
func WithClock(now func() time.Time) Option {
	return func(e *Emitter) { e.now = now }
}

// WithMaxChanges caps how many changes one pack carries, overriding dossier.MaxRegisterRuns. A
// value that is not positive restores the default.
func WithMaxChanges(n int) Option {
	return func(e *Emitter) { e.limit = n }
}

// NewEmitter returns an emitter writing a pack per cadence into dir. It panics on a nil store, an
// empty directory, or a cadence under an hour, all of which are programming errors: a register
// covering minutes is not the artifact this exists to produce.
func NewEmitter(runs run.Store, audits audit.Store, installID, dir string, cadence time.Duration,
	log *zap.Logger, opts ...Option) *Emitter {
	if runs == nil || audits == nil {
		panic("evidence: run and audit stores required")
	}
	if dir == "" {
		panic("evidence: directory required")
	}
	if cadence < time.Hour {
		panic("evidence: cadence must be at least an hour")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Emitter{runs: runs, audits: audits, installID: installID, dir: dir, cadence: cadence,
		limit: dossier.MaxRegisterRuns, log: log, now: time.Now, ctx: ctx, cancel: cancel}
	for _, opt := range opts {
		opt(e)
	}
	if e.limit <= 0 {
		e.limit = dossier.MaxRegisterRuns
	}
	return e
}

// Start launches the loop and returns an error when the directory cannot be used.
//
// The directory is checked here rather than at the first tick, because with the documented
// quarterly cadence the first tick is three months after the misconfiguration, and the startup log
// would have claimed the archive was accumulating the whole time.
//
// Progress lives in the archive rather than in memory. The emitter resumes from the end of the
// newest pack it finds, so a restart does not reset the period, and a server restarted more often
// than its cadence still emits. That also means a failed write cannot be skipped past: nothing
// advanced, so the next attempt covers the same period plus what followed it.
func (e *Emitter) Start() error {
	if err := os.MkdirAll(e.dir, 0o750); err != nil {
		return fmt.Errorf("evidence directory: %w", err)
	}
	e.wg.Go(func() {
		// A short tick relative to the cadence, so a period that became due while the process was
		// down is picked up promptly after a restart rather than a whole cadence later.
		interval := e.cadence / 10
		if interval < time.Minute {
			interval = time.Minute
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		e.emitDue()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.emitDue()
			}
		}
	})
	return nil
}

// emitDue writes a pack when a full cadence has elapsed since the archive's newest one.
func (e *Emitter) emitDue() {
	now := e.now()
	last, err := e.resume(now)
	if err != nil {
		e.log.Error("evidence: read archive: " + err.Error())
		return
	}
	if now.Sub(last) < e.cadence {
		return
	}
	if err := e.Emit(e.ctx, last, now); err != nil {
		// Nothing advances on failure, so the next attempt covers this period again. The archive
		// is the bookkeeping, and it only moves when a pack actually lands.
		e.log.Error("evidence: write pack: " + err.Error())
	}
}

// resume returns the end of the newest pack in the archive, or now when the archive is empty, which
// starts the first period from the moment the feature was switched on rather than from the epoch.
func (e *Emitter) resume(now time.Time) (time.Time, error) {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		return time.Time{}, err
	}
	var ends []time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, to, ok := parsePeriod(entry.Name()); ok {
			ends = append(ends, to)
		}
	}
	if len(ends) == 0 {
		return now, nil
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i].Before(ends[j]) })
	return ends[len(ends)-1], nil
}

// parsePeriod reads the period a pack name covers, reporting whether the name is one of ours.
func parsePeriod(name string) (from, to time.Time, ok bool) {
	if !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, nameSuffix) {
		return time.Time{}, time.Time{}, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, namePrefix), nameSuffix)
	parts := strings.Split(body, nameMiddle)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, false
	}
	f, err := time.Parse(stamp, parts[0])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	t, err := time.Parse(stamp, parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return f, t, true
}

// packName is the file a period is written to. Two periods never share a name, so a pack never
// replaces one already in the archive and a missing period is a missing file.
func packName(from, to time.Time) string {
	return namePrefix + from.UTC().Format(stamp) + nameMiddle + to.UTC().Format(stamp) + nameSuffix
}

// Emit covers from until to, writing one pack per rendered register. A failure is returned rather
// than swallowed: an evidence archive with a silent hole in it is worse than one that is loudly
// incomplete, because only the second gets fixed.
//
// A period holding more changes than one register carries is split rather than truncated. Progress
// is read back out of pack names, so a pack named for the whole period while carrying part of it
// would move the archive past the changes it left out, permanently, while the directory still read
// as continuous. Each pack is therefore named for the range its document actually covers, and the
// remainder is written as the packs that follow it.
//
// Packs already written stay when a later one fails, so the archive advances to the end of the
// last pack that landed and the next attempt picks up from exactly there.
//
// It takes its own context rather than using the emitter's, so a caller generating a pack by hand
// is not canceled by an unrelated shutdown, and so Emit stays usable after Close.
func (e *Emitter) Emit(ctx context.Context, from, to time.Time) error {
	if err := os.MkdirAll(e.dir, 0o750); err != nil {
		return fmt.Errorf("evidence directory: %w", err)
	}
	for cur := from; ; {
		in, err := dossier.CollectRegister(ctx, e.runs, e.audits, e.installID, cur, to, e.now(),
			e.limit)
		if err != nil {
			return fmt.Errorf("collect register: %w", err)
		}
		// The boundary has to fall inside the remaining period for the split to mean anything, and
		// that is also what makes this loop finite. It fails to advance only when a register's
		// worth of changes share the period's first instant, and then no bounded document covers
		// them: the pack says so on its face and the error says so in the log, which is the loudly
		// incomplete case rather than the silent one.
		end, split := to, false
		if in.Truncated && in.CoveredTo.After(cur) && in.CoveredTo.Before(to) {
			end, split = in.CoveredTo, true
		}
		in.To = end
		if err := e.writePack(in, cur, end); err != nil {
			return err
		}
		if !split {
			if in.Truncated {
				return fmt.Errorf("pack %s is truncated: more than %d changes share %s, so the "+
					"archive has a gap after it", packName(cur, end), e.limit,
					in.CoveredTo.UTC().Format(time.RFC3339))
			}
			return nil
		}
		cur = end
	}
}

// writePack renders in and writes it as the pack covering from until to.
func (e *Emitter) writePack(in *dossier.RegisterInput, from, to time.Time) error {
	doc, err := dossier.RenderRegister(in)
	if err != nil {
		return fmt.Errorf("render register: %w", err)
	}
	path := filepath.Join(e.dir, packName(from, to))
	// Written whole or not at all. A pack truncated by a crash or a full disk reads as a present
	// period, which is the one way an archive lies without anything reporting it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, doc, 0o600); err != nil {
		return fmt.Errorf("write pack: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write pack: %w", err)
	}
	e.log.Info("evidence: wrote change register",
		zap.String("path", path), zap.Int("changes", len(in.Runs)),
		zap.Bool("chain_ok", in.ChainOK), zap.Int("anchor_problems", len(in.AnchorProblems)))
	if e.notify != nil {
		e.notify(path, from, to)
	}
	return nil
}

// Close stops the loop and waits for it.
func (e *Emitter) Close() {
	e.cancel()
	e.wg.Wait()
}
