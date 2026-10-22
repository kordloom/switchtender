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
	path := flagPath
	if path == "" {
		path = witnessclient.PathFor(licensePath)
		enr, err := witnessclient.LoadIfPresent(path)
		if err != nil {
			return nil, fmt.Errorf("%w. Fix or remove it to start without a witness", err)
		}
		if enr == nil {
			return nil, nil
		}
	} else if _, err := witnessclient.Load(path); err != nil {
		return nil, fmt.Errorf("--witness-enrollment: %w", err)
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
