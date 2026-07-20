package server

import (
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
)

// metricsHandler serves run and fleet gauges in the Prometheus text exposition format, computed
// from the store at scrape time so no counter state lives in the process.
func metricsHandler(store run.Store, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: metricsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		runs, err := store.List(r.Context())
		if err != nil {
			log.Error("server: metrics: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not compute metrics")
			return
		}
		byStatus := map[run.Status]int{}
		for _, rn := range runs {
			byStatus[rn.Status]++
		}

		var b strings.Builder
		b.WriteString("# HELP switchtender_runs_total Top level runs by status.\n")
		b.WriteString("# TYPE switchtender_runs_total gauge\n")
		for _, status := range []run.Status{
			run.StatusPending, run.StatusRunning, run.StatusSucceeded,
			run.StatusFailed, run.StatusCanceled, run.StatusInterrupted,
		} {
			fmt.Fprintf(&b, "switchtender_runs_total{status=%q} %d\n", status, byStatus[status])
		}

		if health, err := store.FleetHealth(r.Context(), defaultFleetWindow); err == nil {
			flaky := 0
			failing := 0
			for _, h := range health {
				if h.Flaky {
					flaky++
				}
				if run.FailedOutcome(h.LastOutcome) {
					failing++
				}
			}
			b.WriteString("# HELP switchtender_hosts_total Hosts seen across recent runs.\n")
			b.WriteString("# TYPE switchtender_hosts_total gauge\n")
			fmt.Fprintf(&b, "switchtender_hosts_total %d\n", len(health))
			b.WriteString("# HELP switchtender_hosts_flaky Hosts flipping between failing and passing.\n")
			b.WriteString("# TYPE switchtender_hosts_flaky gauge\n")
			fmt.Fprintf(&b, "switchtender_hosts_flaky %d\n", flaky)
			b.WriteString("# HELP switchtender_hosts_failing Hosts whose latest outcome failed.\n")
			b.WriteString("# TYPE switchtender_hosts_failing gauge\n")
			fmt.Fprintf(&b, "switchtender_hosts_failing %d\n", failing)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	}
}
