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
	// Findings the webhook has not accepted yet. The plain witness has no durable record behind
	// it, so an alert lost to one failed POST would be an alert lost for good; it is retried each
	// poll until the channel takes it.
	var undelivered []witness.Finding
	for {
		_, findings, err := watcher.CheckOnce(cmd.Context())
		if witnessOnce {
			printFindings(findings)
			for _, f := range findings {
				// Once mode has no retry queue behind it, so a lost POST is lost for good. It cannot
				// be silent: the cron that runs --once triages by the channel, and a discarded error
				// would leave the finding in the channel's silence. Report it on stderr so the run's
				// output names the delivery failure even though it cannot retry it.
				if derr := postWitnessFinding(cmd.Context(), client, witnessWebhook, watcher.Server(), f); derr != nil {
					fmt.Fprintf(os.Stderr, "witness: webhook delivery failed for %s finding: %v\n", f.Kind, derr)
				}
			}
			if err != nil {
				return err
			}
			if len(findings) > 0 {
				return fmt.Errorf("%d finding(s); the record disagrees with this witness's memory", len(findings))
			}
			return nil
		}
		if err == nil || len(findings) > 0 {
			// A poll that produced findings observed the chain, even when saving the checkpoint
			// then failed, so it is folded rather than re-reported every poll.
			findings = delta.Fresh(findings)
		}
		printFindings(findings)
		undelivered = append(undelivered, findings...)
		if err != nil {
			// A witness that cannot check is not a witness. Logging to a stream nobody reads and
			// looping forever is the failure mode where the process is up, the operator believes
			// they are covered, and a truncation goes unobserved, so the channel hears about it.
			fmt.Fprintln(os.Stderr, "witness: "+err.Error())
		}
		if f := edge.Observe(err); f != nil {
			undelivered = append(undelivered, *f)
		}
		undelivered = deliverFindings(cmd.Context(), client, watcher.Server(), undelivered)
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(witnessInterval):
		}
	}
}

// undeliveredCap bounds the retry queue. Past it the oldest alerts are dropped with a loud line,
// because a webhook that has been down for hundreds of findings is an operator problem the queue
// cannot fix by growing forever.
const undeliveredCap = 100

// printFindings writes each finding to standard error.
func printFindings(findings []witness.Finding) {
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "witness: FINDING %s: %s\n", f.Kind, f.Detail)
	}
}

// deliverFindings posts queued findings to the webhook in order, returning what still awaits
// delivery. The first failure stops the round so ordering holds, and without a webhook there is
// nothing to wait for.
func deliverFindings(ctx context.Context, client *http.Client, server string,
	queue []witness.Finding) []witness.Finding {
	if witnessWebhook == "" || len(queue) == 0 {
		return nil
	}
	for i, f := range queue {
		if err := postWitnessFinding(ctx, client, witnessWebhook, server, f); err != nil {
			fmt.Fprintf(os.Stderr, "witness: webhook: %v; %d finding(s) queued for redelivery\n",
				err, len(queue)-i)
			rest := queue[i:]
			if len(rest) > undeliveredCap {
				fmt.Fprintf(os.Stderr, "witness: webhook: dropping %d oldest undelivered finding(s)\n",
					len(rest)-undeliveredCap)
				rest = rest[len(rest)-undeliveredCap:]
			}
			return rest
		}
	}
	return nil
}

// postWitnessFinding posts one finding to the operator's channel. A transport failure or a
// non-2xx answer is returned, because a delivery the channel refused is a delivery that did not
// happen, whatever the transport says.
func postWitnessFinding(ctx context.Context, client *http.Client, webhook, server string, f witness.Finding) error {
	if webhook == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]string{
		"server": server, "kind": f.Kind, "detail": f.Detail,
		"at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the webhook answered %s", res.Status)
	}
	return nil
}
