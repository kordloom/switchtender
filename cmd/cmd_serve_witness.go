package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/witnessclient"
	"github.com/kordloom/switchtender/spanbeat"
)

// serveWitnessEnrollmentUsage is the help text of the --witness-enrollment flag.
const serveWitnessEnrollmentUsage = "Enrollment file to push each span beat to its witness " +
	"under. Empty reads " + witnessclient.EnrollmentFile + " beside the license file, when there " +
	"is one. An enrolled install needs --span-cadence."

// serveWitnessEnrollment is the --witness-enrollment flag: the enrollment file serve pushes each
// span beat under. Empty reads the one beside the license file, when there is one.
var serveWitnessEnrollment string

// serveEnrollment loads the enrollment serve pushes under: the file flagPath names, or
// witness-enrollment.json beside the license file at licensePath when flagPath is empty. A named
// file that is missing or out of form is an error, a missing file beside the license is an install
// that never enrolled, which returns nil, and a broken one there is an error. It returns the path it
// read or looked for.
func serveEnrollment(flagPath, licensePath string) (*witnessclient.Enrollment, string, error) {
	if flagPath != "" {
		enr, err := witnessclient.Load(flagPath)
		if err != nil {
			return nil, flagPath, fmt.Errorf("--witness-enrollment: %w", err)
		}
		return enr, flagPath, nil
	}
	path := witnessclient.PathFor(licensePath)
	enr, err := witnessclient.LoadIfPresent(path)
	if err != nil {
		return nil, path, fmt.Errorf("%w. Fix or remove it to start without a witness", err)
	}
	return enr, path, nil
}

// serveWitnessClient returns the client serve pushes each span beat to its witness through, or nil
// when the install is not enrolled. The enrollment is the file flagPath names, or
// witness-enrollment.json beside the license file at licensePath when flagPath is empty. A named
// file that is missing is an error, and a missing file beside the license is an install that never
// enrolled, which changes nothing.
//
// An enrolled install that appends no span beats, or has no producer identity to sign its pushes
// with, would run unwitnessed while its enrollment says otherwise, so either refuses the start. The
// startup line names the witness key id and the client's status.
func serveWitnessClient(flagPath, licensePath string, cadence time.Duration, producer *audit.Identity,
	audits audit.Store, log *zap.Logger) (*witnessclient.Client, error) {
	enr, path, err := serveEnrollment(flagPath, licensePath)
	if err != nil {
		return nil, err
	}
	if enr == nil {
		return nil, nil
	}
	if cadence <= 0 {
		return nil, fmt.Errorf("this install is enrolled with a witness in %s, and --span-cadence is "+
			"not set. The witness countersigns span beats, so with none it would witness nothing "+
			"while the enrollment says it does. Set --span-cadence, for example 60s, or remove %s "+
			"to start without a witness", path, path)
	}
	if producer == nil {
		return nil, fmt.Errorf("this install is enrolled with a witness in %s, and it has no "+
			"producer identity to sign its pushes with, so the start is refused. Restore the "+
			"producer key, or remove %s to start without a witness", path, path)
	}
	attestations, ok := audits.(audit.AttestationStore)
	if !ok {
		return nil, fmt.Errorf("this install is enrolled with a witness in %s, and its store keeps "+
			"no witness countersignatures, so the start is refused", path)
	}
	c, err := witnessclient.New(path, *producer, audits, attestations, cadence, log)
	if err != nil {
		return nil, err
	}
	st := c.Status()
	log.Info("span beats will be pushed to the witness: "+st.Line(),
		zap.String("witness_key_id", st.WitnessKeyID))
	return c, nil
}

// beatHooks returns the hook serve runs on every appended span beat: the timestamp authority's
// anchor tsa and the witness push of c, each run independently, so a witness outage never blocks
// an anchor and the reverse. Either may be absent, and with neither it returns nil.
func beatHooks(tsa spanbeat.AnchorFunc, c *witnessclient.Client) spanbeat.AnchorFunc {
	return chainAnchorFuncs(tsa, witnessBeat(c))
}

// witnessBeat returns the push hook of c, or nil when c is nil.
func witnessBeat(c *witnessclient.Client) spanbeat.AnchorFunc {
	if c == nil {
		return nil
	}
	return c.Beat
}

