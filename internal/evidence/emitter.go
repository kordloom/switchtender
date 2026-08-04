// Package evidence generates the period change register on a cadence and writes it where an
// auditor can find it, so the evidence a review samples from exists without anyone remembering to
// produce it. A pack nobody generated is the same as no pack on the day it is asked for.
package evidence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/run"
)

// Emitter writes a change register covering each elapsed period.
type Emitter struct {
	// runs reads the period's runs.
	runs run.Store
	// audits reads the chain the register is drawn from and verified against.
	audits audit.Store
	// dir is where packs are written.
	dir string
	// cadence is how long each pack covers and how often one is written.
	cadence time.Duration
	// log records generation activity.
	log *zap.Logger
	// notify, when set, is called with the path of each pack written, so an operator hears about
	// it through whatever channel they already use.
	notify func(path string, from, to time.Time)
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

// NewEmitter returns an emitter writing a pack per cadence into dir. It panics on a nil store or a
// cadence under an hour, both of which are programming errors: a register covering minutes is not
// the artifact this exists to produce.
func NewEmitter(runs run.Store, audits audit.Store, dir string, cadence time.Duration,
	log *zap.Logger, opts ...Option) *Emitter {
	if runs == nil || audits == nil {
		panic("evidence: run and audit stores required")
	}
	if cadence < time.Hour {
		panic("evidence: cadence must be at least an hour")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Emitter{runs: runs, audits: audits, dir: dir, cadence: cadence, log: log,
		ctx: ctx, cancel: cancel}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Start launches the loop, writing a pack per cadence until Close.
//
// It does not write one immediately. A pack covers a period, and a restart is not the end of one:
// emitting on every boot would fill the directory with overlapping fragments during a deploy and
// make the archive read as though changes happened in periods that never elapsed.
func (e *Emitter) Start() {
	e.wg.Go(func() {
		ticker := time.NewTicker(e.cadence)
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-e.ctx.Done():
				return
			case now := <-ticker.C:
				if err := e.Emit(last, now); err != nil {
					e.log.Error("evidence: write pack: " + err.Error())
				}
				last = now
			}
		}
	})
}

// Emit writes one pack covering from until to, and returns its path. A failure is returned rather
// than swallowed: an evidence archive with a silent hole in it is worse than one that is loudly
// incomplete, because only the second gets fixed.
func (e *Emitter) Emit(from, to time.Time) error {
	if e.dir != "" {
		if err := os.MkdirAll(e.dir, 0o750); err != nil {
			return fmt.Errorf("evidence directory: %w", err)
		}
	}
	in, err := dossier.CollectRegister(e.ctx, e.runs, e.audits, from, to, time.Now())
	if err != nil {
		return fmt.Errorf("collect register: %w", err)
	}
	doc, err := dossier.RenderRegister(in)
	if err != nil {
		return fmt.Errorf("render register: %w", err)
	}
	// Named by the period it covers, so an archive sorts chronologically and a gap is visible as a
	// missing file rather than having to be inferred from contents.
	name := fmt.Sprintf("change-register-%s-to-%s.html",
		from.UTC().Format("20060102"), to.UTC().Format("20060102"))
	path := filepath.Join(e.dir, name)
	if err := os.WriteFile(path, doc, 0o600); err != nil {
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
