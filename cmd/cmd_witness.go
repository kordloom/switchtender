package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/witness"
)

var (
	// witnessServer is the watched server's base URL.
	witnessServer string
	// witnessState is where the signed checkpoint lives.
	witnessState string
	// witnessInterval is how often the loop polls.
	witnessInterval time.Duration
	// witnessOnce runs one check and exits, for cron.
	witnessOnce bool
	// witnessKeyDir overrides where the witness signing identity lives.
	witnessKeyDir string
	// witnessWebhook, when set, receives each finding as a JSON POST.
	witnessWebhook string
)

// witnessCmd watches a server's span beat feed from outside it.
var witnessCmd = &cobra.Command{
	Use:   "witness",
	Short: "Watch a server's beat feed from outside it and attest to what was seen.",
	Long: `Watch a server's span beat feed from outside it.

A chain proves what it holds was not altered. It cannot prove nothing was removed from the end,
because the process that runs the chain also decides what gets written down. A witness on another
machine remembers what the feed served, keeps that memory in a signed checkpoint file, and raises
a finding when a beat goes missing, an already-witnessed beat comes back rewritten, or the head
regresses. Run it where the server's operator has no hand.

Findings go to standard error and, with --webhook, to your channel as a JSON POST. Exit is
nonzero under --once when the check found problems, so a cron line can page on it.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runWitness,
}

// init registers the witness command and its flags.
func init() {
	witnessCmd.Flags().StringVar(&witnessServer, "server", "",
		"Base URL of the SwitchTender server to watch. Required.")
	witnessCmd.Flags().StringVar(&witnessState, "state", witness.DefaultStatePath(),
		"Signed checkpoint file holding what this witness has seen.")
	witnessCmd.Flags().DurationVar(&witnessInterval, "interval", time.Minute,
		"How often to poll the feed.")
	witnessCmd.Flags().BoolVar(&witnessOnce, "once", false,
		"Run one check and exit nonzero on findings, for cron.")
	witnessCmd.Flags().StringVar(&witnessKeyDir, "key-dir", "",
		"Directory holding the witness signing key. Defaults to the state file's directory.")
	witnessCmd.Flags().StringVar(&witnessWebhook, "webhook", "",
		"URL that receives each finding as a JSON POST.")
	rootCmd.AddCommand(witnessCmd)
}

// runWitness polls the feed until stopped, or once under --once.
func runWitness(cmd *cobra.Command, _ []string) error {
	if witnessServer == "" {
		return fmt.Errorf("--server is required")
	}
	base, err := url.Parse(witnessServer)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return fmt.Errorf("--server must be an http or https URL")
	}
	keyDir := witnessKeyDir
	if keyDir == "" {
		keyDir = filepath.Dir(witnessState)
	}
	id, err := audit.LoadIdentity(keyDir)
	if err != nil {
		return fmt.Errorf("load witness identity: %w", err)
	}
	witnessServer = witness.NormalizeServer(witnessServer)
	fmt.Fprintf(os.Stderr, "witness: watching %s, checkpoint %s, key %s\n",
		witnessServer, witnessState, id.KeyID())

	client := &http.Client{Timeout: 30 * time.Second}
	watcher := witness.NewWatcher(witnessServer, witnessState, id, client)
	// The outage edge pages once when checking starts failing and once when it recovers. A page
	// every minute all night for one unreachable server trains the reader to mute the channel the
	// real finding arrives on.
	var edge witness.BlindEdge
	// In loop mode a persisting condition is reported when it appears, not once per poll; the
	// repeat delivery an operator mutes is the channel the next real finding arrives on.
	var delta witness.Delta
	for {
		findings, err := watcher.CheckOnce(cmd.Context())
		if witnessOnce {
			reportFindings(cmd.Context(), client, watcher.Server(), findings)
			if err != nil {
				return err
			}
			if len(findings) > 0 {
				return fmt.Errorf("%d finding(s); the record disagrees with this witness's memory", len(findings))
			}
			return nil
		}
		if err == nil {
			findings = delta.Fresh(findings)
		}
		reportFindings(cmd.Context(), client, watcher.Server(), findings)
		if err != nil {
			// A witness that cannot check is not a witness. Logging to a stream nobody reads and
			// looping forever is the failure mode where the process is up, the operator believes
			// they are covered, and a truncation goes unobserved, so the channel hears about it.
			fmt.Fprintln(os.Stderr, "witness: "+err.Error())
		}
		if f := edge.Observe(err); f != nil {
			postWitnessFinding(cmd.Context(), client, witnessWebhook, witnessServer, *f)
		}
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(witnessInterval):
		}
	}
}

// reportFindings prints each finding and delivers it to the webhook when one is set.
func reportFindings(ctx context.Context, client *http.Client, server string, findings []witness.Finding) {
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "witness: FINDING %s: %s\n", f.Kind, f.Detail)
		postWitnessFinding(ctx, client, witnessWebhook, server, f)
	}
}

// postWitnessFinding posts one finding to the operator's channel, logging a delivery failure
// rather than losing the finding, which already went to standard error.
func postWitnessFinding(ctx context.Context, client *http.Client, webhook, server string, f witness.Finding) {
	if webhook == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"server": server, "kind": f.Kind, "detail": f.Detail,
		"at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "witness: webhook: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "witness: webhook: "+err.Error())
		return
	}
	_ = res.Body.Close()
}
