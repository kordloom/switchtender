// The supertest deploys SwitchTender the way a customer would and believes nothing it cannot see
// from outside.
//
// It builds the product image from the working tree, brings up a fresh Kind cluster, installs the
// Helm chart twice, and drives real work through the HTTP API: the Community tier on SQLite runs a
// playbook across three real SSH machines, and the Team tier on PostgreSQL walks the whole
// governed arc, from a destructive playbook graded by its own text, through a hold nobody may
// release on their own request, to an execution whose effects and evidence are both checked
// without trusting the server that produced them. Files are read back over kubectl exec, and
// receipts are verified by a local binary that never spoke to the cluster.
//
// The package stands alone on purpose: standard library only, no imports from the rest of the
// module, state confined to one working directory and one named cluster that teardown removes.
// Run it from the repository root:
//
//	go run ./test/supertest -license path/to/team-license.json
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	os.Exit(run())
}

// run executes every phase, renders the report, and returns the process exit code, so main stays
// a single os.Exit and deferred teardown always runs.
func run() int {
	cluster := flag.String("cluster", "supertest", "Kind cluster name.")
	license := flag.String("license", os.Getenv("SUPERTEST_LICENSE_FILE"),
		"Team license file for the paid-tier phase. Also read from SUPERTEST_LICENSE_FILE.")
	skipTeam := flag.Bool("skip-team", false,
		"Run only the Community phases. Without a license this is the honest mode, and it says so.")
	prevImage := flag.String("prev-image", "",
		"Previous release image the upgrade phase starts from. Empty resolves the newest v* tag.")
	shots := flag.String("shots", "", "Directory to write UI screenshots into. Empty skips them.")
	report := flag.String("report", "", "File to append the markdown report to. Empty writes stdout only.")
	keep := flag.Bool("keep", false, "Leave the cluster running afterward, for poking at a failure.")
	flag.Parse()

	repo, err := repositoryRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	work, err := os.MkdirTemp("", "switchtender-supertest-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	h := &harness{
		repo:       repo,
		work:       work,
		cluster:    *cluster,
		kubeconfig: filepath.Join(work, "kubeconfig"),
		bin:        filepath.Join(work, "switchtender"),
		ids:        map[string]string{},
		httpc:      &http.Client{Timeout: 60 * time.Second},
		keep:       *keep,
		skipTeam:   *skipTeam,
	}
	defer h.teardown()

	if !*skipTeam && *license == "" {
		fmt.Fprintln(os.Stderr, "no Team license: pass -license or set SUPERTEST_LICENSE_FILE, "+
			"or run with -skip-team to test the Community tier alone")
		return 1
	}

	previous, perr := previousReleaseImage(*prevImage, repo)
	if perr != nil {
		fmt.Fprintln(os.Stderr, perr)
		return 1
	}

	phases := []struct {
		// Name labels the phase in output and in the report.
		Name string
		// Run is the phase itself. An error here is a harness failure that stops the run; a
		// product failure is recorded in the ledger and the run continues.
		Run func() error
	}{
		{"cluster", h.phaseCluster},
		{"fleet", h.phaseFleet},
		{"community", h.phaseCommunity},
		// Immediately after the run that just crossed three real machines, because the properties
		// here are about what an install discloses of the work it has done and they hold over an
		// empty set otherwise. Later phases upgrade, roll back and fail over the install, and the
		// one this ends up pointed at after all that holds no runs at all, so placing this last
		// traded away the only state it needs. The two accounts and the one grant it leaves behind
		// are inert, and every phase after it runs with grants present, which is closer to a real
		// install than without.
		{"invariants", h.phaseInvariants},
		{"upgrade", func() error { return h.phaseUpgrade(previous) }},
		{"rollback", func() error { return h.phaseRollback(previous) }},
		{"dr", h.phaseDR},
	}
	if !*skipTeam {
		phases = append(phases, struct {
			Name string
			Run  func() error
		}{"team", func() error { return h.phaseTeam(*license) }})
		phases = append(phases, struct {
			Name string
			Run  func() error
		}{"crash", h.phaseCrash})
		phases = append(phases, struct {
			Name string
			Run  func() error
		}{"upgrade-team", func() error { return h.phaseUpgradeTeam(previous, *license) }})
		phases = append(phases, struct {
			Name string
			Run  func() error
		}{"ha", func() error { return h.phaseHA(*license) }})
	}
	if *shots != "" {
		phases = append(phases, struct {
			Name string
			Run  func() error
		}{"screenshots", func() error { return h.phaseShots(*shots) }})
	}

	for i, phase := range phases {
		fmt.Printf("\n== %s\n", phase.Name)
		if err := phase.Run(); err != nil {
			h.fail(phase.Name, "the phase itself completed", err)
			// The phases behind this one are recorded as not reached, never silently absent: a
			// ledger listing only what ran reads as though everything ran, and the crash
			// coverage once vanished exactly that way.
			for _, skipped := range phases[i+1:] {
				h.fail(skipped.Name, "the phase ran",
					fmt.Errorf("not reached: %s failed before it", phase.Name))
			}
			break
		}
	}

	rendered := h.renderReport()
	fmt.Println("\n" + rendered)
	if *report != "" {
		f, err := os.OpenFile(*report, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer f.Close()
		if _, err := f.WriteString(rendered + "\n"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if n := h.failed(); n > 0 {
		fmt.Fprintf(os.Stderr, "\n%d of %d checks failed\n", n, len(h.checks))
		return 1
	}
	fmt.Printf("\nall %d checks held\n", len(h.checks))
	return 0
}

// repositoryRoot requires the supertest to run from the repository root, where the chart and the
// Dockerfile it exists to exercise live. Guessing at a root from somewhere else would test files
// other than the ones in front of the person running it.
func repositoryRoot() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for _, must := range []string{"Dockerfile", "deploy/helm/switchtender", "go.mod"} {
		if _, err := os.Stat(filepath.Join(root, must)); err != nil {
			return "", fmt.Errorf("run from the repository root: %s not found under %s", must, root)
		}
	}
	return root, nil
}

// teardown ends the port-forward and deletes the cluster, unless the run asked to keep it.
func (h *harness) teardown() {
	h.stopForward()
	if h.keep {
		fmt.Printf("\nkeeping cluster %q; delete it with: kind delete cluster --name %s\n",
			h.cluster, h.cluster)
		return
	}
	// Only a cluster this run created is this run's to delete. A run that failed before creating
	// one, or that found the name already taken, must not reach for whatever cluster is standing
	// there: that is a kept debugging cluster or a concurrent run's live one.
	if h.created {
		if out, err := h.run("kind", "delete", "cluster", "--name", h.cluster); err != nil {
			fmt.Fprintf(os.Stderr, "delete cluster: %v\n%s\n", err, out)
		}
	} else {
		fmt.Printf("\ncluster %q was not created by this run; leaving it alone\n", h.cluster)
	}
	_ = os.RemoveAll(h.work)
}

// renderReport renders the ledger as one markdown table, which is what lands in the CI job
// summary: every claim, its verdict, and the observed evidence, with nothing summarized away.
func (h *harness) renderReport() string {
	var b strings.Builder
	b.WriteString("## Supertest report\n\n")
	// The header states what ran, not what usually runs: a -skip-team report claiming both tiers
	// would be the harness doing the one thing it exists to forbid, asserting more than it saw.
	tiers := "both install tiers"
	if h.skipTeam {
		tiers = "the Community tier only (-skip-team: the Team tier was NOT exercised)"
	}
	b.WriteString("A fresh Kind cluster, " + tiers + ", three real SSH machines, and no ")
	b.WriteString("assertion that trusts the product's own word for what happened.\n\n")
	b.WriteString("| | Phase | Claim | Evidence |\n|---|---|---|---|\n")
	for _, c := range h.checks {
		verdict, detail := "✅", c.Detail
		if c.Err != nil {
			verdict, detail = "❌", oneLine(c.Err.Error())
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			verdict, c.Phase, c.Name, strings.ReplaceAll(detail, "|", "\\|"))
	}
	fmt.Fprintf(&b, "\n**%d checks, %d failed.**\n", len(h.checks), h.failed())
	// The table squeezes every failure to one line; the forensics a failure carries, an event
	// tail or a log tail, live below it in full. A summary whose details were amputated made
	// somebody rerun a thirteen-minute suite to read a sentence the run had already captured.
	if h.failed() > 0 {
		b.WriteString("\n### Failures in full\n")
		for _, c := range h.checks {
			if c.Err == nil {
				continue
			}
			fmt.Fprintf(&b, "\n**%s: %s**\n\n```\n%s\n```\n", c.Phase, c.Name, c.Err.Error())
		}
	}
	return b.String()
}