// chainAnchorFuncs returns a hook that runs every non-nil hook in fns on each beat, each in its own
// goroutine, and waits for all of them, so one that fails or hangs never keeps another from running
// or from recording what it fixes. Its error joins every hook's error. It returns nil when no hook
// is set and the one hook unchanged when only one is.
func chainAnchorFuncs(fns ...spanbeat.AnchorFunc) spanbeat.AnchorFunc {
	set := slices.DeleteFunc(slices.Clone(fns), func(f spanbeat.AnchorFunc) bool { return f == nil })
	switch len(set) {
	case 0:
		return nil
	case 1:
		return set[0]
	}
	return func(ctx context.Context, b spanbeat.AppendedBeat) error {
		errs := make([]error, len(set))
		var wg sync.WaitGroup
		for i, f := range set {
			wg.Go(func() { errs[i] = f(ctx, b) })
		}
		wg.Wait()
		return errors.Join(errs...)
	}
}

// witnessOption returns the server option that serves c's status, an install that is not enrolled
// when c is nil.
func witnessOption(c *witnessclient.Client) server.Option {
	if c == nil {
		return server.WithWitness(nil)
	}
	return server.WithWitness(c)
}

// serveWitnessCheckTimeout bounds how long serve waits at start for the witness's statement before
// it starts without the check.
const serveWitnessCheckTimeout = 15 * time.Second

// witnessHeadGate holds the audit chain of an enrolled install to the head its witness
// countersigned before serve starts, with the same two checks adopt makes: the entry at LastSeq
// carries LastHead, and the tree of the first Size entries has root Root. A chain that misses
// either was cut back or rewritten under what the witness saw, a removed adoption record among
// it, so the start is refused naming the witness, the coordinate, and the restore remedy, before
// anything is written. A statement that does not verify, or that this install's enrollment does
// not accept, refuses the start too.
//
// A witness that does not answer with a statement is a warning and the start goes on: pushes queue
// and the witness judges them when it answers, and refusing every restart during a witness outage
// would take the product down. A witness that holds nothing for the install yet bounds nothing. An
// install that is not enrolled, or has no producer identity, is not checked here: serveWitnessClient
// settles both.
func witnessHeadGate(ctx context.Context, flagPath, licensePath string, producer *audit.Identity,
	audits audit.Store, log *zap.Logger) error {
	enr, path, err := serveEnrollment(flagPath, licensePath)
	if err != nil || enr == nil || producer == nil {
		return err
	}
	if err := enr.Bind(*producer); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, serveWitnessCheckTimeout)
	defer cancel()
	st, err := witnessclient.FetchStatement(fetchCtx, nil, enr)
	switch {
	case errors.Is(err, witnessclient.ErrNothingWitnessed):
		log.Info("the witness has witnessed nothing for this install yet, so it bounds nothing at "+
			"start", zap.String("witness", enr.Witness))
		return nil
	case errors.Is(err, witnessclient.ErrWitnessKey), errors.Is(err, witnessclient.ErrEnrollment):
		return fmt.Errorf("the witness at %s answered with a statement this install's enrollment "+
			"does not accept: %w. If the witness rotated its key, run switchtender witness enroll "+
			"--refresh-keys, then start again. Nothing was changed", enr.Witness, err)
	case err != nil:
		log.Warn("the witness did not answer with its signed statement, so the chain is not held "+
			"to its head at this start. Pushes queue and the witness judges them when it answers: "+
			err.Error(), zap.String("witness", enr.Witness))
		return nil
	}
	problems, err := witnessHeadProblems(ctx, audits, *producer, witnessAnchors(st, enr.Witness))
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: the audit chain does not reach the head the witness at %s "+
			"countersigned at %s, entry %d with link %s and a tree of %d entries with root %s: %s. "+
			"Entries the witness saw are gone or rewritten, so the start is refused. %s. Nothing "+
			"was changed", binding.ErrDrift, enr.Witness, st.CheckpointAt, st.LastSeq, st.LastHead,
			st.Size, st.Root, joinPrintable(problems), restoreAdvice)
	}
	log.Info("the audit chain reaches the head the witness countersigned",
		zap.String("witness", enr.Witness), zap.Int64("seq", st.LastSeq), zap.Int64("size", st.Size))
	return nil
}

// witnessHeadProblems walks the whole chain of audits once and returns, for each of anchors the
// chain does not reach, why.
func witnessHeadProblems(ctx context.Context, audits audit.Store, id audit.Identity,
	anchors []*audit.Anchor) ([]string, error) {
	seen := audit.NewAnchorScanner(anchors, id)
	if err := audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		seen.Feed(e)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the audit chain: %w", err)
	}
	var problems []string
	if ok, results := seen.Results(); !ok {
		for _, r := range results {
			if !r.Reached {
				problems = append(problems, r.Problem)
			}
		}
	}
	return problems, nil
}
