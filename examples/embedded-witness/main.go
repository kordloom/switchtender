// Command embedded-witness is a complete third-party witness built from SwitchTender's public
// packages alone: identity for the signing key, witness for the memory and the findings, and,
// through witness, beatfeed for the wire contract. It imports nothing from the product's internals,
// which is the point: anyone can build and run one of these against a server they do not operate,
// and the operator cannot make it forget what it saw.
//
// Run it on a machine the watched server's operator does not control:
//
//	embedded-witness --server https://st.example.com
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/witness"
)

// main parses the flags and runs the watch loop until interrupted.
func main() {
	server := flag.String("server", "", "Base URL of the SwitchTender server to watch. Required.")
	stateDir := flag.String("state", "embedded-witness-state",
		"Directory holding the signed checkpoint and this witness's signing key.")
	interval := flag.Duration("interval", time.Minute, "How often to poll the feed.")
	flag.Parse()
	if *server == "" {
		fmt.Fprintln(os.Stderr, "usage: embedded-witness --server https://st.example.com")
		os.Exit(2)
	}
	if err := run(*server, *stateDir, *interval); err != nil {
		fmt.Fprintln(os.Stderr, "embedded-witness: "+err.Error())
		os.Exit(1)
	}
}

// run polls the feed on the interval, remembering what it saw in a signed checkpoint and printing
// every fresh finding. The checkpoint's signer is pinned to this witness's own key, so a replaced
// state file is refused rather than believed.
func run(server, stateDir string, interval time.Duration) error {
	// The identity is created on first use and never leaves the state directory. Publishing its
	// key id is what lets a relying party pin this witness rather than trust whoever answers.
	id, err := identity.Load(stateDir)
	if err != nil {
		return fmt.Errorf("load witness identity: %w", err)
	}
	statePath := filepath.Join(stateDir, witness.StateFileName(server))
	watcher := witness.NewWatcher(server, statePath, id, nil)
	fmt.Fprintf(os.Stderr, "watching %s, checkpoint %s, key %s\n",
		watcher.Server(), statePath, id.KeyID())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The edge reports going blind once and seeing again once, instead of paging every poll while
	// a server is unreachable. The delta reports a standing condition when it appears and again
	// when it changes, not once per interval.
	var edge witness.BlindEdge
	var delta witness.Delta
	for {
		_, findings, err := watcher.CheckOnce(ctx)
		if err == nil || len(findings) > 0 {
			// A poll that produced findings observed the chain, even when saving the checkpoint
			// then failed, so it is folded rather than re-reported every poll.
			findings = delta.Fresh(findings)
		}
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "FINDING %s: %s\n", f.Kind, f.Detail)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "check failed: "+err.Error())
		}
		if f := edge.Observe(err); f != nil {
			fmt.Fprintf(os.Stderr, "FINDING %s: %s\n", f.Kind, f.Detail)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
