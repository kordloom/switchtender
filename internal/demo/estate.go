package demo

import (
	"context"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// seedEstateHistory backdates a few host state readings so the estate page answers something.
//
// The seeded runs all happen now, so without this the estate has exactly one reading per host and
// every window reports every host as newly added: a page whose whole subject is change, showing
// none. A visitor would reasonably conclude the feature does nothing.
//
// The readings are written straight to the store rather than produced by a run, because a run
// gathers facts at the moment it executes and there is no way to run one in the past. They name the
// runs that really did gather, so the run each reading links to is a real one.
func seedEstateHistory(ctx context.Context, d Deps) {
	if d.Runs == nil {
		return
	}
	now := seedTime(d)
	// Readings the demo's own gathers would have taken, had they happened then. The kernel moves
	// and one host leaves the estate, which is what makes a diff worth looking at.
	history := []struct {
		Host   string
		Kernel string
		Ago    time.Duration
	}{
		{Host: "db01", Kernel: "5.14.0-362.8.1.el9_3.x86_64", Ago: 90 * 24 * time.Hour},
		{Host: "db01", Kernel: "5.14.0-427.13.1.el9_4.x86_64", Ago: 30 * 24 * time.Hour},
		{Host: "web01", Kernel: "5.15.0-92-generic", Ago: 90 * 24 * time.Hour},
		{Host: "web01", Kernel: "5.15.0-107-generic", Ago: 30 * 24 * time.Hour},
		// Retired between the two windows, so a diff across it shows a host leaving rather than
		// only hosts arriving.
		{Host: "legacy01", Kernel: "4.18.0-513.5.1.el8_9.x86_64", Ago: 90 * 24 * time.Hour},
	}
	// Every gather is kept rather than collapsed into one per day, so the seeded readings survive
	// as distinct points however close together the demo places them.
	prevInterval, prevDepth := run.FactsInterval(), run.FactsDepth()
	run.SetFactsInterval(0)
	defer func() {
		run.SetFactsInterval(prevInterval)
		run.SetFactsDepth(prevDepth)
	}()

	for _, h := range history {
		at := now.Add(-h.Ago)
		facts := []run.HostFacts{{
			Host:       h.Host,
			GatheredAt: at,
			Facts: map[string]string{
				"kernel":               h.Kernel,
				"distribution":         "Rocky",
				"distribution_version": "9.4",
			},
		}}
		// A synthetic run id, so the reading names where it came from without pretending one of the
		// demo's real runs gathered a host months ago.
		_ = d.Runs.SaveHostFacts(ctx, "run_seed_"+h.Host+at.Format("20060102"), facts)
	}
}
