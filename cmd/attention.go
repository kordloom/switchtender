package cmd

import (
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/dispatch"
)

// serveAttentionFile holds the value of the --attention-file flag.
var serveAttentionFile string

// init registers the serve flag that names the attention thresholds file.
func init() {
	serveCmd.Flags().StringVar(&serveAttentionFile, "attention-file", "",
		"YAML file setting when work waiting on the dashboard's Needs attention view is shown as "+
			"blocked and when it alerts, as organization defaults with per-organization, per-queue, "+
			"and per-template overrides. Empty uses the built-in thresholds. A malformed file stops "+
			"the server rather than alerting on defaults nobody chose.")
}

// loadAttentionConfig reads the thresholds file, or returns nil for the built-in thresholds when no
// file is named.
func loadAttentionConfig(log *zap.Logger) (*attention.Config, error) {
	if serveAttentionFile == "" {
		return nil, nil
	}
	cfg, err := attention.LoadConfig(serveAttentionFile)
	if err != nil {
		return nil, err
	}
	log.Info("attention thresholds read from file", zap.String("path", serveAttentionFile))
	return cfg, nil
}

// attentionSource builds what the dashboard, the doctor, and the alert monitor read: the run and
// schedule stores, the accounts behind runs, the workers' reports, and the lease timing the
// dispatcher runs with.
func attentionSource(bundle storeBundle, cfg *attention.Config) *attention.Source {
	return &attention.Source{
		Runs: bundle.Runs(), Presence: bundle.Attention(), Schedules: bundle.Schedules(),
		Users: bundle.Users(), Config: cfg,
		Timing: attention.DefaultTiming(dispatch.LeaseTTL, dispatch.HeartbeatInterval,
			dispatch.SweepInterval, dispatch.PresenceInterval),
	}
}
